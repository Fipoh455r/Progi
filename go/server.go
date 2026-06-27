// server.go — HTTP-сервер v3.4: чат + агент + RAG + сжатие + авторизация + graceful shutdown
//             + умный контекст (SmartContext) + шаблоны промптов + статистика токенов.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

//go:embed static/index.html
var staticFiles embed.FS

const maxUploadSize = 10 << 20 // 10 MB

// runServer запускает HTTP-сервер.
// cacheEnabled/cacheTTLHours управляют кэшем LLM-ответов.
func runServer(ollamaURL, defaultModel, port, dataDir string, cacheEnabled bool, cacheTTLHours int) {
	// ── Балансировщик нод Ollama ─────────────────────────────────────────
	balancer := NewOllamaBalancer(ollamaURL)
	client := balancer.Primary() // для RAG, compress и прочих одиночных вызовов

	store, err := NewStorage(dataDir)
	if err != nil {
		log.Fatalf("Хранилище: %v", err)
	}

	// Инициализируем директорию памяти для инструмента memory
	SetMemoryDir(dataDir)

	// Инициализируем пул специализированных агентов
	InitAgentPool(dataDir)

	// Инициализируем счётчик статистики токенов
	InitTokenStats(dataDir)

	// Инициализируем кэш ответов LLM (если включён в конфиге)
	if cacheEnabled {
		ttl := time.Duration(cacheTTLHours) * time.Hour
		if err := InitCache(dataDir, ttl); err != nil {
			log.Printf("[!] Кэш LLM недоступен: %v", err)
		}
	}

	// v3.6: Семантический кэш (LOCALAI_SEMANTIC_CACHE=true включает)
	scEnabled := os.Getenv("LOCALAI_SEMANTIC_CACHE") == "true"
	if scEnabled {
		scThreshold := scParseThreshold(os.Getenv("LOCALAI_SEMANTIC_THRESHOLD"), scDefaultThreshold)
		scMaxSize := scParseMaxSize(os.Getenv("LOCALAI_SEMANTIC_CACHE_SIZE"), scDefaultMaxSize)
		if err := InitSemanticCache(dataDir, client, "nomic-embed-text", scThreshold, scMaxSize); err != nil {
			log.Printf("[!] Семантический кэш недоступен: %v", err)
		} else {
			log.Printf("[✓] Семантический кэш: threshold=%.2f, max=%d", scThreshold, scMaxSize)
		}
	}

	// v3.6: Очередь фоновых задач (всегда активна)
	jqWorkers := scParseMaxSize(os.Getenv("LOCALAI_JOB_WORKERS"), jqDefaultWorkers)
	if err := InitJobQueue(dataDir, client, defaultModel, jqWorkers); err != nil {
		log.Printf("[!] Очередь задач недоступна: %v", err)
	}

	// Предоставляем клиент инструментам требующим LLM (agent_call)
	SetToolsClient(client, defaultModel)

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

	// ── Список специализированных агентов ────────────────────────────────
	// GET /api/agents[?tag=code] — все роли или по тегу
	mux.Handle("/api/agents", protected(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		tag := r.URL.Query().Get("tag")
		type roleInfo struct {
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Temperature float64  `json:"temperature"`
			MaxSteps    int      `json:"max_steps"`
			UseTools    bool     `json:"use_tools"`
			Tags        []string `json:"tags"`
		}
		var list []roleInfo
		for _, ro := range AllRoles() {
			if tag != "" {
				found := false
				for _, t := range ro.Tags {
					if t == tag {
						found = true
						break
					}
				}
				if !found {
					continue
				}
			}
			list = append(list, roleInfo{ro.Name, ro.Description, ro.Temperature, ro.MaxSteps, ro.UseTools, ro.Tags})
		}
		if list == nil {
			list = []roleInfo{}
		}
		jsonOK(w, list)
	}))

	// ── Мульти-агентная задача (SSE с прогрессом) ────────────────────────
	// POST /api/multiagent {"task":"...", "model":"..."}
	mux.Handle("/api/multiagent", protected(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var body struct {
			Task  string `json:"task"`
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Task) == "" {
			http.Error(w, "нужен непустой task", 400)
			return
		}
		model := firstNonEmpty(body.Model, defaultModel)

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

		ctx := r.Context()
		progressCh := make(chan OrchestratorEvent, 32)

		metrics.AgentReqs.Inc()
		metrics.ActiveStart()
		defer metrics.ActiveDone()

		resultCh := make(chan string, 1)
		go func() {
			answer, _ := OrchestrateTask(ctx, balancer.Pick(), body.Task, model, progressCh)
			resultCh <- answer
		}()

		for ev := range progressCh {
			writeEv(ev)
		}

		writeEv(map[string]bool{"done": true})
	}))

	// ── Статистика кэша ──────────────────────────────────────────────────
	// GET /api/cache/stats
	mux.Handle("/api/cache/stats", protected(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		if globalCache == nil {
			jsonOK(w, map[string]any{"enabled": false})
			return
		}
		hits, misses, entries := globalCache.Stats()
		total := hits + misses
		hitRate := 0.0
		if total > 0 {
			hitRate = float64(hits) / float64(total) * 100
		}
		jsonOK(w, map[string]any{
			"enabled":  true,
			"hits":     hits,
			"misses":   misses,
			"entries":  entries,
			"hit_rate": fmt.Sprintf("%.1f%%", hitRate),
		})
	}))

	// ── Рой агентов (v3.5) ───────────────────────────────────────────────
	// POST /api/swarm  {"text":"...", "question":"...", "model":"...", "max_agents":100}
	// → SSE: SwarmEvent (kind: start|chunk|merge|done|error)
	//
	// Экономия токенов: задача в 100M токенов → ~50K через параллельный рой.
	mux.Handle("/api/swarm", protected(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var body struct {
			Text      string `json:"text"`
			Question  string `json:"question"`
			Model     string `json:"model"`
			MaxAgents int    `json:"max_agents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
			strings.TrimSpace(body.Text) == "" ||
			strings.TrimSpace(body.Question) == "" {
			http.Error(w, `{"error":"нужны непустые поля text и question"}`, 400)
			return
		}
		model := firstNonEmpty(body.Model, defaultModel)

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

		ctx := r.Context()
		progressCh := make(chan SwarmEvent, 128)

		metrics.AgentReqs.Inc()
		metrics.ActiveStart()
		defer metrics.ActiveDone()

		job := SwarmJob{
			Text:      body.Text,
			Question:  body.Question,
			Model:     model,
			MaxAgents: body.MaxAgents,
		}

		go RunSwarm(ctx, balancer.Pick(), job, progressCh)

		for ev := range progressCh {
			writeEv(ev)
		}
	}))

	// ── Шаблоны промптов (v3.4) ─────────────────────────────────────────
	// GET /api/templates           — список всех шаблонов (без поля prompt)
	// GET /api/templates/{name}    — полный шаблон (name + description + prompt + tokens)
	mux.Handle("/api/templates", protected(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		jsonOK(w, ListTemplates())
	}))

	mux.Handle("/api/templates/", protected(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/templates/"), "/")
		if name == "" {
			jsonOK(w, ListTemplates())
			return
		}
		tmpl, ok := GetTemplate(name)
		if !ok {
			http.Error(w, `{"error":"шаблон не найден"}`, 404)
			return
		}
		jsonOK(w, tmpl)
	}))

	// ── Статистика токенов (v3.4) ─────────────────────────────────────────
	// GET /api/token-stats — сводка экономии токенов с момента запуска
	mux.Handle("/api/token-stats", protected(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		jsonOK(w, GetTokenStats())
	}))

	// ── Семантический кэш (v3.6) ────────────────────────────────────────
	// GET    /api/semantic-cache/stats  — статистика
	// DELETE /api/semantic-cache        — очистить кэш
	mux.Handle("/api/semantic-cache/stats", protected(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		if globalSemanticCache == nil {
			jsonOK(w, SemanticCacheDisabledStats())
			return
		}
		jsonOK(w, globalSemanticCache.Stats())
	}))

	mux.Handle("/api/semantic-cache", protected(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", 405)
			return
		}
		if globalSemanticCache == nil {
			jsonOK(w, map[string]bool{"ok": true, "was_enabled": false})
			return
		}
		globalSemanticCache.Clear()
		jsonOK(w, map[string]bool{"ok": true})
	}))

	// ── Очередь фоновых задач (v3.6) ─────────────────────────────────────
	// POST   /api/jobs              — отправить задачу {"kind":"swarm"|"multiagent","payload":{...}}
	// GET    /api/jobs              — список задач
	// GET    /api/jobs/{id}         — статус и результат задачи
	// DELETE /api/jobs/{id}         — удалить задачу
	mux.Handle("/api/jobs", protected(func(w http.ResponseWriter, r *http.Request) {
		if globalJobQueue == nil {
			http.Error(w, "очередь задач не инициализирована", 503)
			return
		}
		switch r.Method {
		case http.MethodGet:
			jsonOK(w, globalJobQueue.List())
		case http.MethodPost:
			var body struct {
				Kind    string          `json:"kind"`
				Payload json.RawMessage `json:"payload"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Kind == "" {
				http.Error(w, `{"error":"нужны поля kind и payload"}`, 400)
				return
			}
			job, err := globalJobQueue.Submit(body.Kind, body.Payload)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			jsonOK(w, job)
		default:
			http.Error(w, "method not allowed", 405)
		}
	}))

	mux.Handle("/api/jobs/", protected(func(w http.ResponseWriter, r *http.Request) {
		if globalJobQueue == nil {
			http.Error(w, "очередь задач не инициализирована", 503)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/jobs/"), "/")
		if id == "" {
			http.Error(w, "missing job id", 400)
			return
		}
		switch r.Method {
		case http.MethodGet:
			job, ok := globalJobQueue.Get(id)
			if !ok {
				http.Error(w, `{"error":"задача не найдена"}`, 404)
				return
			}
			jsonOK(w, job)
		case http.MethodDelete:
			if err := globalJobQueue.Delete(id); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			jsonOK(w, map[string]bool{"ok": true})
		default:
			http.Error(w, "method not allowed", 405)
		}
	}))

	// ── Сессии ──────────────────────────────────────────────────────────
	mux.Handle("/api/sessions", protected(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		list, _ := store.List()

		// По умолчанию фильтруем агент-сессии (agent_ prefix).
		// Передай ?include_agent=true чтобы увидеть их тоже.
		includeAgent := r.URL.Query().Get("include_agent") == "true"
		var filtered []SessionMeta
		for _, s := range list {
			if includeAgent || !strings.HasPrefix(s.ID, "agent_") {
				filtered = append(filtered, s)
			}
		}
		if filtered == nil {
			filtered = []SessionMeta{}
		}
		jsonOK(w, filtered)
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
	// POST /api/chat  {message, model, session_id, temperature, use_rag,
	//                  smart_context, template}
	//
	// smart_context: true — применить SmartContext (фильтрация по релевантности).
	// template:      имя шаблона из /api/templates (напр. "code", "debug").
	mux.Handle("/api/chat", protected(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var body struct {
			Message      string  `json:"message"`
			Model        string  `json:"model"`
			SessionID    string  `json:"session_id"`
			Temp         float64 `json:"temperature"`
			UseRAG       bool    `json:"use_rag"`
			SmartContext bool    `json:"smart_context"`  // v3.4: фильтрация контекста
			Template     string  `json:"template"`       // v3.4: имя шаблона промпта
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

		// v3.4: подставляем системный промпт шаблона (если указан и отличается).
		// Меняем только промпт для этого запроса — не сохраняем в сессии постоянно,
		// чтобы не перетирать пользовательский system prompt.
		if body.Template != "" {
			if tmpl, ok := GetTemplate(body.Template); ok {
				defaultTokens := estimateTemplateTokens(systemPrompt)
				saved := defaultTokens - tmpl.Tokens
				RecordTemplateUsage(saved)
				// Применяем шаблон только если у сессии нет кастомного system prompt
				if sess.Settings.SystemPrompt == "" {
					sess.Messages = ApplyTemplate(sess.Messages, body.Template)
				}
			}
		}

		// Сохраняем сообщение пользователя
		_ = store.AppendAndSave(sess, Message{Role: "user", Content: body.Message})

		// Сжимаем историю если слишком длинная (экономия токенов)
		ctx := r.Context()
		tokensBefore := EstimateTokens(sess.Messages)
		compressed, wasCompressed, _ := CompressHistory(ctx, client, sess.Messages, model)
		if wasCompressed {
			RecordCompression(tokensBefore, EstimateTokens(compressed))
			sess.Messages = compressed
			_ = store.Save(sess)
		}

		// RAG: инжектируем контекст из документов
		msgs := sess.Messages

		// v3.4: SmartContext — фильтруем нерелевантные сообщения из истории.
		// Применяем перед RAG-инжекцией, чтобы не раздувать контекст.
		if body.SmartContext && len(msgs) > 4 {
			const smartBudget = 3000 // токенов; агентский бюджет из compress.go
			filtered, origTok, filteredTok := SmartContext(msgs, body.Message, smartBudget, defaultTopN)
			RecordContextFilter(origTok, filteredTok)
			msgs = filtered
		}
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

		// v3.6: Семантический кэш — проверяем до вызова LLM.
		// При попадании стримим кэшированный ответ как токены (без LLM-вызова).
		if globalSemanticCache != nil {
			if cached, hit, _ := globalSemanticCache.Lookup(ctx, body.Message); hit {
				// Стримим кэшированный ответ по словам (имитируем streaming)
				for _, word := range strings.Fields(cached) {
					writeEv(map[string]string{"token": word + " "})
				}
				_ = store.AppendAndSave(sess, Message{Role: "assistant", Content: cached})
				writeEv(map[string]any{"done": true, "cached": true})
				return
			}
		}

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
			// v3.6: асинхронно сохраняем в семантический кэш
			if globalSemanticCache != nil {
				globalSemanticCache.StoreAsync(body.Message, resp)
			}
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
		// Агент-сессии всегда хранятся с префиксом "agent_", чтобы не смешиваться
		// с обычными сессиями в /api/sessions.
		if !strings.HasPrefix(body.SessionID, "agent_") {
			if body.SessionID == "" {
				body.SessionID = "agent_default"
			} else {
				body.SessionID = "agent_" + body.SessionID
			}
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
	fmt.Printf("%s[✓] LocalAI v3.6 запущен%s\n", colorGreen, colorReset)
	fmt.Printf("    Веб:        %shttp://localhost:%s%s\n", colorCyan, port, colorReset)
	fmt.Printf("    Агент:      POST /api/agent\n")
	fmt.Printf("    Рой:        POST /api/swarm       (до %d агентов)\n", swarmMaxAgents)
	fmt.Printf("    Мульти:     POST /api/multiagent  (%d ролей)\n", len(AllRoles()))
	fmt.Printf("    Агенты:     GET  /api/agents\n")
	fmt.Printf("    Кэш:        GET  /api/cache/stats\n")
	fmt.Printf("    RAG:        POST /api/upload | GET /api/docs\n")
	fmt.Printf("    Шаблоны:    GET  /api/templates  (%d шаблонов)\n", len(builtinTemplates))
	fmt.Printf("    Токены:     GET  /api/token-stats\n")
	if scEnabled {
		fmt.Printf("    СемКэш:     GET  /api/semantic-cache/stats\n")
	}
	fmt.Printf("    Задачи:     POST /api/jobs | GET /api/jobs/{id}\n")
	fmt.Printf("    OpenAI:     %shttp://localhost:%s/v1%s\n", colorCyan, port, colorReset)
	fmt.Printf("    Auth:       POST /api/auth/login | /api/auth/setup\n")
	fmt.Printf("    Аудио:      POST /api/audio/transcriptions | /api/audio/speech\n")
	fmt.Printf("    Метрики:    GET  /metrics  (Prometheus)\n")
	fmt.Printf("    Ноды:       %d/%d Ollama нод доступны\n", balancer.HealthyCount(), balancer.TotalCount())
	fmt.Printf("    Данные:     %s%s%s\n\n", colorGray, dataDir, colorReset)

	// ── Graceful shutdown: ждём SIGINT / SIGTERM ─────────────────────────
	srv := &http.Server{
		Addr:         addr,
		Handler:      withLogging(mux),
		ReadTimeout:  120 * time.Second, // долгие SSE-стримы
		WriteTimeout: 0,                  // 0 = нет тайм-аута записи (нужен для SSE)
		IdleTimeout:  60 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Сервер: %v", err)
		}
	}()

	<-quit
	fmt.Printf("\n%s[→] Завершение работы...%s\n", colorYellow, colorReset)

	// Даём активным соединениям 10 секунд на завершение
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[!] Принудительное завершение: %v", err)
	}
	fmt.Printf("%s[✓] Сервер остановлен%s\n", colorGreen, colorReset)
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
// PDF: через pdftotext (poppler-utils) если установлен, иначе сообщение об ошибке.
func extractText(data []byte, filename string) string {
	lower := strings.ToLower(filename)

	// PDF: пробуем pdftotext
	if strings.HasSuffix(lower, ".pdf") {
		text, err := extractPDF(data)
		if err != nil {
			return "[PDF: " + err.Error() + ". Установи poppler-utils или сконвертируй в TXT.]"
		}
		return text
	}

	// DOCX: читаем через archive/zip + encoding/xml (stdlib, без зависимостей)
	if strings.HasSuffix(lower, ".docx") {
		text, err := extractDOCX(data)
		if err != nil {
			return "[DOCX: " + err.Error() + "]"
		}
		return text
	}

	// DOC (старый бинарный формат Microsoft Word): пробуем antiword
	if strings.HasSuffix(lower, ".doc") {
		text, err := extractDOC(data)
		if err != nil {
			return "[DOC: " + err.Error() + ". Установи antiword или сохрани документ в DOCX/TXT.]"
		}
		return text
	}

	// Всё остальное — текст
	text := string(data)

	// Базовая очистка HTML-тегов для .html файлов
	if strings.HasSuffix(lower, ".html") || strings.HasSuffix(lower, ".htm") {
		text = stripHTMLTags(text)
	}

	return text
}

// extractPDF извлекает текст из PDF через pdftotext (poppler-utils).
// Данные записываются во временный файл, т.к. pdftotext не умеет читать stdin.
func extractPDF(data []byte) (string, error) {
	// Проверяем наличие pdftotext
	if _, err := os.Stat("/usr/bin/pdftotext"); os.IsNotExist(err) {
		// Пробуем PATH
		if !commandExists("pdftotext") {
			return "", fmt.Errorf("pdftotext не найден")
		}
	}

	// Временный файл
	tmp, err := os.CreateTemp("", "localai-*.pdf")
	if err != nil {
		return "", fmt.Errorf("временный файл: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()

	// pdftotext -layout -enc UTF-8 input.pdf - (- означает stdout)
	out, err := runCommand("pdftotext", "-layout", "-enc", "UTF-8", tmp.Name(), "-")
	if err != nil {
		return "", fmt.Errorf("pdftotext: %w", err)
	}

	text := strings.TrimSpace(string(out))
	if text == "" {
		return "", fmt.Errorf("PDF не содержит извлекаемого текста (возможно сканированный документ)")
	}
	return text, nil
}

// commandExists проверяет что команда есть в PATH.
func commandExists(name string) bool {
	_, err := os.Stat("/usr/local/bin/" + name)
	if err == nil {
		return true
	}
	// Поиск по PATH через exec
	return runCommandExists(name)
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

// extractDOCX извлекает текст из DOCX-файла (ZIP + XML) без внешних зависимостей.
//
// DOCX — это ZIP-архив. Текст хранится в word/document.xml в элементах <w:t>.
// Параграфы разделяются переносом строки.
func extractDOCX(data []byte) (string, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("не удалось открыть DOCX как ZIP: %w", err)
	}

	// Ищем word/document.xml в архиве
	var xmlData []byte
	for _, f := range r.File {
		if f.Name == "word/document.xml" {
			rc, err := f.Open()
			if err != nil {
				return "", fmt.Errorf("открытие word/document.xml: %w", err)
			}
			xmlData, err = io.ReadAll(io.LimitReader(rc, 16*1024*1024)) // макс 16 MB
			rc.Close()
			if err != nil {
				return "", fmt.Errorf("чтение word/document.xml: %w", err)
			}
			break
		}
	}
	if xmlData == nil {
		return "", fmt.Errorf("word/document.xml не найден — файл повреждён или не является DOCX")
	}

	// Парсим XML: собираем текст из <w:t> и разделяем параграфы <w:p>
	var sb strings.Builder
	dec := xml.NewDecoder(bytes.NewReader(xmlData))
	inPara := false

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Некорректный XML — возвращаем что успели собрать
			break
		}

		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p": // <w:p> — параграф
				inPara = true
			case "br": // <w:br> — перенос строки внутри параграфа
				sb.WriteByte('\n')
			}
		case xml.EndElement:
			if t.Name.Local == "p" && inPara {
				sb.WriteByte('\n')
				inPara = false
			}
		case xml.CharData:
			// Текст вне <w:t> может быть служебным — пропускаем через родительский элемент.
			// xml.Decoder автоматически вызывает CharData только для текстовых узлов.
			sb.Write([]byte(t))
		}
	}

	text := strings.TrimSpace(sb.String())
	if text == "" {
		return "", fmt.Errorf("документ не содержит текста")
	}
	return text, nil
}

// extractDOC извлекает текст из .doc (старый формат Word) через antiword.
// Возвращает ошибку если antiword не установлен.
func extractDOC(data []byte) (string, error) {
	if !commandExists("antiword") {
		return "", fmt.Errorf("antiword не найден")
	}

	// antiword читает из файла, не из stdin — пишем во временный файл
	tmp, err := os.CreateTemp("", "localai-*.doc")
	if err != nil {
		return "", fmt.Errorf("временный файл: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()

	out, err := runCommand("antiword", tmp.Name())
	if err != nil {
		return "", fmt.Errorf("antiword: %w", err)
	}

	text := strings.TrimSpace(string(out))
	if text == "" {
		return "", fmt.Errorf("документ не содержит текста")
	}
	return text, nil
}
