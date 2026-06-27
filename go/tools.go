// tools.go — встроенные инструменты агента.
//
// Инструменты:
//
//	calculator  — вычисление математических выражений (без CGO)
//	datetime    — текущая дата и время
//	web_search  — поиск через DuckDuckGo (без API-ключа)
//	read_file   — чтение файла с диска
//	write_file  — запись файла на диск
//	http_get    — HTTP GET запрос
//	memory      — долгосрочная память (JSON-файл с фактами о пользователе)
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// memoryDataDir — директория для хранения файла памяти агента.
// Устанавливается через SetMemoryDir при старте сервера.
var memoryDataDir string

// SetMemoryDir инициализирует директорию для хранения памяти агента.
// Должна вызываться до первого использования инструмента memory.
func SetMemoryDir(dataDir string) {
	memoryDataDir = filepath.Join(dataDir, "memory")
}

// ToolDef описывает один инструмент.
type ToolDef struct {
	Name        string
	Description string
	ArgsSchema  string // JSON-схема аргументов (для промпта)
	Run         func(args map[string]any) (string, error)
}

// AllTools — реестр всех доступных инструментов.
var AllTools = map[string]*ToolDef{
	"calculator": {
		Name:        "calculator",
		Description: "Вычисляет математическое выражение. Поддерживает: +−×÷^% sqrt abs sin cos tan log floor ceil round pi e",
		ArgsSchema:  `{"expr": "строка с выражением, например: sqrt(16) + 2^3"}`,
		Run:         toolCalculator,
	},
	"datetime": {
		Name:        "datetime",
		Description: "Возвращает текущую дату, время и день недели",
		ArgsSchema:  `{}`,
		Run:         toolDatetime,
	},
	"web_search": {
		Name:        "web_search",
		Description: "Поиск информации в интернете через DuckDuckGo. Используй когда нужны свежие данные, факты, определения",
		ArgsSchema:  `{"query": "поисковый запрос"}`,
		Run:         toolWebSearch,
	},
	"read_file": {
		Name:        "read_file",
		Description: "Читает текстовый файл с диска. Возвращает содержимое",
		ArgsSchema:  `{"path": "путь к файлу"}`,
		Run:         toolReadFile,
	},
	"write_file": {
		Name:        "write_file",
		Description: "Записывает текст в файл. Создаёт файл если не существует",
		ArgsSchema:  `{"path": "путь к файлу", "content": "содержимое"}`,
		Run:         toolWriteFile,
	},
	"http_get": {
		Name:        "http_get",
		Description: "Выполняет HTTP GET запрос и возвращает тело ответа (до 4KB)",
		ArgsSchema:  `{"url": "https://..."}`,
		Run:         toolHTTPGet,
	},
	"memory": {
		Name:        "memory",
		Description: "Долгосрочная память: сохраняй и загружай факты о пользователе между сессиями",
		ArgsSchema:  `{"action": "save|load|list|delete", "key": "название факта", "value": "значение (только для save)"}`,
		Run:         toolMemory,
	},
}

// ToolsPrompt формирует часть системного промпта с описанием инструментов.
func ToolsPrompt() string {
	var sb strings.Builder
	sb.WriteString("Доступные инструменты (используй ТОЛЬКО когда нужно):\n")
	for _, t := range AllTools {
		sb.WriteString(fmt.Sprintf("• %s: %s\n  Аргументы: %s\n", t.Name, t.Description, t.ArgsSchema))
	}
	return sb.String()
}

// ── calculator ─────────────────────────────────────────────────────────────

func toolCalculator(args map[string]any) (string, error) {
	expr, _ := args["expr"].(string)
	if expr == "" {
		return "", fmt.Errorf("нужен аргумент 'expr'")
	}
	result, err := evalExpr(strings.TrimSpace(expr))
	if err != nil {
		return "", fmt.Errorf("ошибка вычисления: %w", err)
	}
	// Форматируем: если целое — без дробной части
	if result == math.Trunc(result) && !math.IsInf(result, 0) {
		return fmt.Sprintf("%g", result), nil
	}
	return strconv.FormatFloat(result, 'f', 10, 64), nil
}

// evalExpr — рекурсивный десятичный парсер математических выражений.
func evalExpr(expr string) (float64, error) {
	p := &exprParser{input: []rune(strings.ToLower(expr))}
	val, err := p.parseAddSub()
	if err != nil {
		return 0, err
	}
	p.skipWS()
	if p.pos < len(p.input) {
		return 0, fmt.Errorf("неожиданный символ: %q", string(p.input[p.pos:]))
	}
	return val, nil
}

type exprParser struct {
	input []rune
	pos   int
}

func (p *exprParser) skipWS() {
	for p.pos < len(p.input) && unicode.IsSpace(p.input[p.pos]) {
		p.pos++
	}
}

func (p *exprParser) peek() rune {
	p.skipWS()
	if p.pos >= len(p.input) {
		return 0
	}
	return p.input[p.pos]
}

func (p *exprParser) parseAddSub() (float64, error) {
	left, err := p.parseMulDiv()
	if err != nil {
		return 0, err
	}
	for {
		c := p.peek()
		if c != '+' && c != '-' {
			break
		}
		p.pos++
		right, err := p.parseMulDiv()
		if err != nil {
			return 0, err
		}
		if c == '+' {
			left += right
		} else {
			left -= right
		}
	}
	return left, nil
}

func (p *exprParser) parseMulDiv() (float64, error) {
	left, err := p.parsePow()
	if err != nil {
		return 0, err
	}
	for {
		c := p.peek()
		if c != '*' && c != '/' && c != '%' {
			break
		}
		p.pos++
		right, err := p.parsePow()
		if err != nil {
			return 0, err
		}
		switch c {
		case '*':
			left *= right
		case '/':
			if right == 0 {
				return 0, fmt.Errorf("деление на ноль")
			}
			left /= right
		case '%':
			left = math.Mod(left, right)
		}
	}
	return left, nil
}

func (p *exprParser) parsePow() (float64, error) {
	base, err := p.parseUnary()
	if err != nil {
		return 0, err
	}
	if p.peek() == '^' {
		p.pos++
		exp, err := p.parseUnary()
		if err != nil {
			return 0, err
		}
		base = math.Pow(base, exp)
	}
	return base, nil
}

func (p *exprParser) parseUnary() (float64, error) {
	p.skipWS()
	if p.peek() == '-' {
		p.pos++
		v, err := p.parseAtom()
		return -v, err
	}
	if p.peek() == '+' {
		p.pos++
	}
	return p.parseAtom()
}

func (p *exprParser) parseAtom() (float64, error) {
	p.skipWS()
	if p.pos >= len(p.input) {
		return 0, fmt.Errorf("неожиданный конец выражения")
	}

	// Число
	if unicode.IsDigit(p.input[p.pos]) || p.input[p.pos] == '.' {
		return p.parseNumber()
	}

	// Скобки
	if p.input[p.pos] == '(' {
		p.pos++
		v, err := p.parseAddSub()
		if err != nil {
			return 0, err
		}
		p.skipWS()
		if p.pos >= len(p.input) || p.input[p.pos] != ')' {
			return 0, fmt.Errorf("ожидается ')'")
		}
		p.pos++
		return v, nil
	}

	// Идентификатор (функция или константа)
	if unicode.IsLetter(p.input[p.pos]) {
		return p.parseFuncOrConst()
	}

	return 0, fmt.Errorf("неожиданный символ: %q", string(p.input[p.pos]))
}

func (p *exprParser) parseNumber() (float64, error) {
	start := p.pos
	for p.pos < len(p.input) && (unicode.IsDigit(p.input[p.pos]) || p.input[p.pos] == '.') {
		p.pos++
	}
	// Экспоненциальная нотация: 1e5 / 1e-5
	if p.pos < len(p.input) && (p.input[p.pos] == 'e') {
		p.pos++
		if p.pos < len(p.input) && (p.input[p.pos] == '+' || p.input[p.pos] == '-') {
			p.pos++
		}
		for p.pos < len(p.input) && unicode.IsDigit(p.input[p.pos]) {
			p.pos++
		}
	}
	return strconv.ParseFloat(string(p.input[start:p.pos]), 64)
}

func (p *exprParser) parseFuncOrConst() (float64, error) {
	start := p.pos
	for p.pos < len(p.input) && (unicode.IsLetter(p.input[p.pos]) || unicode.IsDigit(p.input[p.pos])) {
		p.pos++
	}
	name := string(p.input[start:p.pos])

	// Константы
	switch name {
	case "pi":
		return math.Pi, nil
	case "e":
		return math.E, nil
	case "inf", "infinity":
		return math.Inf(1), nil
	}

	// Функции: ожидаем скобку
	p.skipWS()
	if p.pos >= len(p.input) || p.input[p.pos] != '(' {
		return 0, fmt.Errorf("неизвестная константа или функция: %q", name)
	}
	p.pos++ // '('
	arg, err := p.parseAddSub()
	if err != nil {
		return 0, err
	}
	p.skipWS()
	if p.pos >= len(p.input) || p.input[p.pos] != ')' {
		return 0, fmt.Errorf("ожидается ')' после %s(...)", name)
	}
	p.pos++ // ')'

	switch name {
	case "sqrt":
		return math.Sqrt(arg), nil
	case "abs":
		return math.Abs(arg), nil
	case "sin":
		return math.Sin(arg), nil
	case "cos":
		return math.Cos(arg), nil
	case "tan":
		return math.Tan(arg), nil
	case "asin":
		return math.Asin(arg), nil
	case "acos":
		return math.Acos(arg), nil
	case "atan":
		return math.Atan(arg), nil
	case "log", "ln":
		return math.Log(arg), nil
	case "log2":
		return math.Log2(arg), nil
	case "log10":
		return math.Log10(arg), nil
	case "floor":
		return math.Floor(arg), nil
	case "ceil":
		return math.Ceil(arg), nil
	case "round":
		return math.Round(arg), nil
	case "exp":
		return math.Exp(arg), nil
	case "sign":
		return math.Copysign(1, arg), nil
	}
	return 0, fmt.Errorf("неизвестная функция: %q", name)
}

// ── datetime ───────────────────────────────────────────────────────────────

func toolDatetime(_ map[string]any) (string, error) {
	now := time.Now()
	weekdays := []string{"воскресенье", "понедельник", "вторник", "среда", "четверг", "пятница", "суббота"}
	months := []string{"", "января", "февраля", "марта", "апреля", "мая", "июня",
		"июля", "августа", "сентября", "октября", "ноября", "декабря"}

	return fmt.Sprintf("%d %s %d года, %s, %02d:%02d:%02d (UTC%+.0f)",
		now.Day(), months[now.Month()], now.Year(),
		weekdays[now.Weekday()],
		now.Hour(), now.Minute(), now.Second(),
		float64(now.UTC().Sub(now).Hours()),
	), nil
}

// ── web_search ─────────────────────────────────────────────────────────────

// ddgResult — поля ответа DuckDuckGo Instant Answer API.
type ddgResult struct {
	Abstract       string `json:"Abstract"`
	AbstractText   string `json:"AbstractText"`
	AbstractSource string `json:"AbstractSource"`
	AbstractURL    string `json:"AbstractURL"`
	Answer         string `json:"Answer"`
	AnswerType     string `json:"AnswerType"`
	Definition     string `json:"Definition"`
	DefinitionURL  string `json:"DefinitionURL"`
	RelatedTopics  []struct {
		Text     string `json:"Text"`
		FirstURL string `json:"FirstURL"`
	} `json:"RelatedTopics"`
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

func toolWebSearch(args map[string]any) (string, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return "", fmt.Errorf("нужен аргумент 'query'")
	}

	apiURL := "https://api.duckduckgo.com/?q=" + url.QueryEscape(query) +
		"&format=json&no_html=1&skip_disambig=1&t=localai"

	resp, err := httpClient.Get(apiURL)
	if err != nil {
		return "", fmt.Errorf("ошибка запроса: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}

	var result ddgResult
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("ошибка разбора ответа: %w", err)
	}

	var parts []string

	// Прямой ответ (например, для вычислений/конвертаций)
	if result.Answer != "" {
		parts = append(parts, "Ответ: "+result.Answer)
	}

	// Краткое описание (Wikipedia и т.п.)
	if result.AbstractText != "" {
		src := ""
		if result.AbstractSource != "" {
			src = " [" + result.AbstractSource + "]"
		}
		parts = append(parts, "Информация"+src+": "+result.AbstractText)
		if result.AbstractURL != "" {
			parts = append(parts, "Источник: "+result.AbstractURL)
		}
	}

	// Определение
	if result.Definition != "" {
		parts = append(parts, "Определение: "+result.Definition)
	}

	// Связанные темы (до 5)
	if len(result.RelatedTopics) > 0 {
		parts = append(parts, "Связанные результаты:")
		max := 5
		if len(result.RelatedTopics) < max {
			max = len(result.RelatedTopics)
		}
		for _, t := range result.RelatedTopics[:max] {
			if t.Text != "" {
				line := "• " + t.Text
				if t.FirstURL != "" {
					line += " (" + t.FirstURL + ")"
				}
				parts = append(parts, line)
			}
		}
	}

	if len(parts) == 0 {
		return fmt.Sprintf("По запросу %q ничего не найдено через Instant Answer API. "+
			"Попробуй сформулировать точнее.", query), nil
	}

	return strings.Join(parts, "\n"), nil
}

// ── read_file ──────────────────────────────────────────────────────────────

func toolReadFile(args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return "", fmt.Errorf("нужен аргумент 'path'")
	}

	// Базовая защита от path traversal
	clean := filepath.Clean(path)
	if strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("недопустимый путь: %q", path)
	}

	data, err := os.ReadFile(clean)
	if err != nil {
		return "", fmt.Errorf("не удалось прочитать файл %q: %w", path, err)
	}

	// Ограничиваем вывод — иначе контекст переполнится
	const maxBytes = 8 * 1024
	if len(data) > maxBytes {
		return fmt.Sprintf("[Файл обрезан до %d KB из %d KB]\n%s",
			maxBytes/1024, len(data)/1024, string(data[:maxBytes])), nil
	}

	return string(data), nil
}

// ── write_file ─────────────────────────────────────────────────────────────

func toolWriteFile(args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
	if path == "" {
		return "", fmt.Errorf("нужен аргумент 'path'")
	}

	clean := filepath.Clean(path)
	if strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("недопустимый путь: %q", path)
	}

	// Создаём директории при необходимости
	if dir := filepath.Dir(clean); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("не удалось создать директорию: %w", err)
		}
	}

	if err := os.WriteFile(clean, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("не удалось записать файл: %w", err)
	}

	return fmt.Sprintf("Файл %q записан (%d байт)", path, len(content)), nil
}

// ── http_get ───────────────────────────────────────────────────────────────

func toolHTTPGet(args map[string]any) (string, error) {
	rawURL, _ := args["url"].(string)
	if rawURL == "" {
		return "", fmt.Errorf("нужен аргумент 'url'")
	}

	// Только HTTP/HTTPS
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return "", fmt.Errorf("поддерживаются только http:// и https://")
	}

	resp, err := httpClient.Get(rawURL)
	if err != nil {
		return "", fmt.Errorf("HTTP ошибка: %w", err)
	}
	defer resp.Body.Close()

	// Ограничиваем ответ 4KB
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, string(body)), nil
}

// ── RunTool ────────────────────────────────────────────────────────────────

// ── memory ─────────────────────────────────────────────────────────────────

// memoryStore — мьютекс для безопасного доступа к файлу памяти.
var memoryMu sync.Mutex

// memoryFacts — структура файла памяти.
type memoryFacts struct {
	Facts map[string]string `json:"facts"`
}

// memoryFilePath возвращает путь к файлу памяти.
func memoryFilePath() (string, error) {
	if memoryDataDir == "" {
		return "", fmt.Errorf("память недоступна: сервер не инициализирован (запусти localai serve)")
	}
	return filepath.Join(memoryDataDir, "facts.json"), nil
}

// loadFacts читает факты с диска.
func loadFacts() (memoryFacts, error) {
	path, err := memoryFilePath()
	if err != nil {
		return memoryFacts{}, err
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return memoryFacts{Facts: make(map[string]string)}, nil
	}
	if err != nil {
		return memoryFacts{}, fmt.Errorf("чтение памяти: %w", err)
	}

	var mf memoryFacts
	if err := json.Unmarshal(data, &mf); err != nil {
		return memoryFacts{Facts: make(map[string]string)}, nil
	}
	if mf.Facts == nil {
		mf.Facts = make(map[string]string)
	}
	return mf, nil
}

// saveFacts записывает факты на диск (атомарно через temp-файл).
func saveFacts(mf memoryFacts) error {
	path, err := memoryFilePath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("создание директории памяти: %w", err)
	}

	data, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// toolMemory реализует операции над долгосрочной памятью агента.
//
//	save  — сохранить факт:  {"action":"save","key":"имя","value":"Иван"}
//	load  — загрузить факт:  {"action":"load","key":"имя"}
//	list  — все факты:       {"action":"list"}
//	delete — удалить факт:  {"action":"delete","key":"имя"}
func toolMemory(args map[string]any) (string, error) {
	action, _ := args["action"].(string)
	key, _ := args["key"].(string)
	value, _ := args["value"].(string)

	if action == "" {
		return "", fmt.Errorf("нужен аргумент 'action': save | load | list | delete")
	}

	memoryMu.Lock()
	defer memoryMu.Unlock()

	mf, err := loadFacts()
	if err != nil {
		return "", err
	}

	switch action {
	case "save":
		if key == "" {
			return "", fmt.Errorf("нужен аргумент 'key'")
		}
		if value == "" {
			return "", fmt.Errorf("нужен аргумент 'value'")
		}
		mf.Facts[key] = value
		if err := saveFacts(mf); err != nil {
			return "", fmt.Errorf("сохранение факта: %w", err)
		}
		return fmt.Sprintf("Запомнено: %s = %s", key, value), nil

	case "load":
		if key == "" {
			// Без ключа — все факты
			return formatFacts(mf.Facts), nil
		}
		v, ok := mf.Facts[key]
		if !ok {
			return fmt.Sprintf("Факт %q не найден в памяти", key), nil
		}
		return fmt.Sprintf("%s: %s", key, v), nil

	case "list":
		return formatFacts(mf.Facts), nil

	case "delete":
		if key == "" {
			return "", fmt.Errorf("нужен аргумент 'key'")
		}
		if _, ok := mf.Facts[key]; !ok {
			return fmt.Sprintf("Факт %q не найден", key), nil
		}
		delete(mf.Facts, key)
		if err := saveFacts(mf); err != nil {
			return "", fmt.Errorf("удаление факта: %w", err)
		}
		return fmt.Sprintf("Факт %q удалён", key), nil

	default:
		return "", fmt.Errorf("неизвестное действие %q; допустимо: save, load, list, delete", action)
	}
}

// formatFacts форматирует карту фактов в читаемую строку.
func formatFacts(facts map[string]string) string {
	if len(facts) == 0 {
		return "Память пуста"
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Фактов в памяти: %d\n", len(facts)))
	for k, v := range facts {
		sb.WriteString(fmt.Sprintf("• %s: %s\n", k, v))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// RunTool выполняет инструмент по имени с заданными аргументами.
func RunTool(name string, args map[string]any) (string, error) {
	tool, ok := AllTools[name]
	if !ok {
		available := make([]string, 0, len(AllTools))
		for k := range AllTools {
			available = append(available, k)
		}
		return "", fmt.Errorf("инструмент %q не найден. Доступны: %s",
			name, strings.Join(available, ", "))
	}
	return tool.Run(args)
}
