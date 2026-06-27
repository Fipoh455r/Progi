// semantic_cache.go — семантический кэш LLM-ответов на базе embedding-similarity.
//
// Отличие от LLM-кэша в cache.go:
//   cache.go       → точное совпадение SHA256(model+temp+messages) — очень узко
//   semantic_cache → косинусное расстояние между эмбеддингами запросов
//
// Пример: запросы "что такое Go?" и "расскажи о языке программирования Go"
// имеют cosine similarity ~0.96 → кэш-хит, LLM не вызывается.
//
// Алгоритм:
//  1. Вычисляем эмбеддинг нового запроса (nomic-embed-text или любая embedding-модель).
//  2. Перебираем записи кэша, ищем максимальное cosine similarity.
//  3. Если max_sim >= threshold (по умолчанию 0.92) → возвращаем кэшированный ответ.
//  4. Иначе → кэш-промах. После получения ответа от LLM — асинхронно сохраняем.
//
// Конфигурация через переменные окружения:
//   LOCALAI_SEMANTIC_CACHE=true              — включить (по умолчанию выключен)
//   LOCALAI_SEMANTIC_THRESHOLD=0.92          — порог similarity [0, 1]
//   LOCALAI_SEMANTIC_CACHE_SIZE=500          — макс записей (LRU eviction)
//
// API:
//   GET /api/semantic-cache/stats
//   DELETE /api/semantic-cache   — очистить кэш
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ── Константы ─────────────────────────────────────────────────────────────────

const (
	// scDefaultThreshold — минимальное cosine similarity для cache hit.
	// 0.92 = очень похожие запросы; 0.85 = более широкий поиск.
	scDefaultThreshold = 0.92

	// scDefaultMaxSize — максимум записей в кэше (LRU eviction сверх этого).
	scDefaultMaxSize = 500

	// scEmbedTimeout — тайм-аут на вычисление одного эмбеддинга.
	scEmbedTimeout = 12 * time.Second

	// scFlushInterval — как часто сбрасываем кэш на диск.
	scFlushInterval = 5 * time.Minute

	// scStoreTimeout — тайм-аут для асинхронного сохранения записи.
	scStoreTimeout = 20 * time.Second
)

// ── Типы ──────────────────────────────────────────────────────────────────────

// scEntry — одна запись семантического кэша.
type scEntry struct {
	ID        string    `json:"id"`
	Query     string    `json:"query"`
	Response  string    `json:"response"`
	Embedding []float64 `json:"embedding"`
	Hits      int64     `json:"hits"`
	CreatedAt time.Time `json:"created_at"`
	LastHit   time.Time `json:"last_hit"`
}

// SemanticCacheStats — статистика для /api/semantic-cache/stats.
type SemanticCacheStats struct {
	Enabled   bool    `json:"enabled"`
	Entries   int     `json:"entries"`
	Hits      int64   `json:"hits"`
	Misses    int64   `json:"misses"`
	HitRate   string  `json:"hit_rate"`
	Threshold float64 `json:"threshold"`
	MaxSize   int     `json:"max_size"`
	EmbedMod  string  `json:"embed_model"`
}

// SemanticCache — потокобезопасный семантический кэш.
type SemanticCache struct {
	mu        sync.RWMutex
	path      string // data/semantic_cache.json
	entries   []*scEntry
	maxSize   int
	threshold float64
	client    *OllamaClient
	embedMod  string
	hits      atomic.Int64
	misses    atomic.Int64
}

// globalSemanticCache — глобальный экземпляр (nil если отключён).
var globalSemanticCache *SemanticCache

// ── Инициализация ──────────────────────────────────────────────────────────────

// InitSemanticCache инициализирует глобальный семантический кэш.
// Загружает сохранённые записи с диска и запускает фоновый flush.
// threshold ∈ (0, 1]: значение 0 → используется scDefaultThreshold.
// maxSize ≤ 0 → используется scDefaultMaxSize.
func InitSemanticCache(dataDir string, client *OllamaClient, embedMod string, threshold float64, maxSize int) error {
	if threshold <= 0 || threshold > 1 {
		threshold = scDefaultThreshold
	}
	if maxSize <= 0 {
		maxSize = scDefaultMaxSize
	}
	if embedMod == "" {
		embedMod = "nomic-embed-text"
	}

	sc := &SemanticCache{
		path:      filepath.Join(dataDir, "semantic_cache.json"),
		maxSize:   maxSize,
		threshold: threshold,
		client:    client,
		embedMod:  embedMod,
	}
	sc.load() // ошибку при первом запуске игнорируем

	// Фоновый flush каждые 5 минут
	go func() {
		ticker := time.NewTicker(scFlushInterval)
		defer ticker.Stop()
		for range ticker.C {
			if err := sc.flush(); err != nil {
				log.Printf("[semantic_cache] flush error: %v", err)
			}
		}
	}()

	globalSemanticCache = sc
	return nil
}

// ── Основные операции ─────────────────────────────────────────────────────────

// Lookup ищет семантически близкий запрос в кэше.
// Возвращает (response, hit, error).
// При ошибке эмбеддинга — деградирует (miss, nil error) чтобы не блокировать чат.
func (sc *SemanticCache) Lookup(ctx context.Context, query string) (string, bool, error) {
	embedCtx, cancel := context.WithTimeout(ctx, scEmbedTimeout)
	defer cancel()

	emb, err := sc.client.EmbedText(embedCtx, sc.embedMod, query)
	if err != nil {
		// Деградация: embedding недоступен → пропускаем кэш, не возвращаем ошибку
		sc.misses.Add(1)
		return "", false, nil
	}

	sc.mu.Lock() // Write lock: обновляем Hits и LastHit при cache hit
	defer sc.mu.Unlock()

	var bestEntry *scEntry
	bestSim := 0.0

	for _, e := range sc.entries {
		if len(e.Embedding) == 0 {
			continue
		}
		sim := cosineSimilarity(emb, e.Embedding) // из rag.go (package-level)
		if sim > bestSim {
			bestSim = sim
			bestEntry = e
		}
	}

	if bestEntry != nil && bestSim >= sc.threshold {
		sc.hits.Add(1)
		bestEntry.Hits++
		bestEntry.LastHit = time.Now()
		return bestEntry.Response, true, nil
	}

	sc.misses.Add(1)
	return "", false, nil
}

// StoreAsync сохраняет пару (query, response) в кэш асинхронно.
// Не блокирует ответ пользователю — вычисление эмбеддинга идёт в фоне.
func (sc *SemanticCache) StoreAsync(query, response string) {
	go func() {
		storeCtx, cancel := context.WithTimeout(context.Background(), scStoreTimeout)
		defer cancel()
		if err := sc.store(storeCtx, query, response); err != nil {
			// Не критично: кэш — оптимизация, не основная функция
			log.Printf("[semantic_cache] store: %v", err)
		}
	}()
}

// store — внутренний синхронный вариант сохранения.
func (sc *SemanticCache) store(ctx context.Context, query, response string) error {
	emb, err := sc.client.EmbedText(ctx, sc.embedMod, query)
	if err != nil {
		return fmt.Errorf("embed: %w", err)
	}

	entry := &scEntry{
		ID:        scMakeID(),
		Query:     query,
		Response:  response,
		Embedding: emb,
		CreatedAt: time.Now(),
		LastHit:   time.Now(),
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()

	// LRU eviction: если переполнено — удаляем запись с наиболее давним LastHit
	if len(sc.entries) >= sc.maxSize {
		sc.evictOldest()
	}
	sc.entries = append(sc.entries, entry)
	return nil
}

// Clear удаляет все записи кэша и сбрасывает счётчики.
func (sc *SemanticCache) Clear() {
	sc.mu.Lock()
	sc.entries = nil
	sc.mu.Unlock()
	sc.hits.Store(0)
	sc.misses.Store(0)
	_ = sc.flush()
}

// Stats возвращает текущую статистику.
func (sc *SemanticCache) Stats() SemanticCacheStats {
	sc.mu.RLock()
	entries := len(sc.entries)
	sc.mu.RUnlock()

	hits := sc.hits.Load()
	misses := sc.misses.Load()
	total := hits + misses

	rate := "0%"
	if total > 0 {
		rate = fmt.Sprintf("%.1f%%", float64(hits)/float64(total)*100)
	}

	return SemanticCacheStats{
		Enabled:   true,
		Entries:   entries,
		Hits:      hits,
		Misses:    misses,
		HitRate:   rate,
		Threshold: sc.threshold,
		MaxSize:   sc.maxSize,
		EmbedMod:  sc.embedMod,
	}
}

// ── Персистентность ───────────────────────────────────────────────────────────

// load читает записи из JSON-файла на диске.
func (sc *SemanticCache) load() {
	data, err := os.ReadFile(sc.path)
	if err != nil {
		return // нормально для первого запуска
	}
	var entries []*scEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		log.Printf("[semantic_cache] load corrupt: %v", err)
		return
	}
	sc.mu.Lock()
	sc.entries = entries
	sc.mu.Unlock()
}

// flush сохраняет записи на диск (без эмбеддингов, только query+response+meta).
// Эмбеддинги не сохраняем: они занимают много места (~3KB/entry при 768d)
// и будут пересчитаны при следующем попадании — но это слишком медленно.
// Компромисс: сохраняем эмбеддинги — данные (~1.5MB для 500 записей), но ускорение огромное.
func (sc *SemanticCache) flush() error {
	sc.mu.RLock()
	entries := make([]*scEntry, len(sc.entries))
	copy(entries, sc.entries)
	sc.mu.RUnlock()

	if err := os.MkdirAll(filepath.Dir(sc.path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return os.WriteFile(sc.path, data, 0o644)
}

// ── Вспомогательные ───────────────────────────────────────────────────────────

// evictOldest удаляет запись с наиболее давним LastHit (LRU).
// Вызывается под write-lock.
func (sc *SemanticCache) evictOldest() {
	if len(sc.entries) == 0 {
		return
	}
	// Находим индекс с минимальным LastHit
	oldest := 0
	for i, e := range sc.entries {
		if e.LastHit.Before(sc.entries[oldest].LastHit) {
			oldest = i
		}
	}
	// Удаляем элемент
	sc.entries = append(sc.entries[:oldest], sc.entries[oldest+1:]...)
}

// scMakeID генерирует уникальный ID для записи кэша.
func scMakeID() string {
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

// scParseThreshold парсит float из строки с дефолтом.
func scParseThreshold(s string, def float64) float64 {
	if s == "" {
		return def
	}
	var f float64
	if _, err := fmt.Sscanf(s, "%f", &f); err != nil || f <= 0 || f > 1 {
		return def
	}
	return f
}

// scParseMaxSize парсит int из строки с дефолтом.
func scParseMaxSize(s string, def int) int {
	if s == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
		return def
	}
	return n
}

// SemanticCacheDisabledStats возвращает статистику когда кэш отключён.
func SemanticCacheDisabledStats() SemanticCacheStats {
	return SemanticCacheStats{
		Enabled:   false,
		Threshold: scDefaultThreshold,
		MaxSize:   scDefaultMaxSize,
	}
}

// TopSemanticEntries возвращает до n записей отсортированных по кол-ву хитов (для отладки).
func (sc *SemanticCache) TopEntries(n int) []scEntryPreview {
	sc.mu.RLock()
	cp := make([]*scEntry, len(sc.entries))
	copy(cp, sc.entries)
	sc.mu.RUnlock()

	sort.Slice(cp, func(i, j int) bool {
		return cp[i].Hits > cp[j].Hits
	})

	if n > len(cp) {
		n = len(cp)
	}
	result := make([]scEntryPreview, n)
	for i := 0; i < n; i++ {
		e := cp[i]
		q := e.Query
		if len([]rune(q)) > 80 {
			q = string([]rune(q)[:77]) + "…"
		}
		result[i] = scEntryPreview{
			ID:        e.ID,
			Query:     q,
			Hits:      e.Hits,
			CreatedAt: e.CreatedAt,
			LastHit:   e.LastHit,
		}
	}
	return result
}

// scEntryPreview — краткое представление записи без эмбеддинга и полного ответа.
type scEntryPreview struct {
	ID        string    `json:"id"`
	Query     string    `json:"query"`
	Hits      int64     `json:"hits"`
	CreatedAt time.Time `json:"created_at"`
	LastHit   time.Time `json:"last_hit"`
}
