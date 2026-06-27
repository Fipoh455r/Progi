// token_stats.go — глобальная статистика экономии токенов.
//
// Отслеживает три источника экономии:
//  1. Сжатие истории (CompressHistory) — суммаризация старых сообщений.
//  2. Умный контекст (SmartContext)    — фильтрация нерелевантных сообщений.
//  3. Компактные шаблоны (templates)   — замена длинных system prompt короткими.
//
// Статистика накапливается в памяти и периодически сбрасывается на диск
// (data/token_stats.json). При перезапуске сервера загружается с диска.
//
// API: GET /api/token-stats
package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// TokenSavingsStats — публичная структура для JSON-ответа API.
type TokenSavingsStats struct {
	// Сжатие истории
	CompressionEvents  int64 `json:"compression_events"`  // сколько раз сжималась история
	CompressionBefore  int64 `json:"compression_before"`  // токенов до сжатия (сумма)
	CompressionAfter   int64 `json:"compression_after"`   // токенов после сжатия (сумма)
	CompressionSaved   int64 `json:"compression_saved"`   // разница (сумма)

	// Умный контекст (FilterByRelevance / SmartContext)
	ContextFilterEvents int64 `json:"context_filter_events"` // сколько раз применялся фильтр
	ContextBefore       int64 `json:"context_before"`
	ContextAfter        int64 `json:"context_after"`
	ContextSaved        int64 `json:"context_saved"`

	// Шаблоны (ApplyTemplate)
	TemplateEvents int64 `json:"template_events"` // сколько раз применялся шаблон
	TemplatesSaved int64 `json:"templates_saved"` // токенов сэкономлено vs default system prompt

	// Итого
	TotalSaved   int64  `json:"total_saved"`    // сумма всех источников
	TotalSavedKB string `json:"total_saved_kb"` // красивое представление

	// Мета
	Since    string `json:"since"`     // когда начали считать (RFC3339)
	UpdatedAt string `json:"updated_at"` // последнее обновление
}

// tokenStatsState — внутреннее состояние (атомарные счётчики).
type tokenStatsState struct {
	CompressionEvents  atomic.Int64
	CompressionBefore  atomic.Int64
	CompressionAfter   atomic.Int64

	ContextFilterEvents atomic.Int64
	ContextBefore       atomic.Int64
	ContextAfter        atomic.Int64

	TemplateEvents atomic.Int64
	TemplatesSaved atomic.Int64

	since     time.Time
	mu        sync.Mutex // только для сброса на диск
	statsPath string
}

// persistedStats — структура для JSON-сериализации на диск.
type persistedStats struct {
	CompressionEvents   int64  `json:"ce"`
	CompressionBefore   int64  `json:"cb"`
	CompressionAfter    int64  `json:"ca"`
	ContextFilterEvents int64  `json:"cfe"`
	ContextBefore       int64  `json:"ctxb"`
	ContextAfter        int64  `json:"ctxa"`
	TemplateEvents      int64  `json:"te"`
	TemplatesSaved      int64  `json:"ts"`
	Since               string `json:"since"`
}

var globalTokenStats = &tokenStatsState{
	since: time.Now(),
}

// InitTokenStats инициализирует глобальную статистику: загружает с диска и запускает
// фоновую горутину автосохранения каждые 5 минут.
func InitTokenStats(dataDir string) {
	globalTokenStats.statsPath = filepath.Join(dataDir, "token_stats.json")
	globalTokenStats.since = time.Now()

	// Загружаем сохранённое состояние
	if data, err := os.ReadFile(globalTokenStats.statsPath); err == nil {
		var p persistedStats
		if json.Unmarshal(data, &p) == nil {
			globalTokenStats.CompressionEvents.Store(p.CompressionEvents)
			globalTokenStats.CompressionBefore.Store(p.CompressionBefore)
			globalTokenStats.CompressionAfter.Store(p.CompressionAfter)
			globalTokenStats.ContextFilterEvents.Store(p.ContextFilterEvents)
			globalTokenStats.ContextBefore.Store(p.ContextBefore)
			globalTokenStats.ContextAfter.Store(p.ContextAfter)
			globalTokenStats.TemplateEvents.Store(p.TemplateEvents)
			globalTokenStats.TemplatesSaved.Store(p.TemplatesSaved)
			if t, err := time.Parse(time.RFC3339, p.Since); err == nil {
				globalTokenStats.since = t
			}
		}
	}

	// Фоновая горутина: сохраняем каждые 5 минут
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			if err := globalTokenStats.flush(); err != nil {
				log.Printf("[token_stats] ошибка сохранения: %v", err)
			}
		}
	}()
}

// RecordCompression регистрирует одно событие сжатия истории.
// before/after — оценка токенов до и после сжатия.
func RecordCompression(before, after int) {
	if before <= after {
		return // нет экономии — не считаем
	}
	globalTokenStats.CompressionEvents.Add(1)
	globalTokenStats.CompressionBefore.Add(int64(before))
	globalTokenStats.CompressionAfter.Add(int64(after))
}

// RecordContextFilter регистрирует одно событие фильтрации контекста.
func RecordContextFilter(before, after int) {
	if before <= after {
		return
	}
	globalTokenStats.ContextFilterEvents.Add(1)
	globalTokenStats.ContextBefore.Add(int64(before))
	globalTokenStats.ContextAfter.Add(int64(after))
}

// RecordTemplateUsage регистрирует применение компактного шаблона.
// saved — сколько токенов сэкономили vs обычного system prompt (>0).
func RecordTemplateUsage(saved int) {
	if saved <= 0 {
		return
	}
	globalTokenStats.TemplateEvents.Add(1)
	globalTokenStats.TemplatesSaved.Add(int64(saved))
}

// GetTokenStats возвращает текущую статистику для API.
func GetTokenStats() TokenSavingsStats {
	ce := globalTokenStats.CompressionEvents.Load()
	cb := globalTokenStats.CompressionBefore.Load()
	ca := globalTokenStats.CompressionAfter.Load()
	cSaved := cb - ca

	cfe := globalTokenStats.ContextFilterEvents.Load()
	ctxb := globalTokenStats.ContextBefore.Load()
	ctxa := globalTokenStats.ContextAfter.Load()
	ctxSaved := ctxb - ctxa

	te := globalTokenStats.TemplateEvents.Load()
	ts := globalTokenStats.TemplatesSaved.Load()

	total := cSaved + ctxSaved + ts

	// Примерный перевод токенов в KB текста (1 токен ≈ 4 байта)
	totalKB := float64(total) * 4 / 1024
	savedKBStr := ""
	if totalKB < 1 {
		savedKBStr = "< 1 KB"
	} else {
		savedKBStr = formatKB(totalKB)
	}

	return TokenSavingsStats{
		CompressionEvents:   ce,
		CompressionBefore:   cb,
		CompressionAfter:    ca,
		CompressionSaved:    cSaved,
		ContextFilterEvents: cfe,
		ContextBefore:       ctxb,
		ContextAfter:        ctxa,
		ContextSaved:        ctxSaved,
		TemplateEvents:      te,
		TemplatesSaved:      ts,
		TotalSaved:          total,
		TotalSavedKB:        savedKBStr,
		Since:               globalTokenStats.since.Format(time.RFC3339),
		UpdatedAt:           time.Now().Format(time.RFC3339),
	}
}

// flush сохраняет текущее состояние на диск.
func (s *tokenStatsState) flush() error {
	if s.statsPath == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	p := persistedStats{
		CompressionEvents:   s.CompressionEvents.Load(),
		CompressionBefore:   s.CompressionBefore.Load(),
		CompressionAfter:    s.CompressionAfter.Load(),
		ContextFilterEvents: s.ContextFilterEvents.Load(),
		ContextBefore:       s.ContextBefore.Load(),
		ContextAfter:        s.ContextAfter.Load(),
		TemplateEvents:      s.TemplateEvents.Load(),
		TemplatesSaved:      s.TemplatesSaved.Load(),
		Since:               s.since.Format(time.RFC3339),
	}

	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(s.statsPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.statsPath, data, 0o644)
}

// formatKB форматирует KB в читаемый вид.
func formatKB(kb float64) string {
	if kb >= 1024 {
		return formatFloat(kb/1024) + " MB"
	}
	return formatFloat(kb) + " KB"
}

// formatFloat форматирует float без trailing zeros (1.0 → "1", 1.5 → "1.5").
func formatFloat(f float64) string {
	s := ""
	intPart := int(f)
	frac := f - float64(intPart)
	s += itoa(intPart)
	if frac >= 0.05 {
		s += "." + itoa(int(frac*10))
	}
	return s
}

// itoa — простая int-to-string без импорта strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 10)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}
