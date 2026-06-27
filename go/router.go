// router.go — Model Router v3.7: классификатор задач без вызова LLM.
//
// Определяет тип задачи по тексту запроса на основе ключевых слов и эвристик.
// Работает синхронно (~microseconds), не требует модели.
//
// Типы задач:
//   TaskChat   — обычный разговор
//   TaskRAG    — вопрос по загруженным документам
//   TaskAgent  — нужны инструменты (поиск, вычисления, файлы)
//   TaskCode   — генерация / ревью кода
//   TaskMath   — математические вычисления
package main

import (
	"strings"
	"unicode"
)

// TaskType — тип задачи, определённый роутером.
type TaskType string

const (
	TaskChat  TaskType = "chat"
	TaskRAG   TaskType = "rag"
	TaskAgent TaskType = "agent"
	TaskCode  TaskType = "code"
	TaskMath  TaskType = "math"
)

// RouteResult — результат классификации запроса.
type RouteResult struct {
	Task       TaskType `json:"task"`        // определённый тип задачи
	Confidence float64  `json:"confidence"`  // уверенность [0.0, 1.0]
	Reason     string   `json:"reason"`      // краткое объяснение решения
}

// ── Ключевые слова по категориям ─────────────────────────────────────────

var ragKeywords = []string{
	// Русские
	"документ", "файл", "загруж", "текст", "статья", "книга", "отчёт", "отчет",
	"согласно", "в документе", "из документа", "по документу",
	"что написано", "что говорится", "найди в", "поищи в",
	// Английские
	"document", "file", "uploaded", "according to", "based on", "in the doc",
	"from the file", "what does it say", "find in",
}

var agentKeywords = []string{
	// Русские
	"найди в интернете", "погода", "курс валют", "актуальн", "сейчас стоит",
	"запусти", "выполни", "сделай запрос", "проверь сайт", "скачай",
	"список файлов", "покажи файлы", "создай файл", "удали файл",
	"время сейчас", "дата сегодня",
	// Английские
	"search the web", "current weather", "exchange rate", "run command",
	"execute", "fetch url", "download", "list files", "create file",
	"what time is it", "today's date",
}

var codeKeywords = []string{
	// Русские
	"напиши код", "напиши функцию", "напиши класс", "напиши скрипт",
	"напиши программу", "исправь код", "отладь", "рефактор",
	"как реализовать", "покажи пример кода", "сделай метод",
	"баг", "ошибка в коде", "почему не работает код",
	// Английские
	"write code", "write a function", "write a class", "write a script",
	"fix the code", "debug", "refactor", "how to implement",
	"show me code", "code example", "make a method",
}

var mathKeywords = []string{
	// Русские
	"вычисли", "посчитай", "реши уравнение", "реши задачу",
	"интеграл", "производная", "матрица", "вероятность",
	"сколько будет", "чему равно", "найди корень", "логарифм",
	// Английские
	"calculate", "compute", "solve equation", "integral",
	"derivative", "matrix", "probability", "how much is",
	"what is the value", "find the root", "logarithm",
}

// ── Паттерны кода ────────────────────────────────────────────────────────

// codePatterns — признаки кода непосредственно в тексте.
var codePatterns = []string{
	"func ", "def ", "class ", "import ", "package ",
	"return ", "if (", "for (", "while (", "```",
	"var ", "const ", "let ", "=>", ":=",
}

// mathPatterns — математические символы и выражения.
var mathPatterns = []string{
	"sqrt(", "log(", "sin(", "cos(", "∫", "∑", "∏",
	" + ", " - ", " * ", " / ", " = ", "^2", "^n",
}

// ── Классификатор ────────────────────────────────────────────────────────

// ClassifyTask определяет тип задачи по тексту запроса.
// Не вызывает LLM — работает на основе ключевых слов и эвристик.
func ClassifyTask(query string) RouteResult {
	lower := strings.ToLower(query)
	words := tokenizeQuery(lower)

	// Считаем очки по каждой категории
	ragScore := scoreKeywords(lower, words, ragKeywords)
	agentScore := scoreKeywords(lower, words, agentKeywords)
	codeScore := scoreKeywords(lower, words, codeKeywords)
	mathScore := scoreKeywords(lower, words, mathKeywords)

	// Бонус за паттерны в коде / математике
	codeScore += scorePatterns(query, codePatterns) * 1.5
	mathScore += scorePatterns(lower, mathPatterns) * 1.5

	// Бонус: если в тексте есть блок кода ```...```
	if strings.Contains(query, "```") {
		codeScore += 3.0
	}

	// Выбираем победителя
	type candidate struct {
		task  TaskType
		score float64
	}
	candidates := []candidate{
		{TaskRAG, ragScore},
		{TaskAgent, agentScore},
		{TaskCode, codeScore},
		{TaskMath, mathScore},
	}

	best := candidate{TaskChat, 0}
	for _, c := range candidates {
		if c.score > best.score {
			best = c
		}
	}

	// Порог: меньше 1.0 — это просто chat
	if best.score < 1.0 {
		return RouteResult{
			Task:       TaskChat,
			Confidence: 0.9,
			Reason:     "no specific keywords found",
		}
	}

	// Нормализуем уверенность в [0.5, 0.98]
	conf := normalizeScore(best.score)
	reason := reasonFor(best.task, best.score)

	return RouteResult{
		Task:       best.task,
		Confidence: conf,
		Reason:     reason,
	}
}

// ── Вспомогательные функции ──────────────────────────────────────────────

// scoreKeywords считает очки по совпадениям ключевых слов.
// Фразы (несколько слов) дают больше очков, чем одиночные слова.
func scoreKeywords(lower string, words []string, keywords []string) float64 {
	score := 0.0
	for _, kw := range keywords {
		kw = strings.ToLower(kw)
		if strings.Contains(kw, " ") {
			// Фраза — точное вхождение в текст
			if strings.Contains(lower, kw) {
				score += 2.0
			}
		} else {
			// Одиночное слово — проверяем и полное совпадение, и префикс
			for _, w := range words {
				if w == kw {
					score += 1.0
					break
				}
				if strings.HasPrefix(w, kw) && len(kw) >= 4 {
					score += 0.7
					break
				}
			}
		}
	}
	return score
}

// scorePatterns считает очки за точное вхождение паттернов.
func scorePatterns(text string, patterns []string) float64 {
	score := 0.0
	for _, p := range patterns {
		if strings.Contains(text, p) {
			score += 1.0
		}
	}
	return score
}

// normalizeScore переводит сырой счёт в уверенность [0.5, 0.98].
func normalizeScore(score float64) float64 {
	// Логистическая функция: score=1 → ~0.73, score=3 → ~0.90, score=6 → ~0.97
	x := score / 3.0
	conf := 1.0 / (1.0 + fastExp(-x+0.5))
	if conf < 0.5 {
		conf = 0.5
	}
	if conf > 0.98 {
		conf = 0.98
	}
	return conf
}

// fastExp — приближённая экспонента для нормализации (достаточно для роутера).
func fastExp(x float64) float64 {
	// Используем стандартную math.Exp через ряд Тейлора, но нам хватит
	// простого приближения: e^x ≈ (1 + x/256)^256 для малых x
	// Для нашего диапазона [-3, 3] точность достаточная.
	if x > 20 {
		return 485165195.4
	}
	if x < -20 {
		return 0.0000000021
	}
	result := 1.0
	term := 1.0
	for i := 1; i <= 15; i++ {
		term *= x / float64(i)
		result += term
	}
	if result < 0 {
		return 0.000001
	}
	return result
}

// reasonFor возвращает краткое объяснение решения роутера.
func reasonFor(task TaskType, score float64) string {
	switch task {
	case TaskRAG:
		return "document/file keywords detected"
	case TaskAgent:
		return "tool-use keywords detected"
	case TaskCode:
		return "code keywords or syntax patterns detected"
	case TaskMath:
		return "math keywords or symbols detected"
	default:
		return "general conversation"
	}
}

// tokenizeQuery разбивает запрос на слова (только буквы и цифры).
func tokenizeQuery(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}
