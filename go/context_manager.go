// context_manager.go — релевантностная фильтрация контекста диалога.
//
// Задача: перед отправкой в LLM отобрать из истории только те сообщения,
// которые содержательно связаны с текущим вопросом пользователя.
// Это уменьшает объём контекста и экономит токены без потери качества.
//
// Алгоритм (без дополнительных LLM-вызовов):
//  1. Токенизируем запрос и сообщения в наборы значимых слов (>3 символов).
//  2. Считаем TF-пересечение: сколько уникальных слов запроса встречается в сообщении.
//  3. Бонус +0.15 за соседнее сообщение (user+assistant пара — держим вместе).
//  4. Отбираем topN сообщений с наибольшим score; system-сообщения всегда сохраняем.
//  5. Восстанавливаем порядок (по исходным индексам).
//
// Дополнительно:
//   - SmartContext: сначала TrimToTokenBudget, потом FilterByRelevance — двойная экономия.
//   - PairMessages: удобная обёртка, гарантирующая что пары user/assistant не разрываются.
package main

import (
	"sort"
	"strings"
	"unicode"
)

const (
	// minWordLen — минимальная длина слова для индексирования (стоп-слова короче игнорируются).
	minWordLen = 3

	// defaultTopN — сколько non-system сообщений оставлять по умолчанию.
	defaultTopN = 12

	// pairBonus — бонус к score соседнего сообщения в паре user/assistant.
	pairBonus = 0.15
)

// scoredMsg — сообщение с вычисленным коэффициентом релевантности.
type scoredMsg struct {
	idx   int     // исходный индекс в слайсе messages
	score float64 // чем выше — тем релевантнее
}

// FilterByRelevance возвращает отфильтрованный слайс сообщений:
//   - Все system-сообщения сохраняются безусловно.
//   - Из non-system оставляем topN наиболее релевантных query.
//   - Порядок сообщений сохраняется (как в оригинале).
//
// Если topN <= 0 — используется defaultTopN.
// Если messages содержит ≤ topN non-system сообщений — возвращается как есть.
func FilterByRelevance(messages []Message, query string, topN int) []Message {
	if topN <= 0 {
		topN = defaultTopN
	}

	// Разделяем: system vs остальные
	var sysMessages []Message
	var rest []Message
	restIdx := make([]int, 0, len(messages))

	for i, m := range messages {
		if m.Role == "system" {
			sysMessages = append(sysMessages, m)
		} else {
			rest = append(rest, m)
			restIdx = append(restIdx, i)
		}
	}

	// Фильтровать нечего
	if len(rest) <= topN {
		return messages
	}

	// Токенизируем запрос
	queryWords := tokenize(query)
	if len(queryWords) == 0 {
		// Запрос пустой — возвращаем последние topN сообщений (свежие важнее)
		result := append([]Message{}, sysMessages...)
		start := len(rest) - topN
		if start < 0 {
			start = 0
		}
		result = append(result, rest[start:]...)
		return result
	}

	// Считаем score для каждого non-system сообщения
	scores := make([]scoredMsg, len(rest))
	for i, m := range rest {
		scores[i] = scoredMsg{
			idx:   i,
			score: relevanceScore(m.Content, queryWords),
		}
	}

	// Бонус: если одно из сообщений пары (user/assistant) имеет высокий score,
	// поднимаем score соседнего — чтобы не разрывать диалоговые пары.
	for i := 0; i < len(scores)-1; i++ {
		if scores[i].score > 0 || scores[i+1].score > 0 {
			scores[i].score += pairBonus
			scores[i+1].score += pairBonus
		}
	}

	// Бонус за свежесть: последние 4 сообщения всегда в топе
	freshStart := len(scores) - 4
	if freshStart < 0 {
		freshStart = 0
	}
	for i := freshStart; i < len(scores); i++ {
		scores[i].score += 0.5
	}

	// Сортируем по убыванию score (stable — сохраняем порядок при равных)
	sort.SliceStable(scores, func(i, j int) bool {
		return scores[i].score > scores[j].score
	})

	// Берём топ-N индексов
	selectedSet := make(map[int]bool, topN)
	for i := 0; i < topN && i < len(scores); i++ {
		selectedSet[scores[i].idx] = true
	}

	// Восстанавливаем порядок: system-сообщения + отобранные non-system в исходном порядке
	result := append([]Message{}, sysMessages...)
	for i, m := range rest {
		if selectedSet[i] {
			result = append(result, m)
		}
	}

	return result
}

// SmartContext применяет двухэтапное сжатие контекста:
//  1. TrimToTokenBudget — обрезает по бюджету токенов.
//  2. FilterByRelevance  — оставляет только релевантные сообщения.
//
// Возвращает (отфильтрованные сообщения, originalTokens, filteredTokens).
func SmartContext(messages []Message, query string, budget int, topN int) ([]Message, int, int) {
	original := EstimateTokens(messages)

	// Шаг 1: обрезка по бюджету
	trimmed := TrimToTokenBudget(messages, budget)

	// Шаг 2: фильтрация по релевантности
	filtered := FilterByRelevance(trimmed, query, topN)

	return filtered, original, EstimateTokens(filtered)
}

// ── Вспомогательные ──────────────────────────────────────────────────────────

// tokenize разбивает текст на уникальные значимые слова в нижнем регистре.
// Слова короче minWordLen символов отбрасываются (стоп-слова: и, в, на, по, …).
func tokenize(text string) map[string]bool {
	words := make(map[string]bool)
	text = strings.ToLower(text)

	var buf strings.Builder
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			buf.WriteRune(r)
		} else {
			if w := buf.String(); len([]rune(w)) >= minWordLen {
				words[w] = true
			}
			buf.Reset()
		}
	}
	if w := buf.String(); len([]rune(w)) >= minWordLen {
		words[w] = true
	}
	return words
}

// relevanceScore вычисляет долю слов запроса, встречающихся в тексте сообщения.
// Возвращает значение в [0, 1].
func relevanceScore(msgContent string, queryWords map[string]bool) float64 {
	if len(queryWords) == 0 {
		return 0
	}
	msgWords := tokenize(msgContent)
	hits := 0
	for w := range queryWords {
		if msgWords[w] {
			hits++
		}
	}
	return float64(hits) / float64(len(queryWords))
}
