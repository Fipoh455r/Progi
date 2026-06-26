// server.go — HTTP-сервер с веб-интерфейсом.
// Веб-страница (static/index.html) зашита в бинарник через go:embed.
package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
)

//go:embed static/index.html
var staticFiles embed.FS

// chatSession хранит историю одного диалога.
type chatSession struct {
	History []Message
}

// sessionStore — потокобезопасное хранилище сессий.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*chatSession
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]*chatSession)}
}

// get возвращает сессию, создавая новую если её нет.
func (s *sessionStore) get(id string) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		sess = &chatSession{
			History: []Message{{Role: "system", Content: systemPrompt}},
		}
		s.sessions[id] = sess
	}
	// Возвращаем копию, чтобы не было гонок при чтении
	cp := make([]Message, len(sess.History))
	copy(cp, sess.History)
	return cp
}

// append добавляет сообщения в историю сессии.
func (s *sessionStore) append(id string, msgs ...Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		sess = &chatSession{
			History: []Message{{Role: "system", Content: systemPrompt}},
		}
		s.sessions[id] = sess
	}
	sess.History = append(sess.History, msgs...)
}

// clear сбрасывает историю сессии до начального состояния.
func (s *sessionStore) clear(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = &chatSession{
		History: []Message{{Role: "system", Content: systemPrompt}},
	}
}

// runServer запускает HTTP-сервер с веб-интерфейсом.
func runServer(ollamaURL, defaultModel, port string) {
	client := NewOllamaClient(ollamaURL)
	store := newSessionStore()

	if !client.IsAvailable() {
		fmt.Printf("%s[!] Ollama недоступен по %s%s\n", colorYellow, ollamaURL, colorReset)
		fmt.Println("    Сервер запущен, но ответы работать не будут пока не поднят Ollama.")
	}

	mux := http.NewServeMux()

	// GET / — главная страница (из embed)
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

	// GET /api/models — список моделей
	mux.HandleFunc("/api/models", func(w http.ResponseWriter, r *http.Request) {
		models, err := client.ListModels()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(models)
	})

	// POST /api/chat — SSE-поток токенов
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var body struct {
			Message   string `json:"message"`
			Model     string `json:"model"`
			SessionID string `json:"session_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}

		if strings.TrimSpace(body.Message) == "" {
			http.Error(w, "message is empty", http.StatusBadRequest)
			return
		}
		if body.Model == "" {
			body.Model = defaultModel
		}
		if body.SessionID == "" {
			body.SessionID = "default"
		}

		// Добавляем сообщение пользователя в историю
		store.append(body.SessionID, Message{Role: "user", Content: body.Message})
		history := store.get(body.SessionID)

		// SSE-заголовки
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		// Используем контекст запроса — когда клиент закрывает соединение,
		// контекст отменяется и ChatStream завершается.
		ctx := r.Context()

		tokenCh, errCh := client.ChatStream(ctx, history, body.Model)

		var fullResponse strings.Builder

		writeEvent := func(v any) {
			data, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}

		for token := range tokenCh {
			fullResponse.WriteString(token)
			writeEvent(map[string]string{"token": token})
		}

		if err := <-errCh; err != nil {
			// Не репортим ошибку отменённого контекста (клиент ушёл)
			if ctx.Err() == nil {
				writeEvent(map[string]string{"error": err.Error()})
				// Убираем неудачное сообщение пользователя из истории
				store.clear(body.SessionID)
				store.append(body.SessionID,
					history[1:len(history)-1]...) // восстанавливаем без последнего user msg
			}
			return
		}

		// Сохраняем ответ ассистента
		if resp := fullResponse.String(); resp != "" {
			store.append(body.SessionID, Message{Role: "assistant", Content: resp})
		}

		writeEvent(map[string]bool{"done": true})
	})

	// POST /api/clear — сброс истории сессии
	mux.HandleFunc("/api/clear", func(w http.ResponseWriter, r *http.Request) {
		sid := r.URL.Query().Get("session")
		if sid == "" {
			sid = "default"
		}
		store.clear(sid)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})

	// Health-check
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		ok := client.IsAvailable()
		status := http.StatusOK
		if !ok {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"ollama":%v}`, ok)
	})

	addr := ":" + port
	fmt.Printf("%s[✓] LocalAI веб-сервер запущен%s\n", colorGreen, colorReset)
	fmt.Printf("    Открой: %shttp://localhost:%s%s\n", colorCyan, port, colorReset)
	fmt.Printf("    Модель: %s%s%s\n\n", colorYellow, defaultModel, colorReset)

	if err := http.ListenAndServe(addr, withLogging(mux)); err != nil {
		log.Fatalf("Ошибка сервера: %v", err)
	}
}

// withLogging — простой middleware для логирования запросов.
func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Не логируем SSE-запросы и health-check (шумят)
		if r.URL.Path != "/api/chat" && r.URL.Path != "/health" {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}


