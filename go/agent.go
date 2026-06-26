// agent.go — ReAct агент (Reasoning + Acting).
//
// Принцип работы:
//  1. Модель получает запрос + описание инструментов
//  2. Если нужен инструмент — выводит TOOL_CALL: {...}
//  3. Агент выполняет инструмент, добавляет TOOL_RESULT в контекст
//  4. Цикл повторяется до финального ответа (без TOOL_CALL) или maxSteps
//
// Экономия токенов:
//  - Компрессия истории перед каждым шагом (compress.go)
//  - Бюджет токенов 4096 на весь контекст
//  - Обрезка длинных TOOL_RESULT до 2KB
//  - Краткий системный промпт агента
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	maxAgentSteps  = 8     // максимум шагов до принудительного завершения
	maxToolOutput  = 2048  // байт вывода инструмента (больше — обрезаем)
	agentTokenBudget = 3500 // максимум токенов в контексте агента
)

// StepKind описывает тип шага агента.
type StepKind string

const (
	StepThought     StepKind = "thought"     // рассуждение модели
	StepToolCall    StepKind = "tool_call"   // вызов инструмента
	StepToolResult  StepKind = "tool_result" // результат инструмента
	StepFinalAnswer StepKind = "answer"      // финальный ответ
	StepError       StepKind = "error"       // ошибка
)

// AgentStep — один шаг исполнения агента.
type AgentStep struct {
	Kind     StepKind `json:"kind"`
	Content  string   `json:"content"`            // текст шага
	ToolName string   `json:"tool_name,omitempty"` // для StepToolCall
	ToolArgs string   `json:"tool_args,omitempty"` // JSON аргументов
	Duration int64    `json:"duration_ms,omitempty"`
}

// agentSystemPrompt — короткий системный промпт агента.
// Специально краткий для экономии токенов.
func agentSystemPrompt() string {
	return `Ты LocalAI — умный AI-агент. У тебя есть инструменты.

Правила:
- Если задача требует данных/вычислений — используй инструмент
- Формат вызова: TOOL_CALL: {"name":"имя","args":{аргументы}}
- После получения TOOL_RESULT: продолжай рассуждение
- Финальный ответ — без TOOL_CALL, чётко и по делу
- Не выдумывай факты — используй web_search если не уверен

` + ToolsPrompt()
}

// RunAgent выполняет ReAct-цикл и стримит шаги через stepCh.
// Возвращает финальный ответ.
//
// messages — история чата (с system-промптом пользователя или без).
// stepCh   — канал для отправки шагов в UI (закрывается по завершении).
func RunAgent(
	ctx context.Context,
	client *OllamaClient,
	messages []Message,
	model string,
	temp float64,
	stepCh chan<- AgentStep,
) (string, error) {
	defer close(stepCh)

	// Строим рабочий контекст: заменяем/добавляем системный промпт агента
	agentMsgs := buildAgentContext(messages)

	for step := 0; step < maxAgentSteps; step++ {
		// Применяем бюджет токенов перед каждым шагом
		agentMsgs = TrimToTokenBudget(agentMsgs, agentTokenBudget)

		// Запрашиваем модель
		t0 := time.Now()
		raw, err := collectStream(ctx, client, agentMsgs, model, temp)
		if err != nil {
			stepCh <- AgentStep{Kind: StepError, Content: err.Error()}
			return "", err
		}
		elapsed := time.Since(t0).Milliseconds()

		// Парсим ответ: есть ли вызов инструмента?
		thought, toolCall, finalAnswer := parseAgentOutput(raw)

		// Отправляем шаг рассуждения (если есть текст до вызова)
		if thought != "" {
			stepCh <- AgentStep{
				Kind:     StepThought,
				Content:  thought,
				Duration: elapsed,
			}
		}

		// Финальный ответ — выходим из цикла
		if toolCall == nil {
			answer := finalAnswer
			if answer == "" {
				answer = raw
			}
			stepCh <- AgentStep{Kind: StepFinalAnswer, Content: answer}
			return answer, nil
		}

		// Вызов инструмента
		argsJSON, _ := json.Marshal(toolCall.Args)
		stepCh <- AgentStep{
			Kind:     StepToolCall,
			Content:  fmt.Sprintf("Вызов: %s(%s)", toolCall.Name, string(argsJSON)),
			ToolName: toolCall.Name,
			ToolArgs: string(argsJSON),
		}

		// Выполняем инструмент
		toolResult, toolErr := RunTool(toolCall.Name, toolCall.Args)
		if toolErr != nil {
			toolResult = "ОШИБКА: " + toolErr.Error()
		}

		// Обрезаем длинный результат
		if len(toolResult) > maxToolOutput {
			toolResult = toolResult[:maxToolOutput] + "\n[...обрезано]"
		}

		stepCh <- AgentStep{
			Kind:    StepToolResult,
			Content: toolResult,
		}

		// Добавляем в контекст: ответ модели + результат инструмента
		agentMsgs = append(agentMsgs,
			Message{Role: "assistant", Content: raw},
			Message{Role: "user", Content: "TOOL_RESULT: " + toolResult},
		)
	}

	// Превышен лимит шагов — просим финальный ответ
	agentMsgs = append(agentMsgs, Message{
		Role:    "user",
		Content: "Дай финальный ответ на основе собранной информации. Без TOOL_CALL.",
	})
	answer, err := collectStream(ctx, client, agentMsgs, model, temp)
	if err != nil {
		return "", err
	}
	stepCh <- AgentStep{Kind: StepFinalAnswer, Content: answer}
	return answer, nil
}

// ── Вспомогательные функции ────────────────────────────────────────────────

// parsedToolCall — результат парсинга TOOL_CALL из текста модели.
type parsedToolCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// parseAgentOutput разбирает вывод модели на: рассуждение, вызов инструмента, финальный ответ.
func parseAgentOutput(raw string) (thought string, call *parsedToolCall, final string) {
	const marker = "TOOL_CALL:"

	idx := strings.Index(raw, marker)
	if idx < 0 {
		// Нет вызова инструмента — всё является финальным ответом
		return "", nil, strings.TrimSpace(raw)
	}

	thought = strings.TrimSpace(raw[:idx])
	rest := strings.TrimSpace(raw[idx+len(marker):])

	// Извлекаем JSON объект
	jsonStr := extractJSON(rest)
	if jsonStr == "" {
		// Не удалось распарсить — считаем финальным ответом
		return thought, nil, strings.TrimSpace(raw)
	}

	var tc parsedToolCall
	if err := json.Unmarshal([]byte(jsonStr), &tc); err != nil || tc.Name == "" {
		return thought, nil, strings.TrimSpace(raw)
	}

	return thought, &tc, ""
}

// extractJSON извлекает первый JSON-объект из строки.
func extractJSON(s string) string {
	start := strings.Index(s, "{")
	if start < 0 {
		return ""
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// buildAgentContext формирует контекст для агента:
// заменяет первый system-промпт на агентский, остальные сообщения оставляет.
func buildAgentContext(messages []Message) []Message {
	agentSys := Message{Role: "system", Content: agentSystemPrompt()}

	if len(messages) == 0 {
		return []Message{agentSys}
	}

	// Заменяем системное сообщение (если есть)
	if messages[0].Role == "system" {
		result := make([]Message, len(messages))
		copy(result, messages)
		result[0] = agentSys
		return result
	}

	// Вставляем в начало
	return append([]Message{agentSys}, messages...)
}

// collectStream выполняет запрос к модели и собирает полный ответ.
func collectStream(ctx context.Context, client *OllamaClient, messages []Message, model string, temp float64) (string, error) {
	tokenCh, errCh := client.ChatStreamWithTemp(ctx, messages, model, temp)

	var sb strings.Builder
	for token := range tokenCh {
		sb.WriteString(token)
	}
	if err := <-errCh; err != nil {
		return "", err
	}
	return strings.TrimSpace(sb.String()), nil
}
