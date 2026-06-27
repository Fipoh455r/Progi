// rag.go — Retrieval-Augmented Generation (RAG).
//
// Позволяет задавать вопросы по своим документам:
//  1. Документ загружается → разбивается на чанки → каждый чанк эмбеддируется
//  2. При запросе: запрос эмбеддируется → cosine similarity → топ-K чанков
//  3. Топ-K чанков инжектируются в контекст перед сообщением пользователя
//
// Экономия токенов: вместо целого документа (тысячи токенов) инжектируем
// только 3-5 релевантных чанков (~500-800 токенов).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ── Типы ──────────────────────────────────────────────────────────────────

// Chunk — один фрагмент документа с эмбеддингом.
type Chunk struct {
	ID        string    `json:"id"`
	DocID     string    `json:"doc_id"`
	DocName   string    `json:"doc_name"`
	Text      string    `json:"text"`
	Embedding []float64 `json:"embedding"`
	ChunkIdx  int       `json:"chunk_idx"`
}

// DocMeta — метаданные документа (без чанков).
type DocMeta struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Size       int       `json:"size"`
	ChunkCount int       `json:"chunk_count"`
	AddedAt    time.Time `json:"added_at"`
}

// ragIndex — весь индекс на диске.
type ragIndex struct {
	Docs   []DocMeta `json:"docs"`
	Chunks []Chunk   `json:"chunks"`
}

// RAG управляет индексом документов.
type RAG struct {
	mu       sync.RWMutex
	dataDir  string
	index    *ragIndex
	client   *OllamaClient
	embedMod string // модель для эмбеддингов
}

// ── Инициализация ─────────────────────────────────────────────────────────

// NewRAG создаёт или загружает RAG-индекс.
// embedModel — модель для эмбеддингов. Если пустая, используется nomic-embed-text,
// при отсутствии — fallback на chat-модель.
func NewRAG(dataDir string, client *OllamaClient, embedModel string) (*RAG, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	r := &RAG{
		dataDir:  dataDir,
		client:   client,
		embedMod: embedModel,
		index:    &ragIndex{},
	}
	_ = r.load() // ошибку при первом запуске игнорируем
	return r, nil
}

func (r *RAG) indexPath() string {
	return filepath.Join(r.dataDir, "rag_index.json")
}

func (r *RAG) load() error {
	data, err := os.ReadFile(r.indexPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, r.index)
}

func (r *RAG) save() error {
	data, err := json.MarshalIndent(r.index, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.indexPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.indexPath())
}

// ── Добавление документа ──────────────────────────────────────────────────

// AddDocument добавляет документ в индекс: разбивает на чанки и эмбеддирует каждый.
// Возвращает количество созданных чанков.
func (r *RAG) AddDocument(ctx context.Context, name, text string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Удаляем старую версию документа с тем же именем
	r.removeDocByName(name)

	docID := fmt.Sprintf("doc_%d", time.Now().UnixNano())
	chunks := chunkText(text, 400, 60) // ~400 слов на чанк, 60 слов overlap

	var newChunks []Chunk
	for i, chunkText := range chunks {
		// Получаем эмбеддинг у Ollama
		emb, err := r.embed(ctx, chunkText)
		if err != nil {
			return 0, fmt.Errorf("эмбеддинг чанка %d: %w", i, err)
		}
		newChunks = append(newChunks, Chunk{
			ID:        fmt.Sprintf("%s_c%d", docID, i),
			DocID:     docID,
			DocName:   name,
			Text:      chunkText,
			Embedding: emb,
			ChunkIdx:  i,
		})
	}

	r.index.Docs = append(r.index.Docs, DocMeta{
		ID:         docID,
		Name:       name,
		Size:       len(text),
		ChunkCount: len(newChunks),
		AddedAt:    time.Now(),
	})
	r.index.Chunks = append(r.index.Chunks, newChunks...)

	if err := r.save(); err != nil {
		return len(newChunks), fmt.Errorf("сохранение индекса: %w", err)
	}
	return len(newChunks), nil
}

// ── Поиск ─────────────────────────────────────────────────────────────────

// SearchResult — один результат поиска.
type SearchResult struct {
	Chunk      Chunk
	Similarity float64
}

// Search находит topK наиболее релевантных чанков для запроса.
func (r *RAG) Search(ctx context.Context, query string, topK int) ([]SearchResult, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.index.Chunks) == 0 {
		return nil, nil
	}

	// Эмбеддируем запрос
	queryEmb, err := r.embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("эмбеддинг запроса: %w", err)
	}

	// Вычисляем cosine similarity для всех чанков
	type scored struct {
		chunk Chunk
		score float64
	}
	results := make([]scored, 0, len(r.index.Chunks))
	for _, c := range r.index.Chunks {
		if len(c.Embedding) == 0 {
			continue
		}
		sim := cosineSimilarity(queryEmb, c.Embedding)
		results = append(results, scored{c, sim})
	}

	// Сортируем по убыванию similarity
	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	if topK > len(results) {
		topK = len(results)
	}

	out := make([]SearchResult, topK)
	for i := 0; i < topK; i++ {
		out[i] = SearchResult{results[i].chunk, results[i].score}
	}
	return out, nil
}

// BuildContextString формирует строку контекста из результатов поиска.
// Эта строка инжектируется в промпт перед сообщением пользователя.
func BuildContextString(results []SearchResult) string {
	if len(results) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[КОНТЕКСТ ИЗ ДОКУМЕНТОВ]\n")
	for i, r := range results {
		if r.Similarity < 0.3 { // отсекаем нерелевантные
			break
		}
		sb.WriteString(fmt.Sprintf("--- Документ: %s (фрагмент %d, схожесть %.0f%%) ---\n",
			r.Chunk.DocName, r.Chunk.ChunkIdx+1, r.Similarity*100))
		sb.WriteString(r.Chunk.Text)
		sb.WriteString("\n")
		if i < len(results)-1 {
			sb.WriteString("\n")
		}
	}
	sb.WriteString("[КОНЕЦ КОНТЕКСТА]\n\nОтвечай на основе контекста выше.")
	return sb.String()
}

// ── Управление документами ────────────────────────────────────────────────

// ListDocs возвращает список всех документов.
func (r *RAG) ListDocs() []DocMeta {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]DocMeta, len(r.index.Docs))
	copy(result, r.index.Docs)
	return result
}

// ListChunks возвращает все чанки (без эмбеддингов для экономии памяти).
// Нужно для BM25-индекса и отладки.
func (r *RAG) ListChunks() []Chunk {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Chunk, len(r.index.Chunks))
	for i, c := range r.index.Chunks {
		// Копируем без эмбеддинга — экономим выделение памяти
		result[i] = Chunk{
			ID:       c.ID,
			DocID:    c.DocID,
			DocName:  c.DocName,
			Text:     c.Text,
			ChunkIdx: c.ChunkIdx,
		}
	}
	return result
}

// DeleteDoc удаляет документ и все его чанки.
func (r *RAG) DeleteDoc(docID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	newDocs := r.index.Docs[:0]
	for _, d := range r.index.Docs {
		if d.ID != docID {
			newDocs = append(newDocs, d)
		}
	}
	newChunks := r.index.Chunks[:0]
	for _, c := range r.index.Chunks {
		if c.DocID != docID {
			newChunks = append(newChunks, c)
		}
	}
	r.index.Docs = newDocs
	r.index.Chunks = newChunks
	return r.save()
}

func (r *RAG) removeDocByName(name string) {
	var keepDocs []DocMeta
	removedIDs := map[string]bool{}
	for _, d := range r.index.Docs {
		if d.Name == name {
			removedIDs[d.ID] = true
		} else {
			keepDocs = append(keepDocs, d)
		}
	}
	if len(removedIDs) == 0 {
		return
	}
	var keepChunks []Chunk
	for _, c := range r.index.Chunks {
		if !removedIDs[c.DocID] {
			keepChunks = append(keepChunks, c)
		}
	}
	r.index.Docs = keepDocs
	r.index.Chunks = keepChunks
}

// ── Эмбеддинги ────────────────────────────────────────────────────────────

type embedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type embedResponse struct {
	Embedding []float64 `json:"embedding"`
}

// embed получает вектор эмбеддинга для текста через Ollama /api/embeddings.
func (r *RAG) embed(ctx context.Context, text string) ([]float64, error) {
	model := r.embedMod
	if model == "" {
		model = "nomic-embed-text" // предпочтительная модель для эмбеддингов
	}

	body, _ := json.Marshal(embedRequest{Model: model, Prompt: text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.client.baseURL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embeddings: %w", err)
	}
	defer resp.Body.Close()

	// Если модель не найдена — пробуем fallback
	if resp.StatusCode == 404 || resp.StatusCode == 400 {
		if model != "nomic-embed-text" {
			return nil, fmt.Errorf("модель эмбеддингов %q недоступна", model)
		}
		// Fallback: используем текущую chat-модель
		return r.embedWithChatModel(ctx, text)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ollama embeddings: статус %d", resp.StatusCode)
	}

	var result embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Embedding, nil
}

// embedWithChatModel — fallback: используем chat-модель для получения эмбеддингов.
// Менее точно но работает с любой моделью Ollama.
func (r *RAG) embedWithChatModel(ctx context.Context, text string) ([]float64, error) {
	// Запрашиваем модель дать краткое представление текста в виде ключевых слов
	// Это псевдо-эмбеддинг: TF-IDF на ключевых словах
	// Используется только если нет нормальной embedding-модели
	return tfidfVector(text), nil
}

// tfidfVector — простой TF-IDF вектор как fallback для эмбеддингов.
// Работает без вызова модели, достаточно для базового поиска.
func tfidfVector(text string) []float64 {
	words := tokenizeWords(strings.ToLower(text))
	freq := make(map[string]int)
	for _, w := range words {
		if len(w) > 2 { // пропускаем очень короткие слова
			freq[w]++
		}
	}

	// Фиксированный размер вектора 256, хеш слов → индекс
	vec := make([]float64, 256)
	for w, cnt := range freq {
		idx := fnv32(w) % 256
		vec[idx] += float64(cnt)
	}
	return normalize(vec)
}

// ── Чанкинг текста ────────────────────────────────────────────────────────

// chunkText разбивает текст на чанки по ~targetWords слов с overlap перекрытием.
func chunkText(text string, targetWords, overlap int) []string {
	// Разбиваем на абзацы сначала
	paragraphs := splitParagraphs(text)

	var chunks []string
	var currentWords []string

	flush := func() {
		if len(currentWords) == 0 {
			return
		}
		chunks = append(chunks, strings.Join(currentWords, " "))
		// Оставляем overlap слов для следующего чанка
		if len(currentWords) > overlap {
			currentWords = currentWords[len(currentWords)-overlap:]
		} else {
			currentWords = currentWords[:0]
		}
	}

	for _, para := range paragraphs {
		words := strings.Fields(para)
		if len(words) == 0 {
			continue
		}

		// Если абзац сам по себе больше target — разбиваем его
		if len(words) > targetWords*2 {
			sentences := splitSentences(para)
			for _, sent := range sentences {
				sentWords := strings.Fields(sent)
				currentWords = append(currentWords, sentWords...)
				if len(currentWords) >= targetWords {
					flush()
				}
			}
		} else {
			currentWords = append(currentWords, words...)
			if len(currentWords) >= targetWords {
				flush()
			}
		}
	}
	flush() // финальный чанк

	return chunks
}

func splitParagraphs(text string) []string {
	parts := strings.Split(text, "\n\n")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func splitSentences(text string) []string {
	var sentences []string
	var sb strings.Builder
	runes := []rune(text)
	for i, r := range runes {
		sb.WriteRune(r)
		if r == '.' || r == '!' || r == '?' {
			// Проверяем что следующий символ — пробел или конец
			if i+1 >= len(runes) || unicode.IsSpace(runes[i+1]) {
				s := strings.TrimSpace(sb.String())
				if s != "" {
					sentences = append(sentences, s)
				}
				sb.Reset()
			}
		}
	}
	if sb.Len() > 0 {
		s := strings.TrimSpace(sb.String())
		if s != "" {
			sentences = append(sentences, s)
		}
	}
	return sentences
}

func tokenizeWords(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// ── Математика ────────────────────────────────────────────────────────────

func cosineSimilarity(a, b []float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return 0
	}
	dot, normA, normB := 0.0, 0.0, 0.0
	for i := 0; i < n; i++ {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func normalize(v []float64) []float64 {
	norm := 0.0
	for _, x := range v {
		norm += x * x
	}
	if norm == 0 {
		return v
	}
	norm = math.Sqrt(norm)
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = x / norm
	}
	return out
}

// fnv32 — быстрый хеш FNV-1a для строки.
func fnv32(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}
