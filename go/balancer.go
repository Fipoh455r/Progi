// balancer.go — балансировщик нагрузки для нескольких Ollama-нод v3.0.
//
// Конфигурация (через переменные окружения):
//
//	LOCALAI_OLLAMA_NODES — список URL нод через запятую:
//	  http://node1:11434,http://node2:11434,http://node3:11434
//	  Если не задана — используется единственная нода ollamaURL из -ollama флага.
//
// Алгоритм:
//   - Round-robin среди здоровых нод (без весов, все равны).
//   - Фоновый health check каждые 15 секунд.
//   - Circuit breaker: нода считается нездоровой после 3 последовательных ошибок.
//   - Автовосстановление: нода повторно проверяется через 60 секунд.
//   - Pick() возвращает следующую здоровую ноду или primary если все упали.
package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// Нода считается недоступной после этого числа последовательных ошибок.
	circuitBreakerThreshold = 3
	// Интервал фонового health check.
	healthCheckInterval = 15 * time.Second
	// Нода недоступна: пробуем снова через этот интервал.
	recoveryInterval = 60 * time.Second
)

// balancerNode — одна нода в пуле балансировщика.
type balancerNode struct {
	client     *OllamaClient
	url        string
	mu         sync.RWMutex
	healthy    bool
	failures   int
	lastFail   time.Time
}

// markSuccess сбрасывает счётчик ошибок.
func (n *balancerNode) markSuccess() {
	n.mu.Lock()
	n.failures = 0
	n.healthy = true
	n.mu.Unlock()
}

// markFailure инкрементирует счётчик ошибок; отключает ноду при достижении порога.
func (n *balancerNode) markFailure() {
	n.mu.Lock()
	n.failures++
	if n.failures >= circuitBreakerThreshold {
		n.healthy = false
		n.lastFail = time.Now()
	}
	n.mu.Unlock()
}

// isHealthy возвращает true если нода активна.
// Нода, упавшая давно (> recoveryInterval), временно считается «вероятно живой»
// чтобы дать ей шанс на восстановление.
func (n *balancerNode) isHealthy() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.healthy {
		return true
	}
	// Через recoveryInterval — попробуем снова
	return time.Since(n.lastFail) > recoveryInterval
}

// OllamaBalancer — пул нод с round-robin балансировкой.
type OllamaBalancer struct {
	nodes   []*balancerNode
	counter atomic.Uint64
}

// NewOllamaBalancer создаёт балансировщик.
// primaryURL используется если LOCALAI_OLLAMA_NODES не задана.
func NewOllamaBalancer(primaryURL string) *OllamaBalancer {
	// Читаем список нод из окружения
	rawNodes := os.Getenv("LOCALAI_OLLAMA_NODES")

	var urls []string
	if rawNodes != "" {
		for _, u := range strings.Split(rawNodes, ",") {
			u = strings.TrimSpace(u)
			if u != "" {
				urls = append(urls, u)
			}
		}
	}
	// Всегда включаем primary (если его ещё нет в списке)
	found := false
	for _, u := range urls {
		if u == primaryURL {
			found = true
			break
		}
	}
	if !found {
		urls = append([]string{primaryURL}, urls...)
	}

	nodes := make([]*balancerNode, 0, len(urls))
	for _, u := range urls {
		nodes = append(nodes, &balancerNode{
			client:  NewOllamaClient(u),
			url:     u,
			healthy: true,
		})
	}

	b := &OllamaBalancer{nodes: nodes}
	go b.healthCheckLoop()

	if len(nodes) > 1 {
		log.Printf("[balancer] запущен с %d нодами: %s", len(nodes), strings.Join(urls, ", "))
	}
	return b
}

// Pick выбирает следующую здоровую ноду (round-robin).
// Если нет ни одной здоровой — возвращает primary (первую) ноду как fallback.
func (b *OllamaBalancer) Pick() *OllamaClient {
	if len(b.nodes) == 1 {
		return b.nodes[0].client
	}

	total := uint64(len(b.nodes))
	for range b.nodes {
		idx := b.counter.Add(1) % total
		n := b.nodes[idx]
		if n.isHealthy() {
			return n.client
		}
	}
	// Все ноды нездоровы — fallback на primary
	return b.nodes[0].client
}

// Primary возвращает первую ноду (для инициализации RAG, проверок и т.п.).
func (b *OllamaBalancer) Primary() *OllamaClient {
	return b.nodes[0].client
}

// HealthyCount возвращает количество здоровых нод.
func (b *OllamaBalancer) HealthyCount() int {
	count := 0
	for _, n := range b.nodes {
		if n.isHealthy() {
			count++
		}
	}
	return count
}

// TotalCount возвращает общее количество нод.
func (b *OllamaBalancer) TotalCount() int {
	return len(b.nodes)
}

// NodeStatus возвращает строку со статусом каждой ноды (для метрик/логов).
func (b *OllamaBalancer) NodeStatus() []string {
	result := make([]string, len(b.nodes))
	for i, n := range b.nodes {
		n.mu.RLock()
		if n.healthy {
			result[i] = fmt.Sprintf("%s [ok]", n.url)
		} else {
			result[i] = fmt.Sprintf("%s [down, failures=%d]", n.url, n.failures)
		}
		n.mu.RUnlock()
	}
	return result
}

// ReportSuccess сообщает балансировщику об успешном использовании клиента.
// Используй при успешном стриминге чтобы сбросить circuit breaker.
func (b *OllamaBalancer) ReportSuccess(client *OllamaClient) {
	for _, n := range b.nodes {
		if n.client == client {
			n.markSuccess()
			return
		}
	}
}

// ReportFailure сообщает балансировщику об ошибке на клиенте.
func (b *OllamaBalancer) ReportFailure(client *OllamaClient) {
	for _, n := range b.nodes {
		if n.client == client {
			n.markFailure()
			return
		}
	}
}

// healthCheckLoop периодически проверяет каждую ноду.
func (b *OllamaBalancer) healthCheckLoop() {
	// Не спамим при одиночной ноде
	if len(b.nodes) == 1 {
		return
	}

	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	for range ticker.C {
		for _, n := range b.nodes {
			available := n.client.IsAvailable()
			n.mu.Lock()
			if available {
				n.healthy = true
				n.failures = 0
			} else if n.healthy {
				// Не переводим в unhealthy сразу — circuit breaker делает это через markFailure
				log.Printf("[balancer] нода %s не отвечает на health check", n.url)
			}
			n.mu.Unlock()
		}
	}
}
