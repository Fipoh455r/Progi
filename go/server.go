// server.go — HTTP-сервер v2.2: чат + агент + RAG + сжатие + авторизация.
package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

//go:embed static/index.html
var staticFiles embed.FS

const maxUploadSize = 10 << 20 // 10 MB

// runServer запускает HTTP-сервер.
func runServer(ollamaURL, defaultModel, port, dataDir string) {
	// ── Балансировщик нод Ollama ─────────────────────────────────────────
	balancer := NewOllamaBalancer(ollamaURL)
	client := balancer.Primary() // для RAG, compress и прочих одиночных вызовов

	store, err := NewStorage(dataDir)
	if err != nil {
		log.Fatalf("Хранилище: %v", err)
	}

	rag, err := NewRAG(dataDir+"/rag", client, "nomic-embed-text")
	if err != nil {
		log.Fatalf("RAG: %v", err)
	}

	// ── Метрики ──────────────────────────────────────────────────────────
	metrics := NewMetrics(balancer)

	// ── Авторизация ──────────────────────────────────────────────────────
	userStore, err := NewUserStore(dataDir)
	if err != nil {
		log.Fatalf("UserStore: %v", err)
	}

	jwtSecret, err := loadOrCreateSecret(dataDir)
	if err != nil {
		log.Fatalf("JWT секрет: %v", err)
	}

	// Rate limiter: 60 запросов/мин для API, строже для логина
	apiLimiter   := NewRateLimiter(rateLimitMax, rateLimitWindow)
	loginLimiter := NewRateLimiter(10, rateLimitWindow) // 10 попыток/мин

	authMw  := RequireAuth(jwtSecret)
	adminMw := RequireAdmin(jwtSecret)

	// ── Голос: STT (Whisper) + TTS (piper) ──────────────────────────────
	whisperURL := envOr("LOCALAI_WHISPER_URL", "http://localhost:8081")
	whisperClient := NewWhisperClient(whisperURL)

	piperBin      := os.Getenv("LOCALAI_PIPER_BIN")
	piperVoicesDir := envOr("LOCALAI_PIPER_VOICES_DIR", dataDir+"/voices")
	piperVoice    := envOr("LOCALAI_PIPER_VOICE", "en_US-lessac-medium")
	piperTTS := NewPiperTTS(piperBin, piperVoicesDir, piperVoice)

	if balancer.HealthyCount() == 0 {
		fmt.Printf("%s[!] Ollama недоступен (%s). Запусти Docker.%s\n", colorYellow, ollamaURL, colorReset)
	} else if balancer.TotalCount() > 1 {
		fmt.Printf("%s[✓] Ollama балансировщик: %d/%d нод доступны%s\n",
			colorGreen, balancer.HealthyCount(), balancer.TotalCount(), colorReset)
	}
	if !whisperClient.IsAvailable() {
		fmt.Printf("%s[!] Whisper STT недоступен (%s). Голосовой ввод отключён.%s\n",
			colorYellow, whisperURL, colorReset)
	}
	if !piperTTS.IsAvailable() {
		fmt.Printf("%s[!] piper-tts не найден. Голосовой вывод отключён.%s\n", colorYellow, colorReset)
	}

	if userStore.Count() == 0 {
		fmt.Printf("%s[!] Нет пользователей. Открой http://localhost:%s и создай admin через /api/auth/setup%s\n",
			colorYellow, port, colorReset)
	}

	mux := http.NewServeMux()
	registerMetricsRoute(mux, metrics)

	// ── Вспомогательная обёртка для защищённых маршрутов ────────────────
	// protected применяет middleware авторизации и rate limiting
	protected := func(h http.HandlerFunc) http.Handler {
		return apiLimiter.Middleware(authMw(h))
	}
	adminOnly := func(h http.HandlerFunc) http.Handler {
		return apiLimiter.Middleware(adminMw(h))
	}

	// ── Главная страница (публичная) ─────────────────────────────────────
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, _ := staticFiles.ReadFile("static/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	// ── Авторизация (публичные маршруты) ─────────────────────────────────
	registerAuthRoutes(mux, userStore, jwtSecret, loginLimiter, metrics)

	// ── Health (публичный) ───────────────────────────────────────────────
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		ok := client.IsAvailable()
		if !ok {
			w.WriteHeader(503)
		}
		w.Header().Set("Content-Type", "application/json")
		docCount := len(rag.ListDocs())
		fmt.Fprintf(w, `{"ollama":%v,"docs":%d}`, ok, docCount)
	})

	// ── Модели ──────────────────────────────────────────────────────────
	mux.Handle("/api/models", protected(func(w http.ResponseWriter, r *http.Request) {
		models, err := client.ListModels()
		if err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		jsonOK(w, models)
	}))

	// ── Инструменты агента ───────────────────────────────────────────────
	mux.Handle("/api/tools", protected(func(w http.ResponseWriter, r *http.Request) {
		type toolInfo struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			ArgsSchema  string `json:"args_schema"`
		}
		var tools []toolInfo
		for _, t := range AllTools {
			tools = append(tools, toolInfo{t.Name, t.Description, t.ArgsSchema})
		}
		jsonOK(w, tools)
	}))

	// ── Сессии ──────────────────────────────────────────────────────────
	mux.Handle("/api/sessions", protected(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		list, _ := store.List()
		if list == nil {
			list = []SessionMeta{}
		}
		jsonOK(w, list)
	}))

	mux.Handle("/api/sessions/", protected(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/")
		if id == "" {
			http.Error(w, "missing id", 400)
			return
		}
		switch r.Method {
		case http.MethodGet:
			sess, _ := store.Load(id)
			if sess == nil {
				http.NotFound(w, r)
				return
			}
			jsonOK(w, sess)
		case http.MethodPatch:
			sess, _ := store.Load(id)
			if sess == nil {
				http.Error(w, "not found", 404)
				return
			}
			var p struct {
				Title        *string  `json:"title"`
				Model        *string  `json:"model"`
				Temperature  *float64 `json:"temperature"`
				SystemPrompt *string  `json:"system_prompt"`
			}
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				http.Error(w, "bad request", 400)
				return
			}
			if p.Title != nil {
				sess.Title = *p.Title
			}
			if p.Model != nil {
				sess.Settings.Model = *p.Model
			}
			if p.Temperature != nil {
				sess.Settings.Temperature = *p.Temperature
			}
			if p.SystemPrompt != nil {
				sess.Settings.SystemPrompt = *p.SystemPrompt
				for i, m := range sess.Messages {
					if m.Role == "system" {
						sess.Messages[i].Content = *p.SystemPrompt
						break
					}
				}
			}
			_ = store.Save(sess)
			jsonOK(w, sess.SessionMeta)
		case http.MethodDelete:
			_ = store.Delete(id)
			jsonOK(w, map[string]bool{"ok": true})
		default:
			http.Error(w, "method not allowed", 405)
		}
	}))

	// ── Обычный чат (SSE) ────────────────────────────────────────────────
	// POST /api/chat  {message, model, session_id, temperature, use_rag}
	mux.Handle("/api/chat", protected(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var body struct {
			Message   string  `json:"message"`
			Model     string  `json:"model"`
			SessionID string  `json:"session_id"`
			Temp      float64 `json:"temperature"`
			UseRAG    bool    `json:"use_rag"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Message) == "" {
			http.Error(w, "bad request", 400)
			return
		}
		if body.SessionID == "" {
			body.SessionID = "default"
		}

		sess, err := store.GetOrCreate(body.SessionID, defaultModel, "")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		model := firstNonEmpty(body.Model, sess.Settings.Model, defaultModel)
		temp := body.Temp
		if temp == 0 {
			temp = sess.Settings.Temperature
		}
		if temp == 0 {
			temp = 0.7
		}

		// Сохраняем сообщение пользователя
		_ = store.AppendAndSave(sess, Message{Role: "user", Content: body.Message})

		// Сжимаем историю если слишком длинная (экономия токенов)
		ctx := r.Context()
		compressed, wasCompressed, _ := CompressHistory(ctx, client, sess.Messages, model)
		if wasCompressed {
			sess.Messages = compressed
			_ = store.Save(sess)
		}

		// RAG: инжектируем контекст из документов
		msgs := sess.Messages
		if body.UseRAG || len(rag.ListDocs()) > 0 {
			results, err := rag.Search(ctx, body.Message, 4)
			if err == nil && len(results) > 0 {
				ragCtx := BuildContextString(results)
				if ragCtx != "" {
					// Вставляем контекст как system-сообщение перед последним user
					injected := make([]Message, len(msgs))
					copy(injected, msgs)
					last := injected[len(injected)-1]
					injected[len(injected)-1] = Message{
						Role:    "system",
						Content: ragCtx,
					}
					injected = append(injected, last)
					msgs = injected
				}
			}
		}

		// SSE-заголовки
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		flusher := w.(http.Flusher)

		writeEv := func(v any) {
			data, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}

		// Метрики: считаем запрос и активное соединение
		metrics.ChatReqs.Inc()
		metrics.ActiveStart()
		defer metrics.ActiveDone()
		t0 := time.Now()

		// Балансировщик: выбираем здоровую ноду для этого запроса
		chatClient := balancer.Pick()
		tokenCh, errCh := chatClient.ChatStreamWithTemp(ctx, msgs, model, temp)
		var sb strings.Builder
		for token := range tokenCh {
			sb.WriteString(token)
			writeEv(map[string]string{"token": token})
		}

		if err := <-errCh; err != nil {
			metrics.ChatErrs.Inc()
			balancer.ReportFailure(chatClient)
			if ctx.Err() == nil {
				writeEv(map[string]string{"error": err.Error()})
				// Откат последнего user-сообщения
				if len(sess.Messages) > 0 {
					sess.Messages = sess.Messages[:len(sess.Messages)-1]
					_ = store.Save(sess)
				}
			}
			return
		}

		balancer.ReportSuccess(chatClient)
		metrics.ChatDuration.Observe(time.Since(t0).Seconds())
		if resp := sb.String(); resp != "" {
			// Примерно считаем токены: 1 токен ≈ 4 символа
			metrics.Tokens.Add(int64(len([]rune(resp)) / 4))
			_ = store.AppendAndSave(sess, Message{Role: "assistant", Content: resp})
		}
		writeEv(map[string]bool{"done": true})
	}))

	// ── Агент (SSE с шагами) ─────────────────────────────────────────────
	// POST /api/agent  {message, model, session_id, temperature}
	mux.Handle("/api/agent", protected(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var body struct {
			Message   string  `json:"message"`
			Model     string  `json:"model"`
			SessionID string  `json:"session_id"`
			Temp      float64 `json:"temperature"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Message) == "" {
			http.Error(w, "bad request", 400)
			return
		}
		if body.SessionID == "" {
			body.SessionID = "agent_" + body.SessionID
		}

		sess, err := store.GetOrCreate(body.SessionID, defaultModel, agentSystemPrompt())
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		model := firstNonEmpty(body.Model, sess.Settings.Model, defaultModel)
		temp := body.Temp
		if temp == 0 {
			temp = 0.4 // агент работает точнее при низкой температуре
		}

		// Добавляем вопрос в историю
		_ = store.AppendAndSave(sess, Message{Role: "user", Content: body.Message})

		// SSE-заголовки
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		flusher := w.(http.Flusher)

		writeStep := func(step AgentStep) {
			data, _ := json.Marshal(step)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}

		ctx := r.Context()
		stepCh := make(chan AgentStep, 16)

		// Метрики
		metrics.AgentReqs.Inc()
		metrics.ActiveStart()
		defer metrics.ActiveDone()
		t0Agent := time.Now()

		// Запускаем агента с выбранной нодой
		agentClient := balancer.Pick()
		answerCh := make(chan string, 1)
		go func() {
			answer, agentErr := RunAgent(ctx, agentClient, sess.Messages, model, temp, stepCh)
			if agentErr != nil {
				metrics.AgentErrs.Inc()
				balancer.ReportFailure(agentClient)
			} else {
				balancer.ReportSuccess(agentClient)
				metrics.AgentDuration.Observe(time.Since(t0Agent).Seconds())
			}
			answerCh <- answer
		}()

		// Стримим шаги
		for step := range stepCh {
			writeStep(step)
		}

		// Сохраняем финальный ответ
		if answer := <-answerCh; answer != "" {
			_ = store.AppendAndSave(sess, Message{Role: "assistant", Content: answer})
		}

		data, _ := json.Marshal(map[string]bool{"done": true})
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}))

	// ── RAG: загрузка документа ──────────────────────────────────────────
	// POST /api/upload  (multipart/form-data, поле "file")
	mux.Handle("/api/upload", protected(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
		if err := r.ParseMultipartForm(maxUploadSize); err != nil {
			http.Error(w, "файл слишком большой (макс 10MB)", 413)
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "поле 'file' не найдено", 400)
			return
		}
		defer file.Close()

		content, err := io.ReadAll(io.LimitReader(file, maxUploadSize))
		if err != nil {
			http.Error(w, "ошибка чтения файла", 500)
			return
		}

		// Определяем тип и извлекаем текст
		text := extractText(content, header.Filename)
		if strings.TrimSpace(text) == "" {
			http.Error(w, "файл пустой или неподдерживаемый формат", 422)
			return
		}

		ctx := r.Context()
		n, err := rag.AddDocument(ctx, header.Filename, text)
		if err != nil {
			metrics.UploadErrs.Inc()
			http.Error(w, "ошибка индексации: "+err.Error(), 500)
			return
		}

		metrics.Uploads.Inc()
		jsonOK(w, map[string]any{
			"name":   header.Filename,
			"chunks": n,
			"size":   len(content),
		})
	}))

	// ── RAG: список и удаление документов ───────────────────────────────
	mux.Handle("/api/docs", protected(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			docs := rag.ListDocs()
			if docs == nil {
				docs = []DocMeta{}
			}
			jsonOK(w, docs)
		default:
			http.Error(w, "method not allowed", 405)
		}
	}))

	mux.Handle("/api/docs/", protected(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/docs/"), "/")
		if id == "" {
			http.Error(w, "missing doc id", 400)
			return
		}
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", 405)
			return
		}
		if err := rag.DeleteDoc(id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		jsonOK(w, map[string]bool{"ok": true})
	}))

	// ── Очистить историю ─────────────────────────────────────────────────
	mux.Handle("/api/clear", protected(func(w http.ResponseWriter, r *http.Request) {
		sid := r.URL.Query().Get("session")
		if sid == "" {
			sid = "default"
		}
		sess, _ := store.Load(sid)
		if sess != nil {
			prompt := firstNonEmpty(sess.Settings.SystemPrompt, systemPrompt)
			sess.Messages = []Message{{Role: "system", Content: prompt}}
			sess.Title = "Новый чат"
			_ = store.Save(sess)
		}
		jsonOK(w, map[string]bool{"ok": true})
	}))

	// ── Пользователи (admin only) ────────────────────────────────────────
	mux.Handle("/api/auth/users", adminOnly(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			users, err := userStore.List()
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			jsonOK(w, users)
		default:
			http.Error(w, "method not allowed", 405)
		}
	}))

	mux.Handle("/api/auth/users/", adminOnly(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/auth/users/"), "/")
		if id == "" {
			http.Error(w, "missing id", 400)
			return
		}
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", 405)
			return
		}
		// Нельзя удалить самого себя
		claims := contextUser(r.Context())
		if claims != nil {
			self, _ := userStore.GetByUsername(claims.Sub)
			if self != nil && self.ID == id {
				http.Error(w, "нельзя удалить собственный аккаунт", 400)
				return
			}
		}
		if err := userStore.Delete(id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		jsonOK(w, map[string]bool{"ok": true})
	}))

	registerOpenAIRoutes(mux, client, defaultModel)
	registerAudioRoutes(mux, whisperClient, piperTTS, jwtSecret)

	addr := ":" + port
	fmt.Printf("%s[✓] LocalAI v3.0 запущен%s\n", colorGreen, colorReset)
	fmt.Printf("    Веб:    %shttp://localhost:%s%s\n", colorCyan, port, colorReset)
	fmt.Printf("    Агент:  POST /api/agent\n")
	fmt.Printf("    RAG:    POST /api/upload | GET /api/docs\n")
	fmt.Printf("    OpenAI: %shttp://localhost:%s/v1%s\n", colorCyan, port, colorReset)
	fmt.Printf("    Auth:   POST /api/auth/login | /api/auth/setup\n")
	fmt.Printf("    Аудио:  POST /api/audio/transcriptions | /api/audio/speech\n")
	fmt.Printf("    Метрики: GET /metrics  (Prometheus)\n")
	fmt.Printf("    Ноды:   %d/%d Ollama нод доступны\n", balancer.HealthyCount(), balancer.TotalCount())
	fmt.Printf("    Данные: %s%s%s\n\n", colorGray, dataDir, colorReset)

	if err := http.ListenAndServe(addr, withLogging(mux)); err != nil {
		log.Fatalf("Сервер: %v", err)
	}
}

// registerAuthRoutes регистрирует публичные и аутентифицированные маршруты авторизации.
func registerAuthRoutes(mux *http.ServeMux, users *UserStore, secret []byte, loginLimiter *RateLimiter, m *Metrics) {
	// POST /api/auth/setup — создать первого admin (только если нет пользователей)
	mux.HandleFunc("/api/auth/setup", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		if users.Count() > 0 {
			http.Error(w, `{"error":"уже настроено"}`, http.StatusConflict)
			return
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
			strings.TrimSpace(body.Username) == "" || len(body.Password) < 6 {
			http.Error(w, `{"error":"нужны username и password (мин 6 символов)"}`, 400)
			return
		}
		u, err := users.Create(body.Username, body.Password, RoleAdmin)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		token, err := GenerateJWT(u.Username, u.Role, secret)
		if err != nil {
			http.Error(w, "ошибка создания токена", 500)
			return
		}
		jsonOK(w, map[string]string{
			"token":    token,
			"username": u.Username,
			"role":     u.Role,
		})
	})

	// POST /api/auth/login — вход (возвращает JWT)
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		// Строгий rate limiting для защиты от брутфорса
		if !loginLimiter.Allow(clientIP(r)) {
			http.Error(w, `{"error":"слишком много попыток"}`, http.StatusTooManyRequests)
			return
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
			strings.TrimSpace(body.Username) == "" || body.Password == "" {
			http.Error(w, `{"error":"нужны username и password"}`, 400)
			return
		}
		u, err := users.GetByUsername(body.Username)
		if err != nil || u == nil || !VerifyPassword(u.PasswordHash, body.Password) {
			// Одинаковая ошибка чтобы не раскрывать существование пользователя
			m.LoginFail.Inc()
			http.Error(w, `{"error":"неверный логин или пароль"}`, http.StatusUnauthorized)
			return
		}
		token, err := GenerateJWT(u.Username, u.Role, secret)
		if err != nil {
			http.Error(w, "ошибка создания токена", 500)
			return
		}
		m.LoginOK.Inc()
		jsonOK(w, map[string]string{
			"token":    token,
			"username": u.Username,
			"role":     u.Role,
		})
	})

	// POST /api/auth/register — создать пользователя (только admin, требует Bearer)
	mux.Handle("/api/auth/register", RequireAdmin(secret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Role     string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
			strings.TrimSpace(body.Username) == "" || len(body.Password) < 6 {
			http.Error(w, `{"error":"нужны username и password (мин 6 символов)"}`, 400)
			return
		}
		if body.Role != RoleAdmin && body.Role != RoleUser {
			body.Role = RoleUser
		}
		u, err := users.Create(body.Username, body.Password, body.Role)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		u.PasswordHash = "" // не отдаём хэш
		jsonOK(w, u)
	})))

	// GET /api/auth/me — текущий пользователь (требует Bearer)
	mux.Handle("/api/auth/me", RequireAuth(secret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		claims := contextUser(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		jsonOK(w, map[string]string{
			"username": claims.Sub,
			"role":     claims.Role,
		})
	})))

	// GET /api/auth/status — публичный: нужна ли настройка?
	mux.HandleFunc("/api/auth/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		jsonOK(w, map[string]bool{"needs_setup": users.Count() == 0})
	})
}

// loadOrCreateSecret загружает JWT-секрет из файла или создаёт новый.
// Приоритет: переменная окружения LOCALAI_JWT_SECRET → файл jwt_secret.key → генерация.
func loadOrCreateSecret(dataDir string) ([]byte, error) {
	// 1. Переменная окружения
	if env := os.Getenv("LOCALAI_JWT_SECRET"); env != "" {
		return []byte(env), nil
	}

	// 2. Файл на диске
	keyPath := dataDir + "/jwt_secret.key"
	if data, err := os.ReadFile(keyPath); err == nil && len(data) >= 16 {
		return data, nil
	}

	// 3. Генерируем и сохраняем
	secret, err := GenerateSecret()
	if err != nil {
		return nil, fmt.Errorf("генерация секрета: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, secret, 0o600); err != nil {
		return nil, fmt.Errorf("сохранение секрета: %w", err)
	}
	log.Printf("[auth] JWT-секрет создан: %s", keyPath)
	return secret, nil
}

// ── Вспомогательные ─────────────────────────────────────────────────────────

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		skip := r.URL.Path == "/api/chat" || r.URL.Path == "/api/agent" || r.URL.Path == "/health"
		if !skip {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// extractText извлекает текст из содержимого файла.
// Поддерживает: .txt, .md, .csv, .json, .go, .py, .js, .ts, .html, .yaml, .toml
// PDF не поддерживается без CGO — возвращаем сообщение об ошибке.
func extractText(data []byte, filename string) string {
	lower := strings.ToLower(filename)

	// Бинарные форматы без поддержки
	if strings.HasSuffix(lower, ".pdf") {
		return "[PDF файлы не поддерживаются без дополнительных библиотек. " +
			"Пожалуйста, сконвертируй PDF в TXT или MD перед загрузкой.]"
	}
	if strings.HasSuffix(lower, ".docx") || strings.HasSuffix(lower, ".doc") {
		return "[DOCX/DOC не поддерживаются. Пожалуйста, сохрани документ в TXT или MD.]"
	}

	// Всё остальное — текст
	text := string(data)

	// Базовая очистка HTML-тегов для .html файлов
	if strings.HasSuffix(lower, ".html") || strings.HasSuffix(lower, ".htm") {
		text = stripHTMLTags(text)
	}

	return text
}

// stripHTMLTags удаляет HTML-теги из текста.
func stripHTMLTags(html string) string {
	var sb strings.Builder
	inTag := false
	for _, r := range html {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
			sb.WriteRune(' ')
		case !inTag:
			sb.WriteRune(r)
		}
	}
	// Схлопываем лишние пробелы
	return strings.Join(strings.Fields(sb.String()), " ")
}
