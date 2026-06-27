// tools_data.go — Инструменты работы с данными v4.0.
//
// Инструменты:
//   fetch_page — скрапинг веб-страницы: HTML → чистый читаемый текст
//   memory     — персистентная память агента (ключ-значение, JSON-файл)
//   sqlite     — запросы к SQLite-базе через sqlite3 CLI (если установлен)
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ── Инициализация ─────────────────────────────────────────────────────────

// memoryFile — путь к файлу памяти агента. Устанавливается в runServer().
var memoryFile string

// initDataTools вызывается из runServer для задания пути к файлу памяти.
func initDataTools(dataDir string) {
	memoryFile = filepath.Join(dataDir, "agent_memory.json")
	// Создаём пустой файл памяти если не существует
	if _, err := os.Stat(memoryFile); os.IsNotExist(err) {
		_ = os.WriteFile(memoryFile, []byte("{}"), 0o644)
	}
}

func init() {
	AllTools["fetch_page"] = &ToolDef{
		Name: "fetch_page",
		Description: "Загружает веб-страницу по URL и возвращает чистый читаемый текст " +
			"(HTML-теги убраны). Гораздо лучше http_get для чтения статей и документации. " +
			"Возвращает до 8KB текста.",
		ArgsSchema: `{"url": "https://example.com", "max_kb": 8}`,
		Run:        toolFetchPage,
	}
	AllTools["memory"] = &ToolDef{
		Name: "memory",
		Description: "Персистентная память агента. Помни важные факты между сессиями. " +
			"Операции: set (сохранить), get (прочитать), delete (удалить), list (список ключей), clear (очистить всё).",
		ArgsSchema: `{"op": "set|get|delete|list|clear", "key": "название", "value": "значение (для set)"}`,
		Run:        toolMemory,
	}
	AllTools["sqlite"] = &ToolDef{
		Name: "sqlite",
		Description: "Выполняет SQL-запрос к SQLite-базе данных через sqlite3 CLI. " +
			"Поддерживает SELECT, INSERT, UPDATE, CREATE TABLE и т.д.",
		ArgsSchema: `{"db": "путь/к/database.db", "query": "SELECT * FROM table LIMIT 10"}`,
		Run:        toolSQLite,
	}
}

// ── fetch_page ─────────────────────────────────────────────────────────────

// fetchHTTPClient — HTTP клиент с разумными таймаутами для веб-скрапинга.
var fetchHTTPClient = &http.Client{
	Timeout: 20 * time.Second,
}

func toolFetchPage(args map[string]any) (string, error) {
	rawURL, _ := args["url"].(string)
	if rawURL == "" {
		return "", fmt.Errorf("нужен аргумент 'url'")
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return "", fmt.Errorf("поддерживаются только http:// и https://")
	}

	maxKB := 8
	if v, ok := args["max_kb"].(float64); ok && v > 0 && v <= 64 {
		maxKB = int(v)
	}
	maxBytes := maxKB * 1024

	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("неверный URL: %w", err)
	}
	// Имитируем браузер чтобы не получать 403
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; LocalAI/4.0; +https://github.com/Fipoh455r/Progi)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "ru,en;q=0.9")

	resp, err := fetchHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ошибка запроса: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes*4))) // берём больше чтобы после стрипа осталось достаточно
	if err != nil {
		return "", fmt.Errorf("ошибка чтения: %w", err)
	}

	// Извлекаем текст из HTML
	text := htmlToText(string(body))

	// Ограничиваем вывод
	if len(text) > maxBytes {
		// Обрезаем по полному слову
		text = text[:maxBytes]
		if idx := strings.LastIndex(text, " "); idx > maxBytes-200 {
			text = text[:idx]
		}
		text += "\n[...текст обрезан]"
	}

	if strings.TrimSpace(text) == "" {
		return fmt.Sprintf("(страница не содержит текста, HTTP %d)", resp.StatusCode), nil
	}

	return fmt.Sprintf("URL: %s\n---\n%s", rawURL, text), nil
}

// htmlToText извлекает читаемый текст из HTML.
// Убирает теги, скрипты, стили, нормализует пробелы.
func htmlToText(html string) string {
	// Удаляем <script>...</script>
	html = reRemoveScript.ReplaceAllString(html, " ")
	// Удаляем <style>...</style>
	html = reRemoveStyle.ReplaceAllString(html, " ")
	// Удаляем HTML-комментарии
	html = reRemoveComment.ReplaceAllString(html, " ")
	// Заменяем <br>, <p>, <div>, <li>, <tr>, <h1>-<h6> на переносы строк
	html = reBlockTags.ReplaceAllString(html, "\n")
	// Убираем все оставшиеся теги
	html = reAllTags.ReplaceAllString(html, " ")
	// Декодируем HTML-сущности
	html = decodeHTMLEntities(html)
	// Нормализуем пробелы
	lines := strings.Split(html, "\n")
	var result []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Убираем строки из одних пробелов или слишком короткие
		if len([]rune(line)) > 2 {
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
}

var (
	reRemoveScript  = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reRemoveStyle   = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reRemoveComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	reBlockTags     = regexp.MustCompile(`(?i)<(br|p|div|li|tr|td|th|h[1-6]|blockquote|pre|hr)[^>]*>`)
	reAllTags       = regexp.MustCompile(`<[^>]+>`)
)

// decodeHTMLEntities декодирует основные HTML-сущности.
func decodeHTMLEntities(s string) string {
	replacer := strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&#39;", "'",
		"&apos;", "'",
		"&nbsp;", " ",
		"&mdash;", "—",
		"&ndash;", "–",
		"&laquo;", "«",
		"&raquo;", "»",
		"&hellip;", "…",
		"&copy;", "©",
		"&reg;", "®",
		"&trade;", "™",
	)
	return replacer.Replace(s)
}

// ── memory ─────────────────────────────────────────────────────────────────

var memoryMu sync.Mutex

// loadMemory читает память из файла. Возвращает пустую map если файл не существует.
func loadMemory() (map[string]string, error) {
	memoryMu.Lock()
	defer memoryMu.Unlock()

	if memoryFile == "" {
		return nil, fmt.Errorf("память не инициализирована (сервер не запущен?)")
	}

	data, err := os.ReadFile(memoryFile)
	if os.IsNotExist(err) {
		return make(map[string]string), nil
	}
	if err != nil {
		return nil, fmt.Errorf("чтение памяти: %w", err)
	}

	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		// Файл повреждён — начинаем с чистого листа
		return make(map[string]string), nil
	}
	return m, nil
}

// saveMemory атомарно сохраняет память в файл.
func saveMemory(m map[string]string) error {
	// mu уже захвачен вызывающим кодом
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := memoryFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, memoryFile)
}

func toolMemory(args map[string]any) (string, error) {
	op, _ := args["op"].(string)
	key, _ := args["key"].(string)
	value, _ := args["value"].(string)

	if op == "" {
		return "", fmt.Errorf("нужен аргумент 'op': set|get|delete|list|clear")
	}

	switch strings.ToLower(op) {
	case "set":
		if key == "" || value == "" {
			return "", fmt.Errorf("для set нужны 'key' и 'value'")
		}
		memoryMu.Lock()
		m, err := func() (map[string]string, error) {
			data, err := os.ReadFile(memoryFile)
			if os.IsNotExist(err) {
				return make(map[string]string), nil
			}
			if err != nil {
				return nil, err
			}
			var mm map[string]string
			_ = json.Unmarshal(data, &mm)
			if mm == nil {
				mm = make(map[string]string)
			}
			return mm, nil
		}()
		if err != nil {
			memoryMu.Unlock()
			return "", err
		}
		m[key] = value
		err = saveMemory(m)
		memoryMu.Unlock()
		if err != nil {
			return "", fmt.Errorf("сохранение памяти: %w", err)
		}
		return fmt.Sprintf("Сохранено: %q = %q", key, value), nil

	case "get":
		if key == "" {
			return "", fmt.Errorf("для get нужен 'key'")
		}
		m, err := loadMemory()
		if err != nil {
			return "", err
		}
		val, ok := m[key]
		if !ok {
			return fmt.Sprintf("Ключ %q не найден в памяти", key), nil
		}
		return fmt.Sprintf("%s = %s", key, val), nil

	case "delete":
		if key == "" {
			return "", fmt.Errorf("для delete нужен 'key'")
		}
		memoryMu.Lock()
		data, _ := os.ReadFile(memoryFile)
		var m map[string]string
		_ = json.Unmarshal(data, &m)
		if m == nil {
			m = make(map[string]string)
		}
		if _, ok := m[key]; !ok {
			memoryMu.Unlock()
			return fmt.Sprintf("Ключ %q не найден", key), nil
		}
		delete(m, key)
		err := saveMemory(m)
		memoryMu.Unlock()
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Удалено: %q", key), nil

	case "list":
		m, err := loadMemory()
		if err != nil {
			return "", err
		}
		if len(m) == 0 {
			return "Память пуста", nil
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Память агента (%d записей):\n", len(m)))
		for _, k := range keys {
			v := m[k]
			if utf8.RuneCountInString(v) > 60 {
				runes := []rune(v)
				v = string(runes[:57]) + "…"
			}
			sb.WriteString(fmt.Sprintf("  • %s = %s\n", k, v))
		}
		return sb.String(), nil

	case "clear":
		memoryMu.Lock()
		err := saveMemory(make(map[string]string))
		memoryMu.Unlock()
		if err != nil {
			return "", fmt.Errorf("очистка памяти: %w", err)
		}
		return "Память очищена", nil

	default:
		return "", fmt.Errorf("неизвестная операция %q. Доступны: set, get, delete, list, clear", op)
	}
}

// ── sqlite ─────────────────────────────────────────────────────────────────

// sqliteBlockedStmts — SQL-операторы, которые могут нанести вред файловой системе.
var sqliteBlockedStmts = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bATTACH\b`),   // ATTACH DATABASE 'evil.db'
	regexp.MustCompile(`(?i)\bDETACH\b`),
	regexp.MustCompile(`(?i)\.read\b`),     // .read /etc/passwd
	regexp.MustCompile(`(?i)\.shell\b`),    // .shell rm -rf
	regexp.MustCompile(`(?i)\.system\b`),
}

func toolSQLite(args map[string]any) (string, error) {
	dbPath, _ := args["db"].(string)
	query, _ := args["query"].(string)

	if dbPath == "" || query == "" {
		return "", fmt.Errorf("нужны аргументы 'db' и 'query'")
	}

	// Безопасность: проверяем запрос
	for _, re := range sqliteBlockedStmts {
		if re.MatchString(query) {
			return "", fmt.Errorf("запрос заблокирован по соображениям безопасности")
		}
	}

	// Проверяем наличие sqlite3
	result, err := runWithTimeout("sqlite3", []string{
		"-header", "-column",
		filepath.Clean(dbPath),
		query,
	}, "", 10*time.Second)

	if err != nil {
		// sqlite3 не установлен — предлагаем альтернативу
		if strings.Contains(err.Error(), "не найден") || strings.Contains(err.Error(), "not found") {
			return "", fmt.Errorf(
				"sqlite3 не установлен. Установи: apt install sqlite3 (Debian/Ubuntu) или brew install sqlite (macOS).\n"+
					"Альтернатива: используй run_code с python и import sqlite3",
			)
		}
		return "", err
	}

	if result == "" {
		return "(запрос выполнен, результатов нет)", nil
	}
	return result, nil
}
