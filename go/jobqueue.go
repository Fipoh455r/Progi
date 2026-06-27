// jobqueue.go — очередь фоновых задач для тяжёлых операций.
//
// Позволяет запускать длительные задачи (swarm, multiagent) асинхронно:
//  1. POST /api/jobs   {"kind":"swarm","payload":{...}}  → {"id":"...","status":"pending"}
//  2. GET  /api/jobs/{id}                                → Job с полем result когда ready
//  3. GET  /api/jobs                                     → список всех задач
//  4. DELETE /api/jobs/{id}                              → удалить задачу
//
// Конфигурация:
//   LOCALAI_JOB_WORKERS=4   — параллельных воркеров (по умолчанию 4)
//
// Виды задач (kind):
//   "swarm"      — рой агентов (SwarmJob)
//   "multiagent" — оркестратор (поле task+model)
//
// Персистентность: data/jobs/{id}.json — задачи сохраняются при перезапуске.
// Автоочистка: задачи старше 24 часов удаляются при старте.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ── Константы ─────────────────────────────────────────────────────────────────

const (
	jqDefaultWorkers = 4
	jqMaxPending     = 256 // буфер канала ожидания
	jqJobTTL         = 24 * time.Hour
	jqMaxProgress    = 50 // последних событий прогресса хранить в Job
)

// ── Типы ──────────────────────────────────────────────────────────────────────

// JobStatus — состояние задачи.
type JobStatus string

const (
	JobPending JobStatus = "pending"
	JobRunning JobStatus = "running"
	JobDone    JobStatus = "done"
	JobFailed  JobStatus = "failed"
)

// Job — одна фоновая задача.
type Job struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`              // "swarm" | "multiagent"
	Status     JobStatus       `json:"status"`
	Payload    json.RawMessage `json:"payload"`           // зависит от kind
	Result     string          `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	Progress   []string        `json:"progress,omitempty"` // последние jqMaxProgress событий
	CreatedAt  time.Time       `json:"created_at"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
}

// jobSummary — краткое представление для списка.
type jobSummary struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Status     JobStatus  `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// multiagentPayload — тело для kind=multiagent.
type multiagentPayload struct {
	Task  string `json:"task"`
	Model string `json:"model"`
}

// JobQueue — очередь задач с пулом воркеров.
type JobQueue struct {
	mu      sync.RWMutex
	jobs    map[string]*Job
	dir     string
	pending chan string // channel job IDs

	// зависимости (устанавливаются при инициализации)
	client *OllamaClient
	model  string
}

// globalJobQueue — глобальный экземпляр.
var globalJobQueue *JobQueue

// ── Инициализация ─────────────────────────────────────────────────────────────

// InitJobQueue создаёт очередь, загружает незавершённые задачи с диска,
// запускает N воркеров и фоновую горутину очистки.
func InitJobQueue(dataDir string, client *OllamaClient, defaultModel string, workers int) error {
	if workers <= 0 {
		workers = jqDefaultWorkers
	}

	dir := filepath.Join(dataDir, "jobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("jobqueue mkdir: %w", err)
	}

	jq := &JobQueue{
		jobs:    make(map[string]*Job, 64),
		dir:     dir,
		pending: make(chan string, jqMaxPending),
		client:  client,
		model:   defaultModel,
	}

	// Загружаем задачи с диска и сбрасываем "зависшие" running → failed
	jq.loadAll()

	// Запускаем воркеры
	for i := 0; i < workers; i++ {
		go jq.worker()
	}

	// Фоновая очистка старых задач каждые 6 часов
	go func() {
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			jq.cleanOld()
		}
	}()

	globalJobQueue = jq
	return nil
}

// ── Публичный API ─────────────────────────────────────────────────────────────

// Submit добавляет новую задачу в очередь и возвращает её ID.
func (jq *JobQueue) Submit(kind string, payload json.RawMessage) (*Job, error) {
	switch kind {
	case "swarm", "multiagent":
	default:
		return nil, fmt.Errorf("неизвестный kind: %q (допустимо: swarm, multiagent)", kind)
	}

	job := &Job{
		ID:        jqMakeID(),
		Kind:      kind,
		Status:    JobPending,
		Payload:   payload,
		CreatedAt: time.Now(),
	}

	jq.mu.Lock()
	jq.jobs[job.ID] = job
	jq.mu.Unlock()

	if err := jq.save(job); err != nil {
		log.Printf("[jobqueue] save %s: %v", job.ID, err)
	}

	// Отправляем в очередь (non-blocking с fallback)
	select {
	case jq.pending <- job.ID:
	default:
		// Переполнена очередь — всё равно сохранили, перезапуск подберёт
		log.Printf("[jobqueue] очередь переполнена, задача %s будет выполнена после перезапуска", job.ID)
	}

	return job, nil
}

// Get возвращает задачу по ID.
func (jq *JobQueue) Get(id string) (*Job, bool) {
	jq.mu.RLock()
	j, ok := jq.jobs[id]
	jq.mu.RUnlock()
	return j, ok
}

// List возвращает краткий список задач, отсортированный по времени создания (новые первые).
func (jq *JobQueue) List() []jobSummary {
	jq.mu.RLock()
	result := make([]jobSummary, 0, len(jq.jobs))
	for _, j := range jq.jobs {
		result = append(result, jobSummary{
			ID:         j.ID,
			Kind:       j.Kind,
			Status:     j.Status,
			CreatedAt:  j.CreatedAt,
			FinishedAt: j.FinishedAt,
			Error:      j.Error,
		})
	}
	jq.mu.RUnlock()

	sort.Slice(result, func(i, k int) bool {
		return result[i].CreatedAt.After(result[k].CreatedAt)
	})
	return result
}

// Delete удаляет задачу из памяти и с диска.
// Нельзя удалить running-задачу (возвращает ошибку).
func (jq *JobQueue) Delete(id string) error {
	jq.mu.Lock()
	j, ok := jq.jobs[id]
	if !ok {
		jq.mu.Unlock()
		return fmt.Errorf("задача %s не найдена", id)
	}
	if j.Status == JobRunning {
		jq.mu.Unlock()
		return fmt.Errorf("нельзя удалить выполняющуюся задачу")
	}
	delete(jq.jobs, id)
	jq.mu.Unlock()

	_ = os.Remove(filepath.Join(jq.dir, id+".json"))
	return nil
}

// ── Воркер ────────────────────────────────────────────────────────────────────

// worker — горутина воркера, бесконечно берёт задачи из pending и выполняет.
func (jq *JobQueue) worker() {
	for id := range jq.pending {
		jq.execute(id)
	}
}

// execute выполняет задачу с данным ID.
func (jq *JobQueue) execute(id string) {
	jq.mu.RLock()
	job, ok := jq.jobs[id]
	jq.mu.RUnlock()
	if !ok {
		return
	}

	// Отмечаем как running
	now := time.Now()
	jq.mu.Lock()
	job.Status = JobRunning
	job.StartedAt = &now
	jq.mu.Unlock()
	_ = jq.save(job)

	// Контекст без тайм-аута (тяжёлые задачи могут идти долго)
	ctx := context.Background()

	var result string
	var execErr error

	switch job.Kind {
	case "swarm":
		result, execErr = jq.runSwarmJob(ctx, job)
	case "multiagent":
		result, execErr = jq.runMultiagentJob(ctx, job)
	default:
		execErr = fmt.Errorf("неизвестный kind: %s", job.Kind)
	}

	// Обновляем финальный статус
	finish := time.Now()
	jq.mu.Lock()
	job.FinishedAt = &finish
	if execErr != nil {
		job.Status = JobFailed
		job.Error = execErr.Error()
	} else {
		job.Status = JobDone
		job.Result = result
	}
	jq.mu.Unlock()
	_ = jq.save(job)
}

// ── Обработчики по видам задач ────────────────────────────────────────────────

// runSwarmJob выполняет задачу роя агентов.
func (jq *JobQueue) runSwarmJob(ctx context.Context, job *Job) (string, error) {
	var swarmJob SwarmJob
	if err := json.Unmarshal(job.Payload, &swarmJob); err != nil {
		return "", fmt.Errorf("invalid swarm payload: %w", err)
	}
	if swarmJob.Model == "" {
		swarmJob.Model = jq.model
	}

	progressCh := make(chan SwarmEvent, 128)

	// Собираем прогресс в Job.Progress
	go func() {
		for ev := range progressCh {
			if ev.Message != "" {
				jq.addProgress(job, ev.Message)
			}
		}
	}()

	result, err := RunSwarm(ctx, jq.client, swarmJob, progressCh)
	if err != nil {
		return "", err
	}
	return result.Answer, nil
}

// runMultiagentJob выполняет мульти-агентную задачу через оркестратор.
func (jq *JobQueue) runMultiagentJob(ctx context.Context, job *Job) (string, error) {
	var payload multiagentPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return "", fmt.Errorf("invalid multiagent payload: %w", err)
	}
	if payload.Task == "" {
		return "", fmt.Errorf("поле task обязательно")
	}
	model := payload.Model
	if model == "" {
		model = jq.model
	}

	progressCh := make(chan OrchestratorEvent, 32)

	go func() {
		for ev := range progressCh {
			if ev.Message != "" {
				jq.addProgress(job, string(ev.Kind)+": "+ev.Message)
			}
		}
	}()

	answer, err := OrchestrateTask(ctx, jq.client, payload.Task, model, progressCh)
	return answer, err
}

// addProgress добавляет сообщение прогресса в Job (хранит последние jqMaxProgress).
func (jq *JobQueue) addProgress(job *Job, msg string) {
	jq.mu.Lock()
	job.Progress = append(job.Progress, msg)
	if len(job.Progress) > jqMaxProgress {
		job.Progress = job.Progress[len(job.Progress)-jqMaxProgress:]
	}
	jq.mu.Unlock()
}

// ── Персистентность ───────────────────────────────────────────────────────────

// save записывает задачу в data/jobs/{id}.json.
func (jq *JobQueue) save(job *Job) error {
	jq.mu.RLock()
	data, err := json.MarshalIndent(job, "", "  ")
	jq.mu.RUnlock()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(jq.dir, job.ID+".json"), data, 0o644)
}

// loadAll читает все JSON-файлы из data/jobs/ при старте.
// "Зависшие" running-задачи (сервер упал) → статус failed.
// Задачи pending → повторно ставятся в очередь.
func (jq *JobQueue) loadAll() {
	entries, err := os.ReadDir(jq.dir)
	if err != nil {
		return
	}

	now := time.Now()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(jq.dir, e.Name()))
		if err != nil {
			continue
		}
		var job Job
		if err := json.Unmarshal(data, &job); err != nil {
			continue
		}

		// Слишком старые — пропускаем
		if now.Sub(job.CreatedAt) > jqJobTTL {
			_ = os.Remove(filepath.Join(jq.dir, e.Name()))
			continue
		}

		// Зависшие running → failed
		if job.Status == JobRunning {
			t := now
			job.Status = JobFailed
			job.FinishedAt = &t
			job.Error = "прервано: сервер был перезапущен"
			_ = jq.save(&job)
		}

		jq.mu.Lock()
		jq.jobs[job.ID] = &job
		jq.mu.Unlock()

		// Незавершённые pending → в очередь
		if job.Status == JobPending {
			select {
			case jq.pending <- job.ID:
			default:
			}
		}
	}
}

// cleanOld удаляет задачи старше jqJobTTL.
func (jq *JobQueue) cleanOld() {
	cutoff := time.Now().Add(-jqJobTTL)

	jq.mu.Lock()
	var toDelete []string
	for id, j := range jq.jobs {
		if j.CreatedAt.Before(cutoff) && j.Status != JobRunning {
			toDelete = append(toDelete, id)
		}
	}
	for _, id := range toDelete {
		delete(jq.jobs, id)
	}
	jq.mu.Unlock()

	for _, id := range toDelete {
		_ = os.Remove(filepath.Join(jq.dir, id+".json"))
	}
}

// ── Вспомогательные ───────────────────────────────────────────────────────────

// jqMakeID генерирует уникальный ID задачи: "job_<hex timestamp>".
func jqMakeID() string {
	return fmt.Sprintf("job_%x", time.Now().UnixNano())
}
