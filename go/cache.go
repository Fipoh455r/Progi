// cache.go — кэш ответов LLM на диск (stdlib only).
//
// Принцип:
//   - Ключ кэша = SHA256(model + temp + все сообщения) — детерминированный хэш
//   - Запись хранится в data/cache/<2 hex>/<sha256>.json (шардирование папок)
//   - TTL настраивается (0 = не истекает)
//   - Фоновая горутина удаляет устаревшие записи раз в час
//
// Экономия токенов:
//   - Повторный идентичный запрос → 0 вызовов LLM, 0 токенов
//   - При агентном пайплайне подзадачи кэшируются автоматически
//   - При cache hit: задержка ~1мс вместо ~5–30с у LLM
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// cacheEntry — одна запись кэша.
type cacheEntry struct {
	Key       string    `json:"key"`
	Response  string    `json:"response"`
	Model     string    `json:"model"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"` // нулевое время = не истекает
	HitCount  int       `json:"hit_count"`  // сколько раз использовалась запись
}

// IsExpired возвращает true если запись устарела.
func (e *cacheEntry) IsExpired() bool {
	if e.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().After(e.ExpiresAt)
}

// LLMCache — дисковый кэш ответов языковой модели.
type LLMCache struct {
	dir  string
	ttl  time.Duration
	mu   sync.RWMutex
	hits int64  // статистика попаданий
	miss int64  // статистика промахов
	stop chan struct{}
}

// globalCache — глобальный экземпляр кэша (nil = кэш отключён).
var globalCache *LLMCache

// InitCache создаёт и запускает кэш. Если ttl=0 — записи не истекают.
// Вызывается из runServer.
func InitCache(dataDir string, ttl time.Duration) error {
	cacheDir := filepath.Join(dataDir, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fmt.Errorf("создание директории кэша: %w", err)
	}

	c := &LLMCache{
		dir:  cacheDir,
		ttl:  ttl,
		stop: make(chan struct{}),
	}

	// Фоновая горутина: чистка устаревших записей раз в час
	go c.pruneLoop()

	globalCache = c
	log.Printf("[cache] инициализирован: %s (TTL=%v)", cacheDir, ttl)
	return nil
}

// CacheKey вычисляет детерминированный SHA256-ключ для запроса к LLM.
// Учитывает: модель, температуру (до 1 знака), содержимое всех сообщений.
func CacheKey(model string, temp float64, messages []Message) string {
	h := sha256.New()

	// Нормализуем температуру до 1 знака (0.71 и 0.70 — один кэш)
	tempStr := fmt.Sprintf("%.1f", temp)

	fmt.Fprintf(h, "model:%s\ntemp:%s\n", model, tempStr)
	for _, m := range messages {
		// Разделитель гарантирует что конкатенация разных сообщений разная
		fmt.Fprintf(h, "%s\x00%s\x01", m.Role, m.Content)
	}

	return hex.EncodeToString(h.Sum(nil))
}

// Get возвращает кэшированный ответ. ok=false если не найден или устарел.
func (c *LLMCache) Get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, err := c.loadEntry(key)
	if err != nil || entry == nil {
		c.miss++
		return "", false
	}

	if entry.IsExpired() {
		c.miss++
		return "", false
	}

	c.hits++
	return entry.Response, true
}

// Set сохраняет ответ в кэш под заданным ключом.
func (c *LLMCache) Set(key, response, model string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry := &cacheEntry{
		Key:       key,
		Response:  response,
		Model:     model,
		CreatedAt: time.Now(),
	}
	if c.ttl > 0 {
		entry.ExpiresAt = time.Now().Add(c.ttl)
	}

	if err := c.saveEntry(entry); err != nil {
		log.Printf("[cache] ошибка записи %s: %v", key[:8], err)
	}
}

// Stats возвращает статистику кэша.
func (c *LLMCache) Stats() (hits, misses int64, entries int) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	hits = c.hits
	misses = c.miss

	// Считаем файлы в директории кэша
	_ = filepath.Walk(c.dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(path, ".json") {
			entries++
		}
		return nil
	})
	return
}

// Close останавливает фоновую горутину.
func (c *LLMCache) Close() {
	close(c.stop)
}

// ── Внутренние методы ──────────────────────────────────────────────────────

// entryPath возвращает путь к файлу записи.
// Шардирование по первым 2 символам хэша (как в git objects).
func (c *LLMCache) entryPath(key string) string {
	if len(key) < 4 {
		return filepath.Join(c.dir, "misc", key+".json")
	}
	return filepath.Join(c.dir, key[:2], key+".json")
}

// loadEntry читает запись с диска. Возвращает nil если файл не существует.
func (c *LLMCache) loadEntry(key string) (*cacheEntry, error) {
	path := c.entryPath(key)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, err
	}
	return &entry, nil
}

// saveEntry записывает запись на диск (атомарно через temp-файл).
func (c *LLMCache) saveEntry(entry *cacheEntry) error {
	path := c.entryPath(entry.Key)

	// Создаём шард-директорию
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// pruneLoop периодически удаляет устаревшие записи.
func (c *LLMCache) pruneLoop() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.prune()
		case <-c.stop:
			return
		}
	}
}

// prune удаляет устаревшие записи кэша.
func (c *LLMCache) prune() {
	if c.ttl == 0 {
		return // записи бессрочные — нечего удалять
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	removed := 0
	_ = filepath.Walk(c.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var entry cacheEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			// Повреждённый файл — удаляем
			os.Remove(path)
			removed++
			return nil
		}

		if entry.IsExpired() {
			os.Remove(path)
			removed++
		}
		return nil
	})

	if removed > 0 {
		log.Printf("[cache] очистка: удалено %d устаревших записей", removed)
	}
}

// ── Интеграция с OllamaClient ──────────────────────────────────────────────

// CachedChat выполняет запрос к модели с кэшированием.
// Если globalCache == nil — делает обычный запрос без кэша.
// Возвращает (ответ, был_ли_кэш_хит, ошибка).
func CachedChat(
	ctx context.Context,
	client *OllamaClient,
	messages []Message,
	model string,
	temp float64,
) (string, bool, error) {
	if globalCache == nil {
		resp, err := collectStream(ctx, client, messages, model, temp)
		return resp, false, err
	}

	key := CacheKey(model, temp, messages)

	// Проверяем кэш
	if cached, ok := globalCache.Get(key); ok {
		return cached, true, nil
	}

	// Запрос к LLM
	resp, err := collectStream(ctx, client, messages, model, temp)
	if err != nil {
		return "", false, err
	}

	// Сохраняем только успешные непустые ответы
	if resp != "" {
		globalCache.Set(key, resp, model)
	}

	return resp, false, nil
}
