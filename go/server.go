// server.go — HTTP-сервер v3.8: чат + агент + RAG + гибридный поиск + роутер + code interpreter.
package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

//go:embed static/index.html
var staticFiles embed.FS

const maxUploadSize = 10 << 20 // 10 MB

// runServer запускает HTTP-сервер.
func runServer(ollamaURL, defaultModel, port, dataDir string) {
	client := NewOllamaClient(ollamaURL)

	store, err := NewStorage(dataDir)
	if err != nil {
		log.Fatalf("Хранилище: %v", err)
	}

	rag, err := NewRAG(dataDir+"/rag", client, "nomic-embed-text")
	if err != nil {
		log.Fatalf("RAG: %v", err)
	}

	// Гибридный поисковик (BM25 + cosine); строит BM25-индекс при старте.
	hs := NewHybridSearcher(rag)

	if !client.IsAvailable() {
		fmt.Printf("%s[!] Ollama недоступен (%s). Запусти Docker.%s\n", colorYellow, ollamaURL, colorReset)
	}

	mux := http.NewServeMux()

	// ── Главная страница ─────────────────────────────────────────────────
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, _ := staticFiles.ReadFile("static/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	// ── Модели ──────────────────────────────────────────────────────────
	mux.HandleFunc("/api/models", func(w http.ResponseWriter, r *http.Request) {
		models, err := client.ListModels()
		if err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		jsonOK(w, models)
	})

	// ── Инструменты агента ───────────────────────────────────────────────
	mux.HandleFunc("/api/tools", func(w http.ResponseWriter, r *http.Request) {
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
	})

	// ── Сессии ──────────────────────────────────────────────────────────
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		list, _ := store.List()
		if list == nil {
			list = []SessionMeta{}
		}
		jsonOK(w, list)
	})

	mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
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
	})

	// ── Обычный чат (SSE) ────────────────────────────────────────────────
	// POST /api/chat  {message, model, session_id, temperature, use_rag}
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
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

		// RAG: инжектируем контекст из документов (гибридный поиск BM25+cosine)
		msgs := sess.Messages
		if body.UseRAG || len(rag.ListDocs()) > 0 {
			results, err := hs.Search(ctx, body.Message, 4, -1)
			if err == nil && len(results) > 0 {
				ragCtx := BuildContextString(results)
				if ragCtx != "" {
					// Вставляем контекст как system-сообщение перед последним user
					injected := make([]Message, len(msgs))
					copy(injected, msgs)
					// Добавляем перед последним сообщением
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

		tokenCh, errCh := client.ChatStreamWithTemp(ctx, msgs, model, temp)
		var sb strings.Builder
		for token := range tokenCh {
			sb.WriteString(token)
			writeEv(map[string]string{"token": token})
		}

		if err := <-errCh; err != nil {
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

		if resp := sb.String(); resp != "" {
			_ = store.AppendAndSave(sess, Message{Role: "assistant", Content: resp})
		}
		writeEv(map[string]bool{"done": true})
	})

	// ── Агент (SSE с шагами) ─────────────────────────────────────────────
	// POST /api/agent  {message, model, session_id, temperature}
	// Возвращает поток AgentStep как SSE
	mux.HandleFunc("/api/agent", func(w http.ResponseWriter, r *http.Request) {
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

		// Запускаем агента в горутине
		answerCh := make(chan string, 1)
		go func() {
			answer, _ := RunAgent(ctx, client, sess.Messages, model, temp, stepCh)
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
	})

	// ── RAG: загрузка документа ──────────────────────────────────────────
	// POST /api/upload  (multipart/form-data, поле "file")
	mux.HandleFunc("/api/upload", func(w http.ResponseWriter, r *http.Request) {
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
			http.Error(w, "ошибка индексации: "+err.Error(), 500)
			return
		}
		hs.RebuildBM25() // обновляем BM25-индекс после нового документа

		jsonOK(w, map[string]any{
			"name":   header.Filename,
			"chunks": n,
			"size":   len(content),
		})
	})

	// ── RAG: список и удаление документов ───────────────────────────────
	mux.HandleFunc("/api/docs", func(w http.ResponseWriter, r *http.Request) {
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
	})

	mux.HandleFunc("/api/docs/", func(w http.ResponseWriter, r *http.Request) {
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
		hs.RebuildBM25() // обновляем BM25-индекс после удаления
		jsonOK(w, map[string]bool{"ok": true})
	})

	// ── Очистить историю ─────────────────────────────────────────────────
	mux.HandleFunc("/api/clear", func(w http.ResponseWriter, r *http.Request) {
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
	})

	// ── Роутер задач ─────────────────────────────────────────────────────
	// POST /api/router/classify  {query}  → {task, confidence, reason}
	mux.HandleFunc("/api/router/classify", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var body struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Query) == "" {
			http.Error(w, "bad request: поле query обязательно", 400)
			return
		}
		result := ClassifyTask(body.Query)
		jsonOK(w, result)
	})

	// ── Health ───────────────────────────────────────────────────────────
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		ok := client.IsAvailable()
		if !ok {
			w.WriteHeader(503)
		}
		w.Header().Set("Content-Type", "application/json")
		docCount := len(rag.ListDocs())
		fmt.Fprintf(w, `{"ollama":%v,"docs":%d}`, ok, docCount)
	})

	registerOpenAIRoutes(mux, client, defaultModel)

	addr := ":" + port
	fmt.Printf("%s[✓] LocalAI v3.8 запущен%s\n", colorGreen, colorReset)
	fmt.Printf("    Веб:    %shttp://localhost:%s%s\n", colorCyan, port, colorReset)
	fmt.Printf("    Агент:  POST /api/agent  (инструменты: %d)\n", len(AllTools))
	fmt.Printf("    RAG:    POST /api/upload | GET /api/docs  (BM25+cosine, code-aware)\n")
	fmt.Printf("    Роутер: POST /api/router/classify\n")
	fmt.Printf("    OpenAI: %shttp://localhost:%s/v1%s\n", colorCyan, port, colorReset)
	fmt.Printf("    Данные: %s%s%s\n\n", colorGray, dataDir, colorReset)

	if err := http.ListenAndServe(addr, withLogging(mux)); err != nil {
		log.Fatalf("Сервер: %v", err)
	}
}

// ── Вспомогательные ─────────────────────────────────────────────────────

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
