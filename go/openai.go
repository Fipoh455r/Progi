// openai.go — OpenAI-совместимый API.
// Позволяет подключать VS Code (Continue.dev), SillyTavern, Open Interpreter
// и любые другие программы, работающие с OpenAI API.
//
// Реализованные эндпоинты:
//
//	GET  /v1/models
//	POST /v1/chat/completions   (streaming и non-streaming)
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ── OpenAI-совместимые типы ────────────────────────────────────────────────

// oaiModel описывает одну модель в формате OpenAI.
type oaiModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// oaiModelsResp — ответ GET /v1/models.
type oaiModelsResp struct {
	Object string     `json:"object"`
	Data   []oaiModel `json:"data"`
}

// oaiChatMsg — одно сообщение в формате OpenAI.
type oaiChatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// oaiChatReq — тело запроса POST /v1/chat/completions.
type oaiChatReq struct {
	Model       string       `json:"model"`
	Messages    []oaiChatMsg `json:"messages"`
	Stream      bool         `json:"stream"`
	Temperature *float64     `json:"temperature"`
	MaxTokens   *int         `json:"max_tokens"`
}

// oaiChoice — один вариант ответа (non-streaming).
type oaiChoice struct {
	Index        int        `json:"index"`
	Message      oaiChatMsg `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

// oaiChatResp — полный ответ POST /v1/chat/completions (non-streaming).
type oaiChatResp struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   oaiUsage    `json:"usage"`
}

// oaiUsage — статистика токенов.
type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// oaiDelta — дельта для streaming.
type oaiDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// oaiStreamChoice — один чанк streaming-ответа.
type oaiStreamChoice struct {
	Index        int      `json:"index"`
	Delta        oaiDelta `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
}

// oaiStreamChunk — один SSE-чанк в формате OpenAI.
type oaiStreamChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []oaiStreamChoice `json:"choices"`
}

// ── Регистрация маршрутов ──────────────────────────────────────────────────

// registerOpenAIRoutes добавляет /v1/* маршруты в mux.
func registerOpenAIRoutes(mux *http.ServeMux, client *OllamaClient, defaultModel string) {
	// GET /v1/models
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}

		models, err := client.ListModels()
		if err != nil {
			// Если Ollama недоступен — возвращаем хотя бы дефолтную модель
			models = nil
		}

		data := make([]oaiModel, 0, len(models)+1)
		seen := map[string]bool{}

		for _, m := range models {
			data = append(data, oaiModel{
				ID:      m.Name,
				Object:  "model",
				Created: m.ModifiedAt.Unix(),
				OwnedBy: "local",
			})
			seen[m.Name] = true
		}

		// Добавляем дефолтную модель если её нет в списке
		if !seen[defaultModel] {
			data = append(data, oaiModel{
				ID:      defaultModel,
				Object:  "model",
				Created: time.Now().Unix(),
				OwnedBy: "local",
			})
		}

		jsonOK(w, oaiModelsResp{Object: "list", Data: data})
	})

	// POST /v1/chat/completions
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}

		var req oaiChatReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			oaiError(w, "invalid_request_error", "bad request: "+err.Error(), 400)
			return
		}

		if len(req.Messages) == 0 {
			oaiError(w, "invalid_request_error", "messages is empty", 400)
			return
		}

		model := req.Model
		if model == "" {
			model = defaultModel
		}

		temp := 0.7
		if req.Temperature != nil {
			temp = *req.Temperature
		}

		// Конвертируем oaiChatMsg → Message (внутренний тип)
		msgs := make([]Message, len(req.Messages))
		for i, m := range req.Messages {
			msgs[i] = Message{Role: m.Role, Content: m.Content}
		}

		reqID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
		created := time.Now().Unix()

		if req.Stream {
			// ── Streaming ────────────────────────────────────────────────
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no")

			flusher, ok := w.(http.Flusher)
			if !ok {
				oaiError(w, "server_error", "streaming not supported", 500)
				return
			}

			ctx := r.Context()
			tokenCh, errCh := client.ChatStreamWithTemp(ctx, msgs, model, temp)

			// Первый чанк: роль ассистента
			sendSSEChunk(w, flusher, oaiStreamChunk{
				ID:      reqID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []oaiStreamChoice{{
					Index: 0,
					Delta: oaiDelta{Role: "assistant"},
				}},
			})

			for token := range tokenCh {
				sendSSEChunk(w, flusher, oaiStreamChunk{
					ID:      reqID,
					Object:  "chat.completion.chunk",
					Created: created,
					Model:   model,
					Choices: []oaiStreamChoice{{
						Index: 0,
						Delta: oaiDelta{Content: token},
					}},
				})
			}

			if err := <-errCh; err != nil && ctx.Err() == nil {
				// Завершаем поток с ошибкой
				reason := "error"
				sendSSEChunk(w, flusher, oaiStreamChunk{
					ID:      reqID,
					Object:  "chat.completion.chunk",
					Created: created,
					Model:   model,
					Choices: []oaiStreamChoice{{
						Index:        0,
						Delta:        oaiDelta{},
						FinishReason: &reason,
					}},
				})
				fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}

			// Финальный чанк
			reason := "stop"
			sendSSEChunk(w, flusher, oaiStreamChunk{
				ID:      reqID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []oaiStreamChoice{{
					Index:        0,
					Delta:        oaiDelta{},
					FinishReason: &reason,
				}},
			})
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()

		} else {
			// ── Non-streaming ─────────────────────────────────────────────
			ctx := r.Context()
			tokenCh, errCh := client.ChatStreamWithTemp(ctx, msgs, model, temp)

			var sb strings.Builder
			for token := range tokenCh {
				sb.WriteString(token)
			}
			if err := <-errCh; err != nil {
				oaiError(w, "server_error", err.Error(), 502)
				return
			}

			content := sb.String()
			// Грубая оценка токенов: ~4 символа на токен
			promptTok := estimateTokens(msgs)
			completionTok := len([]rune(content)) / 4

			jsonOK(w, oaiChatResp{
				ID:      reqID,
				Object:  "chat.completion",
				Created: created,
				Model:   model,
				Choices: []oaiChoice{{
					Index: 0,
					Message: oaiChatMsg{
						Role:    "assistant",
						Content: content,
					},
					FinishReason: "stop",
				}},
				Usage: oaiUsage{
					PromptTokens:     promptTok,
					CompletionTokens: completionTok,
					TotalTokens:      promptTok + completionTok,
				},
			})
		}
	})
}

// ── Вспомогательные функции ────────────────────────────────────────────────

// sendSSEChunk сериализует чанк и отправляет его как SSE-событие.
func sendSSEChunk(w http.ResponseWriter, f http.Flusher, chunk oaiStreamChunk) {
	data, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	f.Flush()
}

// oaiError отправляет ответ в формате ошибки OpenAI.
func oaiError(w http.ResponseWriter, errType, message string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}

// estimateTokens грубо оценивает количество токенов в сообщениях.
func estimateTokens(msgs []Message) int {
	total := 0
	for _, m := range msgs {
		total += len([]rune(m.Content)) / 4
	}
	return total
}
