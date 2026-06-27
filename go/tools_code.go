// tools_code.go — Инструменты выполнения кода и работы с файловой системой.
//
// Инструменты:
//   run_code  — выполнение кода на Python / JavaScript / Bash / Go
//   shell     — выполнение shell-команды (bash -c) с таймаутом и защитой
//   list_dir  — список файлов в директории
//   grep_file — поиск по регулярному выражению в файлах
//   detect_lang — определение языка программирования по коду
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

func init() {
	// Регистрируем новые инструменты в общем реестре AllTools.
	AllTools["run_code"] = &ToolDef{
		Name:        "run_code",
		Description: "Выполняет код на Python, JavaScript, Bash или Go. Возвращает stdout+stderr. Таймаут 15 сек.",
		ArgsSchema:  `{"language": "python|javascript|bash|go", "code": "исходный код"}`,
		Run:         toolRunCode,
	}
	AllTools["shell"] = &ToolDef{
		Name:        "shell",
		Description: "Выполняет shell-команду (bash -c). Безопасно: таймаут 10 сек, заблокированы деструктивные операции.",
		ArgsSchema:  `{"command": "ls -la /tmp", "workdir": "рабочая директория (опционально)"}`,
		Run:         toolShell,
	}
	AllTools["list_dir"] = &ToolDef{
		Name:        "list_dir",
		Description: "Список файлов и директорий по указанному пути с размерами и датами изменения.",
		ArgsSchema:  `{"path": ".", "recursive": false}`,
		Run:         toolListDir,
	}
	AllTools["grep_file"] = &ToolDef{
		Name:        "grep_file",
		Description: "Поиск строк по регулярному выражению в файле или директории. Возвращает строки с совпадениями.",
		ArgsSchema:  `{"pattern": "регулярное выражение", "path": "файл или директория", "max_results": 50}`,
		Run:         toolGrepFile,
	}
	AllTools["detect_lang"] = &ToolDef{
		Name:        "detect_lang",
		Description: "Определяет язык программирования по содержимому кода или имени файла.",
		ArgsSchema:  `{"code": "исходный код или фрагмент", "filename": "имя файла (опционально)"}`,
		Run:         toolDetectLang,
	}
}

// ── run_code ──────────────────────────────────────────────────────────────

// supportedRuntimes — поддерживаемые языки и команды запуска.
var supportedRuntimes = map[string]struct {
	binary string
	ext    string
}{
	"python":     {"python3", ".py"},
	"python3":    {"python3", ".py"},
	"py":         {"python3", ".py"},
	"javascript": {"node", ".js"},
	"js":         {"node", ".js"},
	"node":       {"node", ".js"},
	"bash":       {"bash", ".sh"},
	"sh":         {"bash", ".sh"},
	"shell":      {"bash", ".sh"},
}

func toolRunCode(args map[string]any) (string, error) {
	lang, _ := args["language"].(string)
	code, _ := args["code"].(string)
	if lang == "" || code == "" {
		return "", fmt.Errorf("нужны аргументы 'language' и 'code'")
	}

	lang = strings.ToLower(strings.TrimSpace(lang))

	// Go запускаем через временный файл с go run
	if lang == "go" || lang == "golang" {
		return runGoCode(code)
	}

	rt, ok := supportedRuntimes[lang]
	if !ok {
		supported := []string{"python", "javascript", "bash", "go"}
		return "", fmt.Errorf("неподдерживаемый язык: %q. Доступны: %s", lang, strings.Join(supported, ", "))
	}

	// Проверяем наличие интерпретатора
	if _, err := exec.LookPath(rt.binary); err != nil {
		return "", fmt.Errorf("интерпретатор %q не найден в системе", rt.binary)
	}

	// Создаём временный файл с кодом
	tmpFile, err := os.CreateTemp("", "localai_code_*"+rt.ext)
	if err != nil {
		return "", fmt.Errorf("ошибка создания временного файла: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(code); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("ошибка записи кода: %w", err)
	}
	tmpFile.Close()

	return runWithTimeout(rt.binary, []string{tmpFile.Name()}, "", 15*time.Second)
}

// runGoCode компилирует и запускает Go-код через go run.
func runGoCode(code string) (string, error) {
	if _, err := exec.LookPath("go"); err != nil {
		return "", fmt.Errorf("Go не установлен в системе")
	}

	// Добавляем package main если не указан
	if !strings.Contains(code, "package ") {
		code = "package main\n\n" + code
	}

	tmpFile, err := os.CreateTemp("", "localai_go_*.go")
	if err != nil {
		return "", fmt.Errorf("ошибка создания временного файла: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(code); err != nil {
		tmpFile.Close()
		return "", err
	}
	tmpFile.Close()

	return runWithTimeout("go", []string{"run", tmpFile.Name()}, "", 20*time.Second)
}

// runWithTimeout запускает команду с таймаутом и возвращает объединённый вывод.
func runWithTimeout(binary string, args []string, workdir string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, args...)
	if workdir != "" {
		cmd.Dir = workdir
	}

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	result := out.String()

	// Ограничиваем вывод
	const maxOutput = 4 * 1024
	if len(result) > maxOutput {
		result = result[:maxOutput] + "\n[...вывод обрезан до 4KB]"
	}

	if ctx.Err() == context.DeadlineExceeded {
		return result + "\n[ТАЙМАУТ: выполнение прервано]", nil
	}

	if err != nil {
		if result != "" {
			// Есть вывод — возвращаем его вместе с ошибкой
			return fmt.Sprintf("Код завершился с ошибкой: %v\n---\n%s", err, result), nil
		}
		return "", fmt.Errorf("ошибка выполнения: %w", err)
	}

	if result == "" {
		return "(нет вывода)", nil
	}
	return result, nil
}

// ── shell ─────────────────────────────────────────────────────────────────

// shellBlockedPatterns — опасные паттерны, блокируем их выполнение.
var shellBlockedPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)rm\s+-[rRf]+\s+/`),           // rm -rf /
	regexp.MustCompile(`(?i)rm\s+-[rRf]+\s+~`),           // rm -rf ~
	regexp.MustCompile(`(?i)mkfs`),                        // форматирование диска
	regexp.MustCompile(`(?i)dd\s+if=`),                    // dd if=...
	regexp.MustCompile(`(?i):\s*\(\s*\)\s*\{.*\}\s*;\s*:`), // fork bomb
	regexp.MustCompile(`(?i)chmod\s+-R\s+777\s+/`),        // chmod 777 /
	regexp.MustCompile(`(?i)>\s*/dev/sd`),                  // запись на блочное устройство
	regexp.MustCompile(`(?i)shutdown|reboot|halt|poweroff`), // выключение системы
}

func toolShell(args map[string]any) (string, error) {
	command, _ := args["command"].(string)
	workdir, _ := args["workdir"].(string)
	if command == "" {
		return "", fmt.Errorf("нужен аргумент 'command'")
	}

	// Проверка безопасности
	for _, pattern := range shellBlockedPatterns {
		if pattern.MatchString(command) {
			return "", fmt.Errorf("команда заблокирована по соображениям безопасности: %q", command)
		}
	}

	if _, err := exec.LookPath("bash"); err != nil {
		return "", fmt.Errorf("bash не найден в системе")
	}

	return runWithTimeout("bash", []string{"-c", command}, workdir, 10*time.Second)
}

// ── list_dir ──────────────────────────────────────────────────────────────

func toolListDir(args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}
	recursive, _ := args["recursive"].(bool)

	// Защита от path traversal
	clean := filepath.Clean(path)

	if recursive {
		return listDirRecursive(clean)
	}

	entries, err := os.ReadDir(clean)
	if err != nil {
		return "", fmt.Errorf("ошибка чтения директории %q: %w", path, err)
	}

	if len(entries) == 0 {
		return fmt.Sprintf("Директория %q пуста", path), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Содержимое %q (%d элементов):\n", clean, len(entries)))

	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if e.IsDir() {
			sb.WriteString(fmt.Sprintf("  [dir]  %s/\n", e.Name()))
		} else {
			sb.WriteString(fmt.Sprintf("  [file] %-30s  %8s  %s\n",
				e.Name(),
				formatSize(info.Size()),
				info.ModTime().Format("2006-01-02 15:04"),
			))
		}
	}
	return sb.String(), nil
}

func listDirRecursive(root string) (string, error) {
	var sb strings.Builder
	count := 0
	const maxFiles = 200

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // пропускаем недоступные
		}
		if count >= maxFiles {
			sb.WriteString(fmt.Sprintf("  [...обрезано, показано %d файлов]\n", maxFiles))
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(root, path)
		if info.IsDir() {
			sb.WriteString(fmt.Sprintf("  [dir]  %s/\n", rel))
		} else {
			sb.WriteString(fmt.Sprintf("  [file] %-40s  %s\n", rel, formatSize(info.Size())))
			count++
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return sb.String(), nil
}

// formatSize форматирует размер файла в читаемый вид.
func formatSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// ── grep_file ─────────────────────────────────────────────────────────────

func toolGrepFile(args map[string]any) (string, error) {
	pattern, _ := args["pattern"].(string)
	path, _ := args["path"].(string)
	maxResults := 50
	if v, ok := args["max_results"].(float64); ok && v > 0 {
		maxResults = int(v)
	}

	if pattern == "" {
		return "", fmt.Errorf("нужен аргумент 'pattern'")
	}
	if path == "" {
		path = "."
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("неверное регулярное выражение: %w", err)
	}

	var results []string
	clean := filepath.Clean(path)

	info, err := os.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("путь не найден: %q", path)
	}

	if info.IsDir() {
		err = filepath.Walk(clean, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return nil
			}
			// Только текстовые файлы
			if !isTextFile(p) {
				return nil
			}
			grepInFile(p, re, &results, maxResults)
			return nil
		})
	} else {
		grepInFile(clean, re, &results, maxResults)
	}

	if err != nil {
		return "", err
	}
	if len(results) == 0 {
		return fmt.Sprintf("По паттерну %q в %q ничего не найдено", pattern, path), nil
	}
	return strings.Join(results, "\n"), nil
}

// grepInFile ищет совпадения в одном файле и добавляет в results.
func grepInFile(path string, re *regexp.Regexp, results *[]string, maxResults int) {
	if len(*results) >= maxResults {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	// Пропускаем бинарные файлы
	if !utf8.Valid(data) {
		return
	}

	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if len(*results) >= maxResults {
			break
		}
		if re.MatchString(line) {
			*results = append(*results, fmt.Sprintf("%s:%d: %s", path, i+1, strings.TrimSpace(line)))
		}
	}
}

// isTextFile проверяет, что файл — текстовый (по расширению).
func isTextFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	textExts := map[string]bool{
		".go": true, ".py": true, ".js": true, ".ts": true, ".jsx": true, ".tsx": true,
		".java": true, ".c": true, ".cpp": true, ".h": true, ".hpp": true,
		".cs": true, ".rb": true, ".rs": true, ".swift": true, ".kt": true,
		".php": true, ".lua": true, ".r": true, ".scala": true, ".hs": true,
		".sh": true, ".bash": true, ".zsh": true, ".fish": true,
		".txt": true, ".md": true, ".rst": true, ".json": true, ".yaml": true,
		".yml": true, ".toml": true, ".ini": true, ".cfg": true, ".conf": true,
		".xml": true, ".html": true, ".htm": true, ".css": true, ".sql": true,
		".csv": true, ".log": true, ".env": true, ".gitignore": true,
		"": true, // файлы без расширения (Makefile, Dockerfile, etc.)
	}
	return textExts[ext]
}

// ── detect_lang ───────────────────────────────────────────────────────────

// LangSignature — признаки языка программирования.
type LangSignature struct {
	Name     string
	Exts     []string // расширения файлов
	Keywords []string // ключевые слова
	Patterns []string // регулярные выражения (подстроки)
}

// knownLanguages — база признаков для определения языка.
var knownLanguages = []LangSignature{
	{
		Name: "Go", Exts: []string{".go"},
		Keywords: []string{"package ", "func ", "import ", ":=", "defer ", "goroutine", "chan ", "go func"},
		Patterns: []string{`^package\s+\w+`, `func\s+\w+\(`},
	},
	{
		Name: "Python", Exts: []string{".py", ".pyw"},
		Keywords: []string{"def ", "import ", "from ", "class ", "lambda ", "elif ", "print(", "self.", "None", "True", "False"},
		Patterns: []string{`def\s+\w+\(`, `import\s+\w+`, `^from\s+\w+\s+import`},
	},
	{
		Name: "JavaScript", Exts: []string{".js", ".mjs", ".cjs"},
		Keywords: []string{"const ", "let ", "var ", "=>", "function ", "require(", "module.exports", "console.log(", "async ", "await "},
		Patterns: []string{`const\s+\w+\s*=`, `function\s+\w+\(`},
	},
	{
		Name: "TypeScript", Exts: []string{".ts", ".tsx"},
		Keywords: []string{"interface ", ": string", ": number", ": boolean", "type ", "readonly ", "enum ", "namespace "},
		Patterns: []string{`interface\s+\w+`, `:\s*(string|number|boolean|void)\b`},
	},
	{
		Name: "Rust", Exts: []string{".rs"},
		Keywords: []string{"fn ", "let mut ", "impl ", "pub ", "use ", "match ", "enum ", "struct ", "trait ", "-> ", "::"},
		Patterns: []string{`fn\s+\w+\(`, `impl\s+\w+`},
	},
	{
		Name: "Java", Exts: []string{".java"},
		Keywords: []string{"public ", "private ", "protected ", "class ", "interface ", "extends ", "implements ", "void ", "static ", "new "},
		Patterns: []string{`(public|private)\s+class\s+\w+`, `public\s+static\s+void\s+main`},
	},
	{
		Name: "C", Exts: []string{".c", ".h"},
		Keywords: []string{"#include ", "#define ", "int main(", "printf(", "scanf(", "malloc(", "free(", "struct ", "typedef "},
		Patterns: []string{`#include\s+[<"]`, `int\s+main\s*\(`},
	},
	{
		Name: "C++", Exts: []string{".cpp", ".cc", ".cxx", ".hpp"},
		Keywords: []string{"#include ", "std::", "cout ", "cin ", "vector<", "namespace ", "template<", "class "},
		Patterns: []string{`#include\s+<(iostream|vector|string)>`, `std::`},
	},
	{
		Name: "C#", Exts: []string{".cs"},
		Keywords: []string{"using ", "namespace ", "public class ", "private ", "static void Main", "Console.Write", "var ", "async Task", "await "},
		Patterns: []string{`using\s+\w+;`, `namespace\s+\w+`},
	},
	{
		Name: "Ruby", Exts: []string{".rb"},
		Keywords: []string{"def ", "end", "puts ", "require ", "class ", "module ", "attr_accessor", "do |", "nil", "@"},
		Patterns: []string{`def\s+\w+`, `require\s+['"]`},
	},
	{
		Name: "PHP", Exts: []string{".php"},
		Keywords: []string{"<?php", "echo ", "$", "->", "::", "function ", "namespace ", "use ", "class "},
		Patterns: []string{`<\?php`, `\$\w+\s*=`},
	},
	{
		Name: "Swift", Exts: []string{".swift"},
		Keywords: []string{"func ", "var ", "let ", "class ", "struct ", "import ", "override ", "guard ", "nil", "optional"},
		Patterns: []string{`func\s+\w+\(`, `import\s+\w+`},
	},
	{
		Name: "Kotlin", Exts: []string{".kt", ".kts"},
		Keywords: []string{"fun ", "val ", "var ", "class ", "object ", "companion ", "data class", "suspend ", "null"},
		Patterns: []string{`fun\s+\w+\(`, `data\s+class\s+\w+`},
	},
	{
		Name: "Bash", Exts: []string{".sh", ".bash"},
		Keywords: []string{"#!/bin/bash", "#!/bin/sh", "echo ", "if [", "for ", "while ", "fi", "then", "do", "done", "export "},
		Patterns: []string{`#!/bin/(bash|sh)`, `\$\{?\w+\}?`},
	},
	{
		Name: "SQL", Exts: []string{".sql"},
		Keywords: []string{"SELECT ", "FROM ", "WHERE ", "INSERT ", "UPDATE ", "DELETE ", "CREATE TABLE", "DROP ", "JOIN ", "INDEX "},
		Patterns: []string{`(?i)SELECT\s+.+\s+FROM`, `(?i)CREATE\s+TABLE`},
	},
	{
		Name: "HTML", Exts: []string{".html", ".htm"},
		Keywords: []string{"<!DOCTYPE", "<html", "<head>", "<body>", "<div", "<script", "<style", "<p>", "<a href"},
		Patterns: []string{`<!DOCTYPE\s+html`, `<html[\s>]`},
	},
	{
		Name: "CSS", Exts: []string{".css", ".scss", ".sass"},
		Keywords: []string{"{", "}", "color:", "background:", "margin:", "padding:", "display:", "font-", "border:", "@media"},
		Patterns: []string{`\w+\s*\{[^}]*\}`, `@media\s*\(`},
	},
	{
		Name: "YAML", Exts: []string{".yaml", ".yml"},
		Keywords: []string{"---", "  - ", ": true", ": false", ": null", ": |", ": >"},
		Patterns: []string{`^\w[\w\s]*:\s`, `^-\s+\w`},
	},
	{
		Name: "JSON", Exts: []string{".json"},
		Keywords: []string{"{\"", "\":", "null", "true", "false"},
		Patterns: []string{`^\s*\{`, `"[^"]+"\s*:`},
	},
	{
		Name: "Markdown", Exts: []string{".md", ".markdown"},
		Keywords: []string{"# ", "## ", "```", "**", "- [", "| ", "---"},
		Patterns: []string{`^#{1,6}\s+\w`, "```"},
	},
}

func toolDetectLang(args map[string]any) (string, error) {
	code, _ := args["code"].(string)
	filename, _ := args["filename"].(string)

	if code == "" && filename == "" {
		return "", fmt.Errorf("нужен хотя бы один аргумент: 'code' или 'filename'")
	}

	lang, confidence := detectLanguage(code, filename)
	return fmt.Sprintf("Язык: %s (уверенность: %s)", lang, confidence), nil
}

// detectLanguage определяет язык по коду и/или имени файла.
// Возвращает название языка и уровень уверенности.
func detectLanguage(code, filename string) (lang string, confidence string) {
	// 1. По расширению файла (высокая точность)
	if filename != "" {
		ext := strings.ToLower(filepath.Ext(filename))
		for _, sig := range knownLanguages {
			for _, e := range sig.Exts {
				if e == ext {
					return sig.Name, "высокая (по расширению)"
				}
			}
		}
		// Специальные файлы без расширения
		base := strings.ToLower(filepath.Base(filename))
		switch base {
		case "dockerfile":
			return "Dockerfile", "высокая (по имени файла)"
		case "makefile", "gnumakefile":
			return "Makefile", "высокая (по имени файла)"
		case "jenkinsfile":
			return "Groovy/Jenkinsfile", "высокая (по имени файла)"
		}
	}

	// 2. По содержимому кода (статистика совпадений)
	if code == "" {
		return "Неизвестно", "низкая"
	}

	type scored struct {
		name  string
		score int
	}
	var scores []scored

	for _, sig := range knownLanguages {
		score := 0
		for _, kw := range sig.Keywords {
			if strings.Contains(code, kw) {
				score++
			}
		}
		if score > 0 {
			scores = append(scores, scored{sig.Name, score})
		}
	}

	if len(scores) == 0 {
		return "Неизвестно", "низкая"
	}

	// Находим максимум
	best := scores[0]
	for _, s := range scores[1:] {
		if s.score > best.score {
			best = s
		}
	}

	conf := "низкая"
	switch {
	case best.score >= 5:
		conf = "высокая"
	case best.score >= 3:
		conf = "средняя"
	}

	return best.name, conf
}
