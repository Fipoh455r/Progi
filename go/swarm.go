// swarm.go — Рой 100 ИИ-агентов: делит большие задачи на микро-чанки,
// обрабатывает параллельно с минимальным контекстом, сливает результаты пирамидально.
//
// ┌──────────────────────────────────────────────────────────────────────────┐
// │  ПРИНЦИП ЭКОНОМИИ ТОКЕНОВ                                                │
// │                                                                          │
// │  Задача 100M токенов → 50K токенов через рой:                            │
// │                                                                          │
// │  1. Relevance filter  — отбрасываем нерелевантные чанки (TF-IDF)         │
// │     100M → только топ-100 чанков × 400 токенов = 40K токенов данных      │
// │                                                                          │
// │  2. Compact prompts   — ультракомпактный system prompt (25 токенов)      │
// │     vs 300 токенов дефолтного = экономия 275 × 100 агентов = 27K         │
// │                                                                          │
// │  3. Parallel micro-agents — каждый агент видит только свой чанк          │
// │     100 агентов × (25 system + 400 chunk + 50 question) = 47.5K          │
// │                                                                          │
// │  4. Pyramidal merge   — O(log₂ 100) = 7 проходов, ~3K токенов суммарно  │
// │                                                                          │
// │  Итого: ~50K токенов  vs  100M наивно = экономия в 2000 раз              │
// └──────────────────────────────────────────────────────────────────────────┘
//
// API:
//   POST /api/swarm  {"text":"...", "question":"...", "model":"...", "max_agents":100}
//   → SSE поток событий SwarmEvent (kind: start|chunk|merge|done|error)
package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ── Константы ────────────────────────────────────────────────────────────────

const (
	// swarmMaxAgents — потолок параллельных агентов (горутин).
	swarmMaxAgents = 100

	// swarmChunkWords — слов в одном чанке (≈300-400 токенов при кириллице).
	swarmChunkWords = 300

	// swarmChunkOverlap — перекрытие чанков в словах (избегаем потери контекста на границах).
	swarmChunkOverlap = 25

	// swarmMergeConcurrency — параллельных слияний на одном уровне пирамиды.
	swarmMergeConcurrency = 12

	// swarmCallTimeout — тайм-аут одного LLM-вызова агента.
	swarmCallTimeout = 90 * time.Second
)

// Ультракомпактные system prompts — ключ к экономии токенов.
// Размер каждого: ~20-30 токенов (vs 150-300 у обычных промптов).
const (
	swarmAnalystPrompt = "Аналитик. По данному тексту кратко ответь на вопрос. Максимум 3 предложения. Только факты из текста. Без вступлений."
	swarmMergePrompt   = "Синтезатор. Объедини два фрагмента в один краткий текст. Убери повторы. Максимум 4 предложения."
	swarmFinalPrompt   = "Финальный синтез. Дай чёткий исчерпывающий ответ на вопрос на основе всех фрагментов. Структурируй, без воды."
)

// ── Типы ─────────────────────────────────────────────────────────────────────

// SwarmJob — задание для роя агентов.
type SwarmJob struct {
	Text      string `json:"text"`       // входной текст (может быть очень большим)
	Question  string `json:"question"`   // вопрос к тексту
	Model     string `json:"model"`      // модель (пустое = дефолтная)
	MaxAgents int    `json:"max_agents"` // макс агентов (0 = swarmMaxAgents)
}

// SwarmResult — итоговый результат работы роя.
type SwarmResult struct {
	Answer      string `json:"answer"`
	ChunkCount  int    `json:"chunk_count"`  // всего чанков во входном тексте
	AgentsUsed  int    `json:"agents_used"`  // агентов реально запущено
	TokensIn    int    `json:"tokens_in"`    // суммарная оценка входных токенов
	TokensOut   int    `json:"tokens_out"`   // суммарная оценка выходных токенов
	MergePasses int    `json:"merge_passes"` // проходов пирамидального слияния
	Duration    string `json:"duration"`     // время выполнения (human-readable)
}

// SwarmEvent — одно SSE-событие прогресса.
type SwarmEvent struct {
	Kind      string       `json:"kind"`                // start|chunk|merge|done|error
	Index     int          `json:"index,omitempty"`     // номер агента (kind=chunk)
	Total     int          `json:"total,omitempty"`     // всего агентов
	Pass      int          `json:"pass,omitempty"`      // номер прохода (kind=merge)
	Remaining int          `json:"remaining,omitempty"` // осталось слить (kind=merge)
	Message   string       `json:"message,omitempty"`
	Elapsed   float64      `json:"elapsed,omitempty"` // секунд с начала
	Result    *SwarmResult `json:"result,omitempty"`  // заполнено только kind=done
}

// ── Точка входа ───────────────────────────────────────────────────────────────

// RunSwarm запускает рой агентов для обработки большого текста.
// progressCh получает события прогресса и закрывается при завершении.
// Канал должен быть буферизован (рекомендуется: make(chan SwarmEvent, 128)).
func RunSwarm(
	ctx context.Context,
	client *OllamaClient,
	job SwarmJob,
	progressCh chan<- SwarmEvent,
) (SwarmResult, error) {
	defer close(progressCh)
	start := time.Now()

	maxAgents := job.MaxAgents
	if maxAgents <= 0 || maxAgents > swarmMaxAgents {
		maxAgents = swarmMaxAgents
	}

	// ── Шаг 1: Разбить текст на чанки ────────────────────────────────────
	allChunks := splitTextIntoChunks(job.Text, swarmChunkWords, swarmChunkOverlap)
	if len(allChunks) == 0 {
		progressCh <- SwarmEvent{Kind: "error", Message: "текст пустой или слишком короткий"}
		return SwarmResult{}, fmt.Errorf("текст пустой")
	}

	// ── Шаг 2: Фильтрация по релевантности вопросу ───────────────────────
	// Если чанков больше maxAgents — берём только самые релевантные.
	relevant := swarmSelectChunks(allChunks, job.Question, maxAgents)

	// Оцениваем входные токены
	tokensIn := 0
	for _, c := range relevant {
		tokensIn += estimateTemplateTokens(c)
	}
	promptOverhead := estimateTemplateTokens(swarmAnalystPrompt) + estimateTemplateTokens(job.Question)
	tokensIn += len(relevant) * promptOverhead

	progressCh <- SwarmEvent{
		Kind:    "start",
		Total:   len(relevant),
		Message: fmt.Sprintf("Рой запущен: %d агентов, %d чанков из %d, ~%d токенов входа", len(relevant), len(relevant), len(allChunks), tokensIn),
		Elapsed: 0,
	}

	// ── Шаг 3: Параллельная обработка ────────────────────────────────────
	answers := swarmProcessChunks(ctx, client, relevant, job.Question, job.Model, progressCh, start)
	if len(answers) == 0 {
		progressCh <- SwarmEvent{Kind: "error", Message: "все агенты вернули пустые ответы — проверь доступность Ollama"}
		return SwarmResult{}, fmt.Errorf("нет ответов от агентов")
	}

	// ── Шаг 4: Пирамидальное слияние ─────────────────────────────────────
	passes := 0
	final := swarmPyramidalMerge(ctx, client, answers, job.Question, job.Model, progressCh, &passes, start)

	// Оцениваем выходные токены (суммарно по всем уровням)
	tokensOut := estimateTemplateTokens(final) * (passes + 1) // грубо: каждый проход производит ~один финальный
	if tokensOut < len(answers)*estimateTemplateTokens("ответ") {
		tokensOut = len(answers) * 30 // минимальная оценка: 30 токенов на агента
	}

	elapsed := time.Since(start)
	result := SwarmResult{
		Answer:      final,
		ChunkCount:  len(allChunks),
		AgentsUsed:  len(relevant),
		TokensIn:    tokensIn,
		TokensOut:   tokensOut,
		MergePasses: passes,
		Duration:    elapsed.Round(time.Millisecond).String(),
	}

	progressCh <- SwarmEvent{
		Kind:    "done",
		Message: fmt.Sprintf("Завершено за %s | агентов: %d | токенов: ~%d вх / ~%d вых | слияний: %d", elapsed.Round(time.Second), len(relevant), tokensIn, tokensOut, passes),
		Elapsed: elapsed.Seconds(),
		Result:  &result,
	}

	// Записываем в статистику токенов (context_filter как источник экономии)
	// Оцениваем "наивную" стоимость: весь текст × 1 запрос
	naiveTokens := estimateTemplateTokens(job.Text)
	RecordContextFilter(naiveTokens, tokensIn)

	return result, nil
}

// ── Параллельная обработка чанков ────────────────────────────────────────────

// swarmProcessChunks запускает агентов параллельно, собирает ответы в порядке индексов.
// Агенты, вернувшие пустой ответ, пропускаются.
func swarmProcessChunks(
	ctx context.Context,
	client *OllamaClient,
	chunks []string,
	question, model string,
	progressCh chan<- SwarmEvent,
	start time.Time,
) []string {
	type result struct {
		idx    int
		answer string
	}

	resultCh := make(chan result, len(chunks))
	sem := make(chan struct{}, swarmMaxAgents) // семафор: не больше N параллельных вызовов
	var wg sync.WaitGroup

	for i, chunk := range chunks {
		wg.Add(1)
		sem <- struct{}{} // ждём свободного слота

		go func(idx int, text string) {
			defer wg.Done()
			defer func() { <-sem }()

			answer, err := swarmAnalyzeChunk(ctx, client, text, question, model)
			if err != nil {
				answer = "" // деградация: пропускаем проблемный агент
			}
			resultCh <- result{idx, strings.TrimSpace(answer)}

			// Прогресс (non-blocking: не блокируемся если читатель не успевает)
			select {
			case progressCh <- SwarmEvent{
				Kind:    "chunk",
				Index:   idx + 1,
				Total:   len(chunks),
				Elapsed: time.Since(start).Seconds(),
			}:
			default:
			}
		}(i, chunk)
	}

	// Горутина закрывает канал когда все завершены
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// Собираем результаты (сохраняем порядок по idx)
	ordered := make([]string, len(chunks))
	for r := range resultCh {
		ordered[r.idx] = r.answer
	}

	// Отфильтровываем пустые, сохраняем порядок
	var nonEmpty []string
	for _, a := range ordered {
		if a != "" {
			nonEmpty = append(nonEmpty, a)
		}
	}
	return nonEmpty
}

// ── Пирамидальное слияние ─────────────────────────────────────────────────────

// swarmPyramidalMerge объединяет answers рекурсивно попарно — O(log₂ N) проходов.
// Каждый проход параллелен с конкурентностью swarmMergeConcurrency.
func swarmPyramidalMerge(
	ctx context.Context,
	client *OllamaClient,
	answers []string,
	question, model string,
	progressCh chan<- SwarmEvent,
	passes *int,
	start time.Time,
) string {
	if len(answers) == 0 {
		return ""
	}
	if len(answers) == 1 {
		return answers[0]
	}

	*passes++

	select {
	case progressCh <- SwarmEvent{
		Kind:      "merge",
		Pass:      *passes,
		Remaining: len(answers),
		Message:   fmt.Sprintf("Слияние: проход %d, %d → %d ответов", *passes, len(answers), (len(answers)+1)/2),
		Elapsed:   time.Since(start).Seconds(),
	}:
	default:
	}

	// Последний ли это проход (финальная пара или одиночный остаток)?
	isFinalPass := len(answers) <= 2

	merged := make([]string, 0, (len(answers)+1)/2)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, swarmMergeConcurrency)

	for i := 0; i < len(answers); i += 2 {
		if i+1 >= len(answers) {
			// Нечётный остаток — переносим без изменений
			mu.Lock()
			merged = append(merged, answers[i])
			mu.Unlock()
			continue
		}

		a, b := answers[i], answers[i+1]
		wg.Add(1)
		sem <- struct{}{}

		go func(x, y string, final bool) {
			defer wg.Done()
			defer func() { <-sem }()

			var combined string
			var err error

			if final {
				// Финальный синтез учитывает исходный вопрос
				combined, err = swarmFinalMerge(ctx, client, x, y, question, model)
			} else {
				// Промежуточное слияние: только объединить, без учёта вопроса
				combined, err = swarmMergeTwo(ctx, client, x, y, model)
			}

			if err != nil || strings.TrimSpace(combined) == "" {
				// Fallback: конкатенация через разделитель + обрезка
				combined = swarmFallbackMerge(x, y)
			}

			mu.Lock()
			merged = append(merged, combined)
			mu.Unlock()
		}(a, b, isFinalPass)
	}

	wg.Wait()

	// Следующий уровень пирамиды
	return swarmPyramidalMerge(ctx, client, merged, question, model, progressCh, passes, start)
}

// ── LLM-вызовы агентов ────────────────────────────────────────────────────────

// swarmAnalyzeChunk — агент-аналитик: отвечает на вопрос по одному чанку.
func swarmAnalyzeChunk(ctx context.Context, client *OllamaClient, chunk, question, model string) (string, error) {
	msgs := []Message{
		{Role: "system", Content: swarmAnalystPrompt},
		{Role: "user", Content: "Текст:\n" + chunk + "\n\nВопрос: " + question},
	}
	return swarmCollect(ctx, client, msgs, model, 0.1)
}

// swarmMergeTwo — промежуточный синтезатор: объединяет два ответа.
func swarmMergeTwo(ctx context.Context, client *OllamaClient, a, b, model string) (string, error) {
	msgs := []Message{
		{Role: "system", Content: swarmMergePrompt},
		{Role: "user", Content: "Ответ 1:\n" + a + "\n\nОтвет 2:\n" + b},
	}
	return swarmCollect(ctx, client, msgs, model, 0.2)
}

// swarmFinalMerge — финальный синтезатор: учитывает исходный вопрос.
func swarmFinalMerge(ctx context.Context, client *OllamaClient, a, b, question, model string) (string, error) {
	msgs := []Message{
		{Role: "system", Content: swarmFinalPrompt},
		{Role: "user", Content: "Вопрос: " + question + "\n\nФрагмент 1:\n" + a + "\n\nФрагмент 2:\n" + b},
	}
	return swarmCollect(ctx, client, msgs, model, 0.3)
}

// swarmCollect запускает LLM, собирает все токены в строку.
// Температура 0.1 для детерминированных точных ответов агентов.
func swarmCollect(ctx context.Context, client *OllamaClient, messages []Message, model string, temp float64) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, swarmCallTimeout)
	defer cancel()

	tokenCh, errCh := client.ChatStreamWithTemp(callCtx, messages, model, temp)

	var sb strings.Builder
	for token := range tokenCh {
		sb.WriteString(token)
	}
	if err := <-errCh; err != nil {
		return strings.TrimSpace(sb.String()), err
	}
	return strings.TrimSpace(sb.String()), nil
}

// ── Вспомогательные ──────────────────────────────────────────────────────────

// splitTextIntoChunks делит текст на чанки по targetWords слов с перекрытием overlap.
// Перекрытие помогает агентам не терять контекст на границах чанков.
func splitTextIntoChunks(text string, targetWords, overlap int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	if targetWords <= 0 {
		targetWords = swarmChunkWords
	}
	if overlap < 0 || overlap >= targetWords {
		overlap = 0
	}

	step := targetWords - overlap
	if step <= 0 {
		step = targetWords
	}

	var chunks []string
	for start := 0; start < len(words); start += step {
		end := start + targetWords
		if end > len(words) {
			end = len(words)
		}
		chunks = append(chunks, strings.Join(words[start:end], " "))
		if end >= len(words) {
			break
		}
	}
	return chunks
}

// swarmSelectChunks отбирает топ-maxN наиболее релевантных чанков по TF keyword scoring.
// Использует tokenize и relevanceScore из context_manager.go.
// Если чанков ≤ maxN — возвращает все без изменений, сохраняя порядок.
func swarmSelectChunks(chunks []string, question string, maxN int) []string {
	if len(chunks) <= maxN {
		return chunks
	}

	queryWords := tokenize(question) // из context_manager.go
	if len(queryWords) == 0 {
		return swarmEvenSample(chunks, maxN)
	}

	type scored struct {
		idx   int
		score float64
	}
	scores := make([]scored, len(chunks))
	for i, c := range chunks {
		scores[i] = scored{i, relevanceScore(c, queryWords)} // из context_manager.go
	}

	sort.SliceStable(scores, func(i, j int) bool {
		return scores[i].score > scores[j].score
	})

	// Запоминаем топ-maxN индексов, восстанавливаем исходный порядок
	selected := make(map[int]struct{}, maxN)
	for i := 0; i < maxN; i++ {
		selected[scores[i].idx] = struct{}{}
	}

	result := make([]string, 0, maxN)
	for i, c := range chunks {
		if _, ok := selected[i]; ok {
			result = append(result, c)
		}
	}
	return result
}

// swarmEvenSample возвращает n равномерно распределённых чанков.
func swarmEvenSample(chunks []string, n int) []string {
	if n <= 0 || len(chunks) == 0 {
		return nil
	}
	if len(chunks) <= n {
		return chunks
	}
	result := make([]string, 0, n)
	step := float64(len(chunks)) / float64(n)
	for i := 0; i < n; i++ {
		idx := int(float64(i) * step)
		if idx >= len(chunks) {
			idx = len(chunks) - 1
		}
		result = append(result, chunks[idx])
	}
	return result
}

// swarmFallbackMerge — аварийное слияние двух строк без LLM.
// Берёт первые 2 предложения каждой части.
func swarmFallbackMerge(a, b string) string {
	return swarmFirstSentences(a, 2) + " " + swarmFirstSentences(b, 2)
}

// swarmFirstSentences возвращает первые n предложений текста.
func swarmFirstSentences(text string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	runes := []rune(text)
	for i, r := range runes {
		if r == '.' || r == '!' || r == '?' {
			count++
			if count >= n {
				return strings.TrimSpace(string(runes[:i+1]))
			}
		}
	}
	// Меньше n предложений — обрезаем до 400 рун
	if len(runes) > 400 {
		return string(runes[:400])
	}
	return text
}
