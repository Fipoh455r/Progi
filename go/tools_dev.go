// tools_dev.go — Инструменты разработчика v3.9.
//
// Инструменты:
//   git           — git status/log/diff/add/commit/push/clone
//   http_request  — полный HTTP-клиент (GET/POST/PUT/PATCH/DELETE + заголовки)
//   json_query    — парсинг и запросы к JSON (JQ-стиль: .field, .arr[0], keys, values)
//   diff          — сравнение двух строк/файлов построчно
//   regex         — match / find_all / replace по регулярному выражению
//   encode        — base64/URL encode и decode, MD5/SHA256 хэш
package main

import (
	"bytes"
	"crypto/md5"  //nolint:gosec // MD5 используется для неcryptographic нужд (хэш файлов)
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func init() {
	AllTools["git"] = &ToolDef{
		Name: "git",
		Description: "Выполняет git-команду в указанной директории. " +
			"Поддерживает: status, log, diff, add, commit, push, pull, clone, branch, checkout",
		ArgsSchema: `{"subcommand": "status|log|diff|add|commit|push|pull|clone|branch|checkout", "args": ["аргументы..."], "workdir": "путь (опционально)"}`,
		Run:        toolGit,
	}
	AllTools["http_request"] = &ToolDef{
		Name: "http_request",
		Description: "Полный HTTP-клиент. Поддерживает GET/POST/PUT/PATCH/DELETE. " +
			"Можно передавать заголовки и JSON-тело. Ответ до 8KB.",
		ArgsSchema: `{"method": "GET|POST|PUT|PATCH|DELETE", "url": "https://...", "headers": {"key": "value"}, "body": "строка или объект"}`,
		Run:        toolHTTPRequest,
	}
	AllTools["json_query"] = &ToolDef{
		Name: "json_query",
		Description: "Парсит JSON и извлекает данные по пути. " +
			"Путь: '.field', '.arr[0]', '.a.b.c', 'keys', 'values', 'length', 'pretty'",
		ArgsSchema: `{"json": "JSON-строка или объект", "path": ".field.subfield или keys/values/length/pretty"}`,
		Run:        toolJSONQuery,
	}
	AllTools["diff"] = &ToolDef{
		Name:        "diff",
		Description: "Сравнивает два текста построчно. Показывает добавленные (+) и удалённые (-) строки.",
		ArgsSchema:  `{"a": "первый текст или путь к файлу", "b": "второй текст или путь к файлу", "context": 3}`,
		Run:         toolDiff,
	}
	AllTools["regex"] = &ToolDef{
		Name:        "regex",
		Description: "Операции с регулярными выражениями: match (совпадает?), find_all (все совпадения), replace (замена).",
		ArgsSchema:  `{"op": "match|find_all|replace", "pattern": "регулярное выражение", "text": "текст", "replacement": "замена (для replace)"}`,
		Run:         toolRegex,
	}
	AllTools["encode"] = &ToolDef{
		Name:        "encode",
		Description: "Кодирование/декодирование и хэширование: base64_encode, base64_decode, url_encode, url_decode, md5, sha256.",
		ArgsSchema:  `{"op": "base64_encode|base64_decode|url_encode|url_decode|md5|sha256", "text": "входные данные"}`,
		Run:         toolEncode,
	}
}

// ── git ───────────────────────────────────────────────────────────────────

// allowedGitSubcmds — разрешённые подкоманды git.
var allowedGitSubcmds = map[string]bool{
	"status": true, "log": true, "diff": true, "add": true,
	"commit": true, "push": true, "pull": true, "clone": true,
	"branch": true, "checkout": true, "show": true, "stash": true,
	"fetch": true, "merge": true, "rebase": true, "remote": true,
	"tag": true, "describe": true, "rev-parse": true, "shortlog": true,
}

func toolGit(args map[string]any) (string, error) {
	subcmd, _ := args["subcommand"].(string)
	workdir, _ := args["workdir"].(string)

	if subcmd == "" {
		return "", fmt.Errorf("нужен аргумент 'subcommand'")
	}
	subcmd = strings.TrimSpace(strings.ToLower(subcmd))
	if !allowedGitSubcmds[subcmd] {
		allowed := make([]string, 0, len(allowedGitSubcmds))
		for k := range allowedGitSubcmds {
			allowed = append(allowed, k)
		}
		return "", fmt.Errorf("git %q не поддерживается. Доступны: %s",
			subcmd, strings.Join(allowed, ", "))
	}

	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git не установлен в системе")
	}

	// Собираем аргументы
	cmdArgs := []string{subcmd}

	// args["args"] может быть []any (из JSON)
	switch v := args["args"].(type) {
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok {
				cmdArgs = append(cmdArgs, s)
			}
		}
	case []string:
		cmdArgs = append(cmdArgs, v...)
	case string:
		if v != "" {
			cmdArgs = append(cmdArgs, strings.Fields(v)...)
		}
	}

	// Ограничиваем объём вывода для log/diff
	if subcmd == "log" {
		hasFmt := false
		for _, a := range cmdArgs {
			if strings.HasPrefix(a, "--format") || strings.HasPrefix(a, "--pretty") {
				hasFmt = true
				break
			}
		}
		if !hasFmt {
			cmdArgs = append([]string{"log", "--oneline", "-20"}, cmdArgs[1:]...)
		}
	}

	return runWithTimeout("git", cmdArgs, workdir, 30*time.Second)
}

// ── http_request ──────────────────────────────────────────────────────────

var httpDevClient = &http.Client{Timeout: 15 * time.Second}

func toolHTTPRequest(args map[string]any) (string, error) {
	method, _ := args["method"].(string)
	rawURL, _ := args["url"].(string)

	if rawURL == "" {
		return "", fmt.Errorf("нужен аргумент 'url'")
	}
	if method == "" {
		method = "GET"
	}
	method = strings.ToUpper(strings.TrimSpace(method))

	allowed := map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true}
	if !allowed[method] {
		return "", fmt.Errorf("метод %q не поддерживается", method)
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return "", fmt.Errorf("поддерживаются только http:// и https://")
	}

	// Тело запроса
	var bodyReader io.Reader
	switch v := args["body"].(type) {
	case string:
		if v != "" {
			bodyReader = strings.NewReader(v)
		}
	case map[string]any:
		data, _ := json.Marshal(v)
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, rawURL, bodyReader)
	if err != nil {
		return "", fmt.Errorf("ошибка создания запроса: %w", err)
	}

	// Заголовки
	if headers, ok := args["headers"].(map[string]any); ok {
		for k, v := range headers {
			if s, ok := v.(string); ok {
				req.Header.Set(k, s)
			}
		}
	}
	// Content-Type по умолчанию для POST/PUT/PATCH с телом
	if bodyReader != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "LocalAI/3.9")

	resp, err := httpDevClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP ошибка: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	if err != nil {
		return "", err
	}

	// Форматируем заголовки ответа
	var headerLines []string
	for k, vs := range resp.Header {
		headerLines = append(headerLines, fmt.Sprintf("%s: %s", k, strings.Join(vs, ", ")))
	}

	result := fmt.Sprintf("HTTP %d %s\n", resp.StatusCode, resp.Status)
	if len(headerLines) > 0 && len(headerLines) <= 10 {
		result += strings.Join(headerLines, "\n") + "\n"
	}
	result += "\n" + string(body)
	return result, nil
}

// ── json_query ────────────────────────────────────────────────────────────

func toolJSONQuery(args map[string]any) (string, error) {
	path, _ := args["path"].(string)

	// Получаем JSON из аргумента (строка или объект)
	var data any
	switch v := args["json"].(type) {
	case string:
		if err := json.Unmarshal([]byte(v), &data); err != nil {
			return "", fmt.Errorf("неверный JSON: %w", err)
		}
	case map[string]any:
		data = v
	case []any:
		data = v
	case nil:
		return "", fmt.Errorf("нужен аргумент 'json'")
	default:
		data = v
	}

	if path == "" || path == "." || path == "pretty" {
		out, _ := json.MarshalIndent(data, "", "  ")
		return string(out), nil
	}

	result, err := jsonNavigate(data, path)
	if err != nil {
		return "", err
	}

	// Форматируем вывод
	switch r := result.(type) {
	case string:
		return r, nil
	case float64:
		if r == float64(int64(r)) {
			return fmt.Sprintf("%d", int64(r)), nil
		}
		return strconv.FormatFloat(r, 'f', -1, 64), nil
	case bool:
		return strconv.FormatBool(r), nil
	case nil:
		return "null", nil
	default:
		out, _ := json.MarshalIndent(result, "", "  ")
		return string(out), nil
	}
}

// jsonNavigate навигирует по JSON по пути вида '.a.b[0].c', 'keys', 'values', 'length'.
func jsonNavigate(data any, path string) (any, error) {
	path = strings.TrimSpace(path)

	// Специальные операции
	switch path {
	case "keys":
		m, ok := data.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("keys требует объект, получено: %T", data)
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		return keys, nil
	case "values":
		m, ok := data.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("values требует объект, получено: %T", data)
		}
		vals := make([]any, 0, len(m))
		for _, v := range m {
			vals = append(vals, v)
		}
		return vals, nil
	case "length":
		switch v := data.(type) {
		case []any:
			return float64(len(v)), nil
		case map[string]any:
			return float64(len(v)), nil
		case string:
			return float64(len(v)), nil
		}
		return nil, fmt.Errorf("length: неподдерживаемый тип %T", data)
	}

	// Нормализуем путь: убираем ведущую точку
	if strings.HasPrefix(path, ".") {
		path = path[1:]
	}
	if path == "" {
		return data, nil
	}

	// Разбиваем путь на сегменты по точкам и квадратным скобкам
	segments := splitJSONPath(path)
	current := data
	for _, seg := range segments {
		if seg == "" {
			continue
		}
		// Индекс массива: [N]
		if strings.HasPrefix(seg, "[") && strings.HasSuffix(seg, "]") {
			idxStr := seg[1 : len(seg)-1]
			idx, err := strconv.Atoi(idxStr)
			if err != nil {
				return nil, fmt.Errorf("неверный индекс: %q", seg)
			}
			arr, ok := current.([]any)
			if !ok {
				return nil, fmt.Errorf("ожидался массив, получено: %T", current)
			}
			if idx < 0 {
				idx = len(arr) + idx
			}
			if idx < 0 || idx >= len(arr) {
				return nil, fmt.Errorf("индекс %d выходит за пределы массива длиной %d", idx, len(arr))
			}
			current = arr[idx]
			continue
		}
		// Поле объекта
		m, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("ожидался объект для поля %q, получено: %T", seg, current)
		}
		val, exists := m[seg]
		if !exists {
			return nil, fmt.Errorf("поле %q не найдено", seg)
		}
		current = val
	}
	return current, nil
}

// splitJSONPath разбивает путь 'a.b[0].c' на сегменты ['a','b','[0]','c'].
func splitJSONPath(path string) []string {
	var segments []string
	var cur strings.Builder
	for _, r := range path {
		switch r {
		case '.':
			if cur.Len() > 0 {
				segments = append(segments, cur.String())
				cur.Reset()
			}
		case '[':
			if cur.Len() > 0 {
				segments = append(segments, cur.String())
				cur.Reset()
			}
			cur.WriteRune(r)
		case ']':
			cur.WriteRune(r)
			segments = append(segments, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		segments = append(segments, cur.String())
	}
	return segments
}

// ── diff ─────────────────────────────────────────────────────────────────

func toolDiff(args map[string]any) (string, error) {
	aRaw, _ := args["a"].(string)
	bRaw, _ := args["b"].(string)
	contextLines := 3
	if v, ok := args["context"].(float64); ok {
		contextLines = int(v)
	}

	if aRaw == "" || bRaw == "" {
		return "", fmt.Errorf("нужны аргументы 'a' и 'b'")
	}

	// Если выглядит как путь к файлу — читаем файл
	aText := readIfFile(aRaw)
	bText := readIfFile(bRaw)

	return unifiedDiff(aText, bText, aRaw, bRaw, contextLines), nil
}

// readIfFile читает файл, если путь существует; иначе возвращает строку как есть.
func readIfFile(s string) string {
	if len(s) < 256 && !strings.ContainsAny(s, "\n\r") {
		if data, err := os.ReadFile(s); err == nil {
			return string(data)
		}
	}
	return s
}

// unifiedDiff генерирует unified diff двух текстов без внешних зависимостей.
func unifiedDiff(a, b, labelA, labelB string, ctx int) string {
	aLines := strings.Split(a, "\n")
	bLines := strings.Split(b, "\n")

	if a == b {
		return "(файлы идентичны)"
	}

	// Используем простой Myers diff: O(nd) алгоритм упрощённо
	edits := myersDiff(aLines, bLines)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("--- %s\n+++ %s\n", labelA, labelB))

	// Группируем изменения в hunks с контекстом
	type hunk struct {
		aStart, bStart int
		lines          []diffEdit
	}

	var hunks []hunk
	var pending []diffEdit
	aLine, bLine := 1, 1
	hunkAStart, hunkBStart := 1, 1

	flushHunk := func() {
		if len(pending) > 0 {
			hunks = append(hunks, hunk{hunkAStart, hunkBStart, pending})
			pending = nil
		}
	}

	for _, e := range edits {
		switch e.op {
		case ' ':
			pending = append(pending, e)
			aLine++
			bLine++
		case '-':
			if len(pending) == 0 {
				hunkAStart = aLine
				hunkBStart = bLine
			}
			pending = append(pending, e)
			aLine++
		case '+':
			if len(pending) == 0 {
				hunkAStart = aLine
				hunkBStart = bLine
			}
			pending = append(pending, e)
			bLine++
		}
	}
	flushHunk()

	// Форматируем hunks
	for _, h := range hunks {
		// Считаем строки a и b в hunk
		aCount, bCount := 0, 0
		for _, e := range h.lines {
			if e.op == ' ' || e.op == '-' {
				aCount++
			}
			if e.op == ' ' || e.op == '+' {
				bCount++
			}
		}
		sb.WriteString(fmt.Sprintf("@@ -%d,%d +%d,%d @@\n", h.aStart, aCount, h.bStart, bCount))
		// Показываем только строки вокруг изменений (ctx строк контекста)
		for _, e := range trimContext(h.lines, ctx) {
			switch e.op {
			case ' ':
				sb.WriteString(" " + e.text + "\n")
			case '-':
				sb.WriteString("-" + e.text + "\n")
			case '+':
				sb.WriteString("+" + e.text + "\n")
			}
		}
	}

	result := sb.String()
	const maxDiff = 8 * 1024
	if len(result) > maxDiff {
		return result[:maxDiff] + "\n[...diff обрезан]"
	}
	return result
}

type diffEdit struct {
	op   byte
	text string
}

// myersDiff — упрощённый Myers diff для строк.
func myersDiff(a, b []string) []diffEdit {
	// Используем DP LCS подход (O(mn) память, простой для реализации)
	m, n := len(a), len(b)
	// lcs[i][j] = длина LCS a[:i] и b[:j]
	lcs := make([][]int, m+1)
	for i := range lcs {
		lcs[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if a[i-1] == b[j-1] {
				lcs[i][j] = lcs[i-1][j-1] + 1
			} else if lcs[i-1][j] >= lcs[i][j-1] {
				lcs[i][j] = lcs[i-1][j]
			} else {
				lcs[i][j] = lcs[i][j-1]
			}
		}
	}

	// Восстанавливаем путь
	var edits []diffEdit
	var backtrack func(i, j int)
	backtrack = func(i, j int) {
		if i == 0 && j == 0 {
			return
		}
		if i > 0 && j > 0 && a[i-1] == b[j-1] {
			backtrack(i-1, j-1)
			edits = append(edits, diffEdit{' ', a[i-1]})
		} else if j > 0 && (i == 0 || lcs[i][j-1] >= lcs[i-1][j]) {
			backtrack(i, j-1)
			edits = append(edits, diffEdit{'+', b[j-1]})
		} else {
			backtrack(i-1, j)
			edits = append(edits, diffEdit{'-', a[i-1]})
		}
	}
	backtrack(m, n)
	return edits
}

// trimContext оставляет только строки вокруг изменений (±ctx строк контекста).
func trimContext(edits []diffEdit, ctx int) []diffEdit {
	// Находим индексы строк с изменениями
	changed := make([]bool, len(edits))
	for i, e := range edits {
		if e.op != ' ' {
			changed[i] = true
		}
	}
	// Расширяем зону контекста
	include := make([]bool, len(edits))
	for i, c := range changed {
		if c {
			for j := max(0, i-ctx); j <= min(len(edits)-1, i+ctx); j++ {
				include[j] = true
			}
		}
	}
	var result []diffEdit
	for i, e := range edits {
		if include[i] {
			result = append(result, e)
		}
	}
	return result
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ── regex ─────────────────────────────────────────────────────────────────

func toolRegex(args map[string]any) (string, error) {
	op, _ := args["op"].(string)
	pattern, _ := args["pattern"].(string)
	text, _ := args["text"].(string)
	replacement, _ := args["replacement"].(string)

	if op == "" || pattern == "" {
		return "", fmt.Errorf("нужны аргументы 'op' и 'pattern'")
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("неверное регулярное выражение: %w", err)
	}

	switch strings.ToLower(op) {
	case "match":
		if re.MatchString(text) {
			match := re.FindString(text)
			return fmt.Sprintf("совпадение найдено: %q", match), nil
		}
		return "совпадений не найдено", nil

	case "find_all":
		matches := re.FindAllString(text, -1)
		if len(matches) == 0 {
			return "совпадений не найдено", nil
		}
		lines := make([]string, len(matches))
		for i, m := range matches {
			lines[i] = fmt.Sprintf("%d: %q", i+1, m)
		}
		return fmt.Sprintf("Найдено %d совпадений:\n%s", len(matches), strings.Join(lines, "\n")), nil

	case "find_groups":
		matches := re.FindAllStringSubmatch(text, -1)
		if len(matches) == 0 {
			return "совпадений не найдено", nil
		}
		var sb strings.Builder
		for i, m := range matches {
			sb.WriteString(fmt.Sprintf("Совпадение %d:\n", i+1))
			for j, g := range m {
				if j == 0 {
					sb.WriteString(fmt.Sprintf("  полное: %q\n", g))
				} else {
					sb.WriteString(fmt.Sprintf("  группа %d: %q\n", j, g))
				}
			}
		}
		return sb.String(), nil

	case "replace":
		result := re.ReplaceAllString(text, replacement)
		return result, nil

	case "split":
		parts := re.Split(text, -1)
		lines := make([]string, len(parts))
		for i, p := range parts {
			lines[i] = fmt.Sprintf("%d: %q", i+1, p)
		}
		return fmt.Sprintf("Разбито на %d частей:\n%s", len(parts), strings.Join(lines, "\n")), nil

	default:
		return "", fmt.Errorf("неизвестная операция: %q. Доступны: match, find_all, find_groups, replace, split", op)
	}
}

// ── encode ────────────────────────────────────────────────────────────────

func toolEncode(args map[string]any) (string, error) {
	op, _ := args["op"].(string)
	text, _ := args["text"].(string)

	if op == "" || text == "" {
		return "", fmt.Errorf("нужны аргументы 'op' и 'text'")
	}

	switch strings.ToLower(op) {
	case "base64_encode":
		return base64.StdEncoding.EncodeToString([]byte(text)), nil

	case "base64_decode":
		data, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			// Пробуем URL-safe base64
			data, err = base64.URLEncoding.DecodeString(text)
			if err != nil {
				return "", fmt.Errorf("ошибка декодирования base64: %w", err)
			}
		}
		return string(data), nil

	case "url_encode":
		return url.QueryEscape(text), nil

	case "url_decode":
		decoded, err := url.QueryUnescape(text)
		if err != nil {
			return "", fmt.Errorf("ошибка URL-декодирования: %w", err)
		}
		return decoded, nil

	case "md5":
		//nolint:gosec // MD5 для хэширования файлов/данных, не для криптографии
		h := md5.Sum([]byte(text))
		return fmt.Sprintf("%x", h), nil

	case "sha256":
		h := sha256.Sum256([]byte(text))
		return fmt.Sprintf("%x", h), nil

	case "hex_encode":
		return fmt.Sprintf("%x", []byte(text)), nil

	case "hex_decode":
		var result []byte
		for i := 0; i+1 < len(text); i += 2 {
			var b byte
			if _, err := fmt.Sscanf(text[i:i+2], "%02x", &b); err != nil {
				return "", fmt.Errorf("неверный hex на позиции %d", i)
			}
			result = append(result, b)
		}
		return string(result), nil

	default:
		return "", fmt.Errorf("неизвестная операция: %q. Доступны: base64_encode, base64_decode, url_encode, url_decode, md5, sha256, hex_encode, hex_decode", op)
	}
}
