// compress.go — автоматическое сжатие истории диалога для экономии токенов.
//
// Стратегия:
//   - Когда история > maxHistory сообщений, суммаризуем старую часть.
//   - Оставляем: [system] + [summary] + последние keepRecent сообщений.
//   - Суммаризация идёт той же моделью, что и основной чат.
//
// Экономия: при 40 сообщениях по ~200 токенов = ~8000 токенов на запрос.
// После сжатия: ~1000 токенов суммари + ~2000 токенов последних 10 = ~3000.
// Выигрыш: 2.7x меньше токенов.
package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	// Сжимаем когда сообщений больше этого значения (не считая system)
	maxHistoryMessages = 24
	// Столько последних сообщений оставляем нетронутыми (должны быть в свежем контексте)
	keepRecentMessages = 8
	// Таймаут на суммаризацию
	summaryTimeout = 45 * time.Second
)

// CompressHistory сжимает историю если она слишком длинная.
// Возвращает (сжатую историю, была ли компрессия, ошибка).
func CompressHistory(
	ctx context.Context,
	client *OllamaClient,
	messages []Message,
	model string,
) ([]Message, bool, error) {
	// Считаем non-system сообщения
	nonSystem := 0
	for _, m := range messages {
		if m.Role != "system" {
			nonSystem++
		}
	}

	if nonSystem <= maxHistoryMessages {
		return messages, false, nil
	}

	// Разделяем историю:
	// [0] = system prompt (всегда первый)
	// [1..n-keepRecent] = старая часть → суммаризуем
	// [n-keepRecent..] = свежие сообщения → оставляем

	systemMsg := messages[0] // гарантированно role=system
	rest := messages[1:]

	if len(rest) <= keepRecentMessages {
		return messages, false, nil
	}

	toSummarize := rest[:len(rest)-keepRecentMessages]
	toKeep := rest[len(rest)-keepRecentMessages:]

	summary, err := summarize(ctx, client, model, toSummarize)
	if err != nil {
		// При ошибке суммаризации — просто обрезаем старую часть
		return append([]Message{systemMsg}, toKeep...), false, nil
	}

	compressed := []Message{
		systemMsg,
		{
			Role:    "system",
			Content: "[СВОДКА ПРЕДЫДУЩЕГО ДИАЛОГА]\n" + summary,
		},
	}
	compressed = append(compressed, toKeep...)

	return compressed, true, nil
}

// summarize просит модель кратко пересказать массив сообщений.
func summarize(ctx context.Context, client *OllamaClient, model string, msgs []Message) (string, error) {
	// Формируем текст для суммаризации
	var sb strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "user":
			sb.WriteString("Пользователь: ")
		case "assistant":
			sb.WriteString("Ассистент: ")
		default:
			continue
		}
		// Ограничиваем каждое сообщение — нет смысла передавать огромные блоки кода
		content := m.Content
		if len([]rune(content)) > 300 {
			content = string([]rune(content)[:297]) + "…"
		}
		sb.WriteString(content)
		sb.WriteString("\n")
	}

	prompt := []Message{
		{
			Role: "system",
			Content: "Ты помощник по суммаризации. Твоя задача — создать краткую, " +
				"информативную сводку диалога. Сохрани ключевые факты, решения, " +
				"и контекст. Пиши сжато, до 200 слов. Без лишних вводных фраз.",
		},
		{
			Role:    "user",
			Content: fmt.Sprintf("Составь краткую сводку этого диалога:\n\n%s", sb.String()),
		},
	}

	sctx, cancel := context.WithTimeout(ctx, summaryTimeout)
	defer cancel()

	tokenCh, errCh := client.ChatStreamWithTemp(sctx, prompt, model, 0.3)

	var result strings.Builder
	for token := range tokenCh {
		result.WriteString(token)
	}
	if err := <-errCh; err != nil {
		return "", fmt.Errorf("суммаризация не удалась: %w", err)
	}

	return strings.TrimSpace(result.String()), nil
}

// EstimateTokens грубо оценивает количество токенов в массиве сообщений.
// Правило: 1 токен ≈ 4 символа (для латиницы) или ≈ 2 символа (для кириллицы).
func EstimateTokens(messages []Message) int {
	total := 0
	for _, m := range messages {
		runes := []rune(m.Content)
		total += len(runes)/3 + 4 // +4 за overhead роли
	}
	return total
}

// TrimToTokenBudget обрезает историю до заданного бюджета токенов.
// Удаляет старые (не системные) сообщения, пока бюджет не будет соблюдён.
func TrimToTokenBudget(messages []Message, budget int) []Message {
	if EstimateTokens(messages) <= budget {
		return messages
	}

	// Находим первое non-system сообщение
	sysEnd := 0
	for sysEnd < len(messages) && messages[sysEnd].Role == "system" {
		sysEnd++
	}

	rest := messages[sysEnd:]
	for len(rest) > 2 && EstimateTokens(append(messages[:sysEnd], rest...)) > budget {
		rest = rest[2:] // удаляем пару (user + assistant)
	}

	return append(messages[:sysEnd], rest...)
}
