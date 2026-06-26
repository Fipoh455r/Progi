// server.go — HTTP-сервер с веб-интерфейсом и персистентным хранением сессий.
package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

//go:embed static/index.html
var staticFiles embed.FS

// runServer запускает HTTP-сервер.
func runServer(ollamaURL, defaultModel, port, dataDir string) {
	client := NewOllamaClient(ollamaURL)

	store, err := NewStorage(dataDir)
	if err != nil {
		log.Fatalf("Не удалось открыть хранилище данных: %v", err)
	}

	if !client.IsAvailable() {
		fmt.Printf("%s[!] Ollama недоступен по %s%s\n", colorYellow, ollamaURL, colorReset)
		fmt.Println("    Сервер запущен, ответы появятся когда Ollama будет готов.")
	}

	mux := http.NewServeMux()

	// ── Статика ────────────────────────────────────────────────────────────
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := staticFiles.ReadFile("static/index.html")
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	// ── Модели ─────────────────────────────────────────────────────────────
	mux.HandleFunc("/api/models", func(w http.ResponseWriter, r *http.Request) {
		models, err := client.ListModels()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		jsonOK(w, models)
	})

	// ── Список сессий ──────────────────────────────────────────────────────
	// GET /api/sessions
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		list, err := store.List()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if list == nil {
			list = []SessionMeta{}
		}
		jsonOK(w, list)
	})

	// ── Операции с конкретной сессией ──────────────────────────────────────
	// GET    /api/sessions/{id}  — загрузить сессию (с историей)
	// PATCH  /api/sessions/{id}  — изменить настройки (title, model, temp, prompt)
	// DELETE /api/sessions/{id}  — удалить
	mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
		id = strings.TrimSuffix(id, "/")
		if id == "" {
			http.Error(w, "missing session id", 400)
			return
		}

		switch r.Method {
		case http.MethodGet:
			sess, err := store.Load(id)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			if sess == nil {
				http.NotFound(w, r)
				return
			}
			jsonOK(w, sess)

		case http.MethodPatch:
			sess, err := store.Load(id)
			if err != nil || sess == nil {
				http.Error(w, "session not found", 404)
				return
			}
			var patch struct {
				Title        *string  `json:"title"`
				Model        *string  `json:"model"`
				Temperature  *float64 `json:"temperature"`
				SystemPrompt *string  `json:"system_prompt"`
			}
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				http.Error(w, "bad request", 400)
				return
			}
			if patch.Title != nil {
				sess.Title = *patch.Title
			}
			if patch.Model != nil {
				sess.Settings.Model = *patch.Model
			}
			if patch.Temperature != nil {
				sess.Settings.Temperature = *patch.Temperature
			}
			if patch.SystemPrompt != nil {
				sess.Settings.SystemPrompt = *patch.SystemPrompt
				// Обновляем системное сообщение в истории
				for i, m := range sess.Messages {
					if m.Role == "system" {
						sess.Messages[i].Content = *patch.SystemPrompt
						break
					}
				}
			}
			if err := store.Save(sess); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			jsonOK(w, sess.SessionMeta)

		case http.MethodDelete:
			if err := store.Delete(id); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			jsonOK(w, map[string]bool{"ok": true})

		default:
			http.Error(w, "method not allowed", 405)
		}
	})

	// ── Чат (SSE streaming) ────────────────────────────────────────────────
	// POST /api/chat
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
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request: "+err.Error(), 400)
			return
		}
		if strings.TrimSpace(body.Message) == "" {
			http.Error(w, "message is empty", 400)
			return
		}
		if body.SessionID == "" {
			body.SessionID = "default"
		}

		// Загружаем или создаём сессию
		sess, err := store.GetOrCreate(body.SessionID, defaultModel, "")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		// Модель: приоритет запроса > настройки сессии > дефолт сервера
		model := body.Model
		if model == "" {
			model = sess.Settings.Model
		}
		if model == "" {
			model = defaultModel
		}

		// Температура: из запроса или из настроек сессии
		temp := body.Temp
		if temp == 0 {
			temp = sess.Settings.Temperature
		}
		if temp == 0 {
			temp = 0.7
		}

		// Добавляем сообщение пользователя в историю
		if err := store.AppendAndSave(sess, Message{Role: "user", Content: body.Message}); err != nil {
			log.Printf("warn: не удалось сохранить сообщение: %v", err)
		}

		// SSE заголовки
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", 500)
			return
		}

		ctx := r.Context()
		tokenCh, errCh := client.ChatStreamWithTemp(ctx, sess.Messages, model, temp)

		writeEvent := func(v any) {
			data, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}

		var fullResp strings.Builder
		for token := range tokenCh {
			fullResp.WriteString(token)
			writeEvent(map[string]string{"token": token})
		}

		if err := <-errCh; err != nil {
			if ctx.Err() == nil {
				writeEvent(map[string]string{"error": err.Error()})
				// Откатываем последнее user-сообщение
				if len(sess.Messages) > 0 {
					sess.Messages = sess.Messages[:len(sess.Messages)-1]
					_ = store.Save(sess)
				}
			}
			return
		}

		// Сохраняем ответ ассистента
		resp := fullResp.String()
		if resp != "" {
			if err := store.AppendAndSave(sess, Message{Role: "assistant", Content: resp}); err != nil {
				log.Printf("warn: не удалось сохранить ответ: %v", err)
			}
		}

		writeEvent(map[string]bool{"done": true})
	})

	// ── Очистить историю ───────────────────────────────────────────────────
	mux.HandleFunc("/api/clear", func(w http.ResponseWriter, r *http.Request) {
		sid := r.URL.Query().Get("session")
		if sid == "" {
			sid = "default"
		}
		sess, _ := store.Load(sid)
		if sess != nil {
			prompt := sess.Settings.SystemPrompt
			if prompt == "" {
				prompt = systemPrompt
			}
			sess.Messages = []Message{{Role: "system", Content: prompt}}
			sess.Title = "Новый чат"
			_ = store.Save(sess)
		}
		jsonOK(w, map[string]bool{"ok": true})
	})

	// ── Health-check ───────────────────────────────────────────────────────
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		ok := client.IsAvailable()
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ollama":%v}`, ok)
	})

	// OpenAI-совместимый API (/v1/models, /v1/chat/completions)
	registerOpenAIRoutes(mux, client, defaultModel)

	addr := ":" + port
	fmt.Printf("%s[✓] LocalAI v1.2 запущен%s\n", colorGreen, colorReset)
	fmt.Printf("    Веб:   %shttp://localhost:%s%s\n", colorCyan, port, colorReset)
	fmt.Printf("    Данные: %s%s%s\n", colorGray, dataDir, colorReset)
	fmt.Printf("    Модель: %s%s%s\n\n", colorYellow, defaultModel, colorReset)

	if err := http.ListenAndServe(addr, withLogging(mux)); err != nil {
		log.Fatalf("Ошибка сервера: %v", err)
	}
}

// ── Вспомогательные функции ────────────────────────────────────────────────

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" && r.URL.Path != "/health" {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}
