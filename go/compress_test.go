// compress_test.go — unit-тесты для сжатия контекста.
// Тестируем только функции без Ollama: EstimateTokens, TrimToTokenBudget.
package main

import (
	"testing"
)

// ── EstimateTokens ────────────────────────────────────────────────────────────

func TestEstimateTokens_Empty(t *testing.T) {
	if got := EstimateTokens(nil); got != 0 {
		t.Errorf("EstimateTokens(nil) = %d, ожидалось 0", got)
	}
	if got := EstimateTokens([]Message{}); got != 0 {
		t.Errorf("EstimateTokens([]) = %d, ожидалось 0", got)
	}
}

func TestEstimateTokens_Nonzero(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "привет"},
		{Role: "assistant", Content: "как дела"},
	}
	got := EstimateTokens(msgs)
	if got <= 0 {
		t.Errorf("EstimateTokens должен вернуть > 0, получили %d", got)
	}
}

func TestEstimateTokens_GrowsWithContent(t *testing.T) {
	short := []Message{{Role: "user", Content: "ok"}}
	long := []Message{{Role: "user", Content: "это длинное сообщение с большим количеством слов и символов для теста"}}

	if EstimateTokens(short) >= EstimateTokens(long) {
		t.Error("длинное сообщение должно давать больше токенов")
	}
}

// ── TrimToTokenBudget ─────────────────────────────────────────────────────────

func makeHistory(pairs int) []Message {
	msgs := []Message{{Role: "system", Content: "Ты помощник."}}
	for i := 0; i < pairs; i++ {
		msgs = append(msgs,
			Message{Role: "user", Content: "вопрос номер один два три четыре"},
			Message{Role: "assistant", Content: "ответ один два три четыре пять"},
		)
	}
	return msgs
}

func TestTrimToTokenBudget_WithinBudget(t *testing.T) {
	msgs := makeHistory(2)
	budget := EstimateTokens(msgs) + 100
	result := TrimToTokenBudget(msgs, budget)
	if len(result) != len(msgs) {
		t.Errorf("TrimToTokenBudget не должен обрезать если бюджет достаточен: len=%d", len(result))
	}
}

func TestTrimToTokenBudget_Trims(t *testing.T) {
	msgs := makeHistory(10)
	budget := 50
	result := TrimToTokenBudget(msgs, budget)
	if len(result) >= len(msgs) {
		t.Error("TrimToTokenBudget должен сократить историю при малом бюджете")
	}
	// Первое сообщение всегда system
	if result[0].Role != "system" {
		t.Error("первое сообщение после обрезки должно быть system")
	}
}

func TestTrimToTokenBudget_KeepsSystem(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "системный промпт"},
		{Role: "user", Content: "сообщение 1"},
		{Role: "assistant", Content: "ответ 1"},
	}
	// Очень маленький бюджет — должны остаться только system
	result := TrimToTokenBudget(msgs, 1)
	if len(result) < 1 || result[0].Role != "system" {
		t.Error("system-сообщение должно сохраниться при любом бюджете")
	}
}

func TestTrimToTokenBudget_MinResult(t *testing.T) {
	msgs := makeHistory(5)
	// Бюджет = 0: не должно паниковать, должен вернуть хотя бы system
	result := TrimToTokenBudget(msgs, 0)
	if len(result) == 0 {
		t.Error("TrimToTokenBudget не должен возвращать пустой слайс")
	}
}
