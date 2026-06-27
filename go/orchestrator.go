// orchestrator.go — оркестратор мульти-агентной системы (stdlib only).
//
// Принцип работы:
//  1. Planner-агент разбивает задачу на подзадачи (JSON-список)
//  2. Каждая подзадача назначается лучшей роли из agent_pool
//  3. Подзадачи выполняются параллельно (goroutines + WaitGroup)
//  4. Summarizer собирает все результаты в финальный ответ
//
// Экономия токенов:
//   - Специалисты используют компактные промпты (~50 токенов vs ~400)
//   - Параллельное выполнение сокращает время (не токены, но пользователь доволен)
//   - Кэш: если подзадача повторяется — ноль вызовов LLM
//   - Без роли: прямой вызов через CachedChat без ReAct-цикла
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// OrchestratorEventKind — тип события оркестратора.
type OrchestratorEventKind string

const (
	OrchestratorPlan     OrchestratorEventKind = "plan"      // план получен
	OrchestratorAssigned OrchestratorEventKind = "assigned"  // задача назначена агенту
	OrchestratorDone     OrchestratorEventKind = "done"      // подзадача завершена
	OrchestratorMerge    OrchestratorEventKind = "merge"     // финальная сборка
	OrchestratorResult   OrchestratorEventKind = "result"    // готовый ответ
	OrchestratorError    OrchestratorEventKind = "error"     // ошибка
	OrchestratorCacheHit OrchestratorEventKind = "cache_hit" // кэш-попадание
)

// OrchestratorEvent — одно событие прогресса оркестратора.
type OrchestratorEvent struct {
	Kind    OrchestratorEventKind `json:"kind"`
	Message string                `json:"message"`
	Agent   string                `json:"agent,omitempty"`   // имя роли
	Index   int                   `json:"index,omitempty"`   // номер подзадачи
	Total   int                   `json:"total,omitempty"`   // всего подзадач
	Elapsed int64                 `json:"elapsed_ms,omitempty"`
}

// subtask — одна подзадача, спланированная planner-агентом.
type subtask struct {
	Index       int    `json:"index"`
	Description string `json:"description"` // текст подзадачи
	Role        string `json:"role"`        // предложенная роль (может быть пустой)
}

// subtaskResult — результат выполнения подзадачи.
type subtaskResult struct {
	Index    int
	Role     string
	Response string
	Err      error
	Duration time.Duration
	FromCache bool
}

// OrchestrateTask выполняет задачу через несколько специализированных агентов.
//
// progressCh — канал для SSE-прогресса (закрывается по завершении).
// Возвращает финальный объединённый ответ.
func OrchestrateTask(
	ctx context.Context,
	client *OllamaClient,
	task string,
	model string,
	progressCh chan<- OrchestratorEvent,
) (string, error) {
	defer close(progressCh)

	t0 := time.Now()

	// ── Шаг 1: Planner разбивает задачу ────────────────────────────────
	subtasks, err := planTask(ctx, client, task, model)
	if err != nil {
		// Planner недоступен или не справился — выполняем как единую задачу
		progressCh <- OrchestratorEvent{
			Kind:    OrchestratorPlan,
			Message: "planner недоступен, выполняю напрямую",
			Total:   1,
		}
		subtasks = []subtask{{Index: 1, Description: task, Role: ""}}
	} else {
		progressCh <- OrchestratorEvent{
			Kind:    OrchestratorPlan,
			Message: fmt.Sprintf("план готов: %d подзадач", len(subtasks)),
			Total:   len(subtasks),
		}
	}

	// ── Шаг 2: Назначаем роли и запускаем параллельно ──────────────────
	results := make([]subtaskResult, len(subtasks))
	var wg sync.WaitGroup

	for i, st := range subtasks {
		// Выбираем лучшую роль для подзадачи
		role := st.Role
		if role == "" {
			role = matchRole(st.Description)
		}

		progressCh <- OrchestratorEvent{
			Kind:    OrchestratorAssigned,
			Message: fmt.Sprintf("подзадача %d → агент [%s]", st.Index, role),
			Agent:   role,
			Index:   st.Index,
			Total:   len(subtasks),
		}

		wg.Add(1)
		go func(idx int, task subtask, roleName string) {
			defer wg.Done()
			ts := time.Now()

			resp, fromCache, dur := runSubtask(ctx, client, task.Description, roleName, model)
			results[idx] = subtaskResult{
				Index:     task.Index,
				Role:      roleName,
				Response:  resp,
				Duration:  dur,
				FromCache: fromCache,
			}

			kind := OrchestratorDone
			msg := fmt.Sprintf("подзадача %d готова [%s] за %dмс", task.Index, roleName, time.Since(ts).Milliseconds())
			if fromCache {
				kind = OrchestratorCacheHit
				msg = fmt.Sprintf("подзадача %d из кэша [%s]", task.Index, roleName)
			}

			progressCh <- OrchestratorEvent{
				Kind:    kind,
				Message: msg,
				Agent:   roleName,
				Index:   task.Index,
				Total:   len(subtasks),
				Elapsed: time.Since(ts).Milliseconds(),
			}
		}(i, st, role)
	}

	wg.Wait()

	// ── Шаг 3: Summarizer объединяет результаты ─────────────────────────
	progressCh <- OrchestratorEvent{
		Kind:    OrchestratorMerge,
		Message: "объединяю результаты",
		Elapsed: time.Since(t0).Milliseconds(),
	}

	final, err := mergeResults(ctx, client, task, results, model)
	if err != nil {
		// При ошибке слияния — конкатенируем результаты вручную
		final = concatResults(results)
	}

	progressCh <- OrchestratorEvent{
		Kind:    OrchestratorResult,
		Message: final,
		Elapsed: time.Since(t0).Milliseconds(),
	}

	return final, nil
}

// ── Вспомогательные функции ────────────────────────────────────────────────

// planTask вызывает planner-агента чтобы разбить задачу на подзадачи.
// Planner возвращает JSON-массив: [{"index":1,"description":"...","role":"coder"}, ...]
func planTask(ctx context.Context, client *OllamaClient, task, model string) ([]subtask, error) {
	plannerPrompt := `Ты планировщик задач. Разбей задачу на 2-5 конкретных подзадач.
Каждая подзадача должна быть выполнима одним специалистом.
Доступные роли: coder, debugger, reviewer, researcher, writer, summarizer, critic, translator, analyst, math, security.
Верни ТОЛЬКО JSON-массив без пояснений:
[{"index":1,"description":"конкретная подзадача","role":"лучшая_роль"},...]`

	messages := []Message{
		{Role: "system", Content: plannerPrompt},
		{Role: "user", Content: "Задача: " + task},
	}

	resp, _, err := CachedChat(ctx, client, messages, model, 0.2)
	if err != nil {
		return nil, err
	}

	// Извлекаем JSON из ответа
	jsonStr := extractJSONArray(resp)
	if jsonStr == "" {
		return nil, fmt.Errorf("planner не вернул JSON")
	}

	var tasks []subtask
	if err := json.Unmarshal([]byte(jsonStr), &tasks); err != nil {
		return nil, fmt.Errorf("ошибка разбора плана: %w", err)
	}

	if len(tasks) == 0 {
		return nil, fmt.Errorf("пустой план")
	}

	// Ограничиваем до 5 подзадач чтобы не расходовать слишком много токенов
	if len(tasks) > 5 {
		tasks = tasks[:5]
	}

	return tasks, nil
}

// runSubtask выполняет одну подзадачу через специализированного агента.
// Возвращает (ответ, был_ли_кэш, длительность).
func runSubtask(ctx context.Context, client *OllamaClient, task, roleName, model string) (string, bool, time.Duration) {
	t0 := time.Now()

	role, ok := GetRole(roleName)
	if !ok {
		// Роль не найдена — прямой вызов без специализации
		resp, fromCache, err := CachedChat(ctx, client, []Message{
			{Role: "user", Content: task},
		}, model, 0.5)
		if err != nil {
			return fmt.Sprintf("[ошибка: %v]", err), false, time.Since(t0)
		}
		return resp, fromCache, time.Since(t0)
	}

	// Выполняем через специализированного агента
	messages := []Message{
		{Role: "system", Content: role.Prompt},
		{Role: "user", Content: task},
	}
	if role.UseTools {
		messages[0].Content += "\n\n" + ToolsPrompt()
	}

	resp, fromCache, err := CachedChat(ctx, client, messages, model, role.Temperature)
	if err != nil {
		return fmt.Sprintf("[ошибка агента %s: %v]", roleName, err), false, time.Since(t0)
	}
	return resp, fromCache, time.Since(t0)
}

// mergeResults вызывает summarizer чтобы объединить ответы подзадач.
func mergeResults(ctx context.Context, client *OllamaClient, originalTask string, results []subtaskResult, model string) (string, error) {
	if len(results) == 1 {
		return results[0].Response, nil // нечего объединять
	}

	var sb strings.Builder
	sb.WriteString("Исходная задача: ")
	sb.WriteString(originalTask)
	sb.WriteString("\n\n")
	for _, r := range results {
		if r.Response == "" {
			continue
		}
		sb.WriteString(fmt.Sprintf("=== Результат [%s] ===\n%s\n\n", r.Role, r.Response))
	}

	mergePrompt := "Объедини результаты работы нескольких специалистов в единый связный ответ. Убери дубли. Сохрани все важные детали."

	messages := []Message{
		{Role: "system", Content: mergePrompt},
		{Role: "user", Content: sb.String()},
	}

	resp, _, err := CachedChat(ctx, client, messages, model, 0.3)
	return resp, err
}

// matchRole выбирает наиболее подходящую роль для подзадачи по ключевым словам.
func matchRole(task string) string {
	lower := strings.ToLower(task)

	rules := []struct {
		role     string
		keywords []string
	}{
		{"coder", []string{"напиши код", "реализуй", "функция", "класс", "метод", "скрипт", "программ", "implement", "code", "write function"}},
		{"debugger", []string{"ошибка", "баг", "не работает", "сломан", "исправь", "debug", "fix", "error", "bug"}},
		{"security", []string{"безопасн", "уязвим", "xss", "sql injection", "audit", "security", "exploit", "cve"}},
		{"math", []string{"вычисли", "посчитай", "математик", "формул", "интеграл", "уравнени", "calculate", "math", "formula"}},
		{"analyst", []string{"анализ", "данные", "статистик", "паттерн", "тренд", "analyse", "data", "pattern", "trend"}},
		{"researcher", []string{"исследуй", "найди информацию", "что такое", "расскажи о", "research", "find", "what is"}},
		{"translator", []string{"переведи", "перевод", "translate", "translation"}},
		{"summarizer", []string{"кратко", "суммаризуй", "главное из", "тезисы", "summarize", "summary", "brief"}},
		{"critic", []string{"оцени", "критика", "недостатки", "риски", "critique", "evaluate", "risks"}},
		{"reviewer", []string{"ревью", "review", "проверь код", "check code", "code review"}},
		{"writer", []string{"документация", "readme", "статья", "опиши", "documentation", "write article", "readme"}},
		{"planner", []string{"план", "шаги", "декомпозиц", "roadmap", "plan", "steps", "breakdown"}},
	}

	bestRole := ""
	bestScore := 0

	for _, rule := range rules {
		score := 0
		for _, kw := range rule.keywords {
			if strings.Contains(lower, kw) {
				score++
			}
		}
		if score > bestScore {
			bestScore = score
			bestRole = rule.role
		}
	}

	if bestRole == "" {
		return "researcher" // дефолтная роль
	}
	return bestRole
}

// concatResults собирает ответы подзадач в текст без LLM.
func concatResults(results []subtaskResult) string {
	var sb strings.Builder
	for _, r := range results {
		if r.Response == "" {
			continue
		}
		sb.WriteString(fmt.Sprintf("### %s\n%s\n\n", r.Role, r.Response))
	}
	return strings.TrimSpace(sb.String())
}

// extractJSONArray извлекает первый JSON-массив из строки.
func extractJSONArray(s string) string {
	start := strings.Index(s, "[")
	if start < 0 {
		return ""
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}
