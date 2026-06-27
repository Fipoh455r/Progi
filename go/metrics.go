// metrics.go — Prometheus-совместимые метрики v3.0.
//
// Реализовано на чистом stdlib (без prometheus/client_golang).
// Формат: Prometheus text format 0.0.4 (https://prometheus.io/docs/instrumenting/exposition_formats/)
//
// Endpoint: GET /metrics (публичный — стандартное поведение для Prometheus-скрейпинга).
// Отключить: LOCALAI_METRICS_ENABLED=false
//
// Пример prometheus.yml для скрейпинга:
//
//	scrape_configs:
//	  - job_name: localai
//	    static_configs:
//	      - targets: ['localhost:8080']
package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ── Атомарный счётчик ─────────────────────────────────────────────────────────

type counter struct{ n atomic.Int64 }

func (c *counter) Inc()          { c.n.Add(1) }
func (c *counter) Add(d int64)   { c.n.Add(d) }
func (c *counter) Get() int64    { return c.n.Load() }

// ── Гистограмма (фиксированные le-бакеты) ────────────────────────────────────

var defaultLatencyBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0}

type histogram struct {
	mu      sync.Mutex
	bounds  []float64 // верхние границы бакетов (le)
	counts  []int64   // счётчик попаданий для каждого бакета
	total   int64     // общий count
	sum     float64   // сумма наблюдений
}

func newHistogram(bounds []float64) *histogram {
	return &histogram{
		bounds: bounds,
		counts: make([]int64, len(bounds)),
	}
}

// Observe добавляет одно наблюдение.
func (h *histogram) Observe(v float64) {
	h.mu.Lock()
	for i, b := range h.bounds {
		if v <= b {
			h.counts[i]++
		}
	}
	h.total++
	h.sum += v
	h.mu.Unlock()
}

// snapshot возвращает копию состояния гистограммы (без блокировки вызывающего).
func (h *histogram) snapshot() (counts []int64, total int64, sum float64) {
	h.mu.Lock()
	counts = make([]int64, len(h.counts))
	copy(counts, h.counts)
	total = h.total
	sum = h.sum
	h.mu.Unlock()
	return
}

// ── Metrics ───────────────────────────────────────────────────────────────────

// Metrics — глобальный сборщик метрик. Инициализируется один раз в runServer.
type Metrics struct {
	startTime time.Time
	balancer  *OllamaBalancer // для метрик нод (может быть nil)

	// ── Counters ──────────────────────────────────────────────────────────
	ChatReqs   counter // localai_chat_requests_total
	AgentReqs  counter // localai_agent_requests_total
	ChatErrs   counter // localai_chat_errors_total
	AgentErrs  counter // localai_agent_errors_total
	Tokens     counter // localai_tokens_generated_total
	Uploads    counter // localai_uploads_total
	UploadErrs counter // localai_upload_errors_total
	LoginOK    counter // localai_auth_logins_total
	LoginFail  counter // localai_auth_failures_total
	AuthFailed counter // localai_auth_unauthorized_total (401/403)

	// ── Gauges ────────────────────────────────────────────────────────────
	Active atomic.Int64 // localai_active_requests

	// ── Histograms ────────────────────────────────────────────────────────
	ChatDuration  *histogram // localai_chat_duration_seconds
	AgentDuration *histogram // localai_agent_duration_seconds
}

// NewMetrics создаёт и возвращает новый Metrics.
func NewMetrics(balancer *OllamaBalancer) *Metrics {
	return &Metrics{
		startTime:     time.Now(),
		balancer:      balancer,
		ChatDuration:  newHistogram(defaultLatencyBuckets),
		AgentDuration: newHistogram(defaultLatencyBuckets),
	}
}

// ActiveStart помечает начало активного запроса.
func (m *Metrics) ActiveStart() { m.Active.Add(1) }

// ActiveDone помечает завершение активного запроса.
func (m *Metrics) ActiveDone() { m.Active.Add(-1) }

// ── Рендер в Prometheus text format ──────────────────────────────────────────

// ServeHTTP отдаёт метрики в формате Prometheus text 0.0.4.
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", 405)
		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	var sb strings.Builder

	writeCounter := func(name, help string, value int64) {
		fmt.Fprintf(&sb, "# HELP %s %s\n# TYPE %s counter\n%s %d\n",
			name, help, name, name, value)
	}
	writeGauge := func(name, help string, value float64) {
		fmt.Fprintf(&sb, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n",
			name, help, name, name, value)
	}
	writeHistogram := func(name, help string, h *histogram) {
		counts, total, sum := h.snapshot()
		fmt.Fprintf(&sb, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
		cumulative := int64(0)
		for i, le := range h.bounds {
			cumulative += counts[i]
			fmt.Fprintf(&sb, "%s_bucket{le=\"%g\"} %d\n", name, le, cumulative)
		}
		fmt.Fprintf(&sb, "%s_bucket{le=\"+Inf\"} %d\n", name, total)
		fmt.Fprintf(&sb, "%s_sum %g\n", name, sum)
		fmt.Fprintf(&sb, "%s_count %d\n", name, total)
	}

	// Версия
	fmt.Fprintf(&sb, "# HELP localai_build_info Build information\n")
	fmt.Fprintf(&sb, "# TYPE localai_build_info gauge\n")
	fmt.Fprintf(&sb, "localai_build_info{version=\"3.0\"} 1\n")

	// Uptime
	writeGauge("localai_uptime_seconds",
		"Seconds since server start",
		time.Since(m.startTime).Seconds())

	// Запросы
	writeCounter("localai_chat_requests_total", "Total chat requests", m.ChatReqs.Get())
	writeCounter("localai_agent_requests_total", "Total agent requests", m.AgentReqs.Get())
	writeCounter("localai_chat_errors_total", "Total chat errors", m.ChatErrs.Get())
	writeCounter("localai_agent_errors_total", "Total agent errors", m.AgentErrs.Get())

	// Токены
	writeCounter("localai_tokens_generated_total",
		"Approximate total tokens generated", m.Tokens.Get())

	// Загрузка файлов
	writeCounter("localai_uploads_total", "Total file uploads", m.Uploads.Get())
	writeCounter("localai_upload_errors_total", "Total upload errors", m.UploadErrs.Get())

	// Авторизация
	writeCounter("localai_auth_logins_total", "Successful logins", m.LoginOK.Get())
	writeCounter("localai_auth_failures_total", "Failed login attempts", m.LoginFail.Get())
	writeCounter("localai_auth_unauthorized_total", "Unauthorized requests (401/403)", m.AuthFailed.Get())

	// Активные запросы (gauge)
	writeGauge("localai_active_requests",
		"Currently active (streaming) requests",
		float64(m.Active.Load()))

	// Ноды Ollama
	if m.balancer != nil {
		writeGauge("localai_ollama_nodes_total",
			"Total configured Ollama nodes",
			float64(m.balancer.TotalCount()))
		writeGauge("localai_ollama_nodes_healthy",
			"Healthy Ollama nodes",
			float64(m.balancer.HealthyCount()))
	}

	// Гистограммы латентности
	writeHistogram("localai_chat_duration_seconds",
		"Chat request duration in seconds", m.ChatDuration)
	writeHistogram("localai_agent_duration_seconds",
		"Agent request duration in seconds", m.AgentDuration)

	w.Write([]byte(sb.String()))
}

// ── Middleware ────────────────────────────────────────────────────────────────

// TrackActive возвращает middleware, который инкрементирует Active при каждом запросе.
func (m *Metrics) TrackActive(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.ActiveStart()
		defer m.ActiveDone()
		next.ServeHTTP(w, r)
	})
}

// ── Регистрация endpoint ──────────────────────────────────────────────────────

// registerMetricsRoute регистрирует /metrics endpoint.
// Включён по умолчанию; отключается через LOCALAI_METRICS_ENABLED=false.
func registerMetricsRoute(mux *http.ServeMux, m *Metrics) {
	if os.Getenv("LOCALAI_METRICS_ENABLED") == "false" {
		return
	}
	mux.Handle("/metrics", m)
}
