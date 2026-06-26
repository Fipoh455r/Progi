// ollama.go — клиент для взаимодействия с Ollama API.
// Документация Ollama API: https://github.com/ollama/ollama/blob/main/docs/api.md
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// --- Типы запросов и ответов ---

// Message — одно сообщение в диалоге.
type Message struct {
	Role    string `json:"role"`    // "system" | "user" | "assistant"
	Content string `json:"content"` // текст сообщения
}

// chatRequest — тело запроса к /api/chat.
type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
	Options  chatOpts  `json:"options,omitempty"`
}

// chatOpts — параметры генерации.
type chatOpts struct {
	Temperature float64 `json:"temperature,omitempty"`
	NumPredict  int     `json:"num_predict,omitempty"`
}

// chatChunk — одна строка streaming-ответа от /api/chat.
type chatChunk struct {
	Message Message `json:"message"`
	Done    bool    `json:"done"`
	Error   string  `json:"error,omitempty"`
}

// ModelInfo — информация об одной модели.
type ModelInfo struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
}

// modelsResponse — ответ /api/tags.
type modelsResponse struct {
	Models []ModelInfo `json:"models"`
}

// pullProgress — одна строка streaming-ответа от /api/pull.
type pullProgress struct {
	Status    string `json:"status"`
	Digest    string `json:"digest,omitempty"`
	Total     int64  `json:"total,omitempty"`
	Completed int64  `json:"completed,omitempty"`
	Error     string `json:"error,omitempty"`
}

// --- Клиент ---

// OllamaClient — HTTP-клиент для Ollama.
type OllamaClient struct {
	baseURL string
	http    *http.Client
}

// NewOllamaClient создаёт клиент с указанным базовым URL.
func NewOllamaClient(baseURL string) *OllamaClient {
	return &OllamaClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			// Таймаут не задаём на уровне клиента:
			// streaming-запросы могут длиться долго.
			// Для обычных запросов используем контекст.
		},
	}
}

// IsAvailable проверяет, доступен ли Ollama.
func (c *OllamaClient) IsAvailable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ChatStream — обёртка с температурой по умолчанию (0.7).
func (c *OllamaClient) ChatStream(ctx context.Context, messages []Message, model string) (<-chan string, <-chan error) {
	return c.chatStreamImpl(ctx, messages, model, 0.7)
}

// ChatStreamWithTemp — стриминг с указанной температурой.
func (c *OllamaClient) ChatStreamWithTemp(ctx context.Context, messages []Message, model string, temperature float64) (<-chan string, <-chan error) {
	return c.chatStreamImpl(ctx, messages, model, temperature)
}

// chatStreamImpl — внутренняя реализация стримингового чата.
func (c *OllamaClient) chatStreamImpl(ctx context.Context, messages []Message, model string, temperature float64) (<-chan string, <-chan error) {
	tokenCh := make(chan string, 64)
	errCh := make(chan error, 1)

	go func() {
		defer close(tokenCh)
		defer close(errCh)

		body, err := json.Marshal(chatRequest{
			Model:    model,
			Messages: messages,
			Stream:   true,
			Options: chatOpts{
				Temperature: temperature,
			},
		})
		if err != nil {
			errCh <- fmt.Errorf("ошибка сериализации: %w", err)
			return
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.baseURL+"/api/chat", bytes.NewReader(body))
		if err != nil {
			errCh <- fmt.Errorf("ошибка создания запроса: %w", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			errCh <- fmt.Errorf("ollama недоступен (%s): %w", c.baseURL, err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			errCh <- fmt.Errorf("ollama вернул статус %d", resp.StatusCode)
			return
		}

		scanner := bufio.NewScanner(resp.Body)
		// Ollama может возвращать большие строки с кодом
		scanner.Buffer(make([]byte, 64*1024), 64*1024)

		for scanner.Scan() {
			// Проверяем отмену контекста
			select {
			case <-ctx.Done():
				return
			default:
			}

			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}

			var chunk chatChunk
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				// Пропускаем нераспознанные строки
				continue
			}

			if chunk.Error != "" {
				errCh <- fmt.Errorf("ollama: %s", chunk.Error)
				return
			}

			if chunk.Message.Content != "" {
				select {
				case tokenCh <- chunk.Message.Content:
				case <-ctx.Done():
					return
				}
			}

			if chunk.Done {
				return
			}
		}

		if err := scanner.Err(); err != nil {
			// Не репортим ошибки отменённого контекста как настоящие ошибки
			if ctx.Err() == nil {
				errCh <- fmt.Errorf("ошибка чтения потока: %w", err)
			}
		}
	}()

	return tokenCh, errCh
}

// ListModels возвращает список моделей, загруженных в Ollama.
func (c *OllamaClient) ListModels() ([]ModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama недоступен: %w", err)
	}
	defer resp.Body.Close()

	var result modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ошибка декодирования ответа: %w", err)
	}

	return result.Models, nil
}

// PullModel скачивает модель из реестра Ollama.
// progress — колбэк, вызывается с текстом статуса по мере загрузки.
func (c *OllamaClient) PullModel(name string, progress func(status string, pct int)) error {
	body, _ := json.Marshal(map[string]any{
		"name":   name,
		"stream": true,
	})

	// Загрузка может занять несколько минут — без таймаута
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ollama недоступен: %w", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var p pullProgress
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			continue
		}

		if p.Error != "" {
			return fmt.Errorf("ollama pull: %s", p.Error)
		}

		if progress != nil {
			pct := 0
			if p.Total > 0 {
				pct = int(p.Completed * 100 / p.Total)
			}
			progress(p.Status, pct)
		}
	}

	return scanner.Err()
}
