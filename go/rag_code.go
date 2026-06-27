// rag_code.go — Языко-осознанный чанкинг кода для RAG.
//
// Проблема стандартного чанкинга по словам:
//   - Функция разрезается посередине → плохой поиск
//   - Нет контекста "к какой функции относится этот код"
//
// Решение: разбивка по логическим единицам языка.
//   - Go/Rust:         разбивка по func/impl
//   - Python:          разбивка по def/class (на уровне отступа 0)
//   - JS/TS:           разбивка по function/class/const arrow
//   - Java/C#/Swift:   разбивка по методам и классам
//   - C/C++:           разбивка по функциям (тип + имя + { )
//   - SQL:             разбивка по statement (;)
//   - Прочее:          разбивка по двойным переносам строк (абзацы)
//
// Чанки снабжаются заголовком «[filename: func_name]» для лучшего поиска.
package main

import (
	"regexp"
	"strings"
)

// maxCodeChunkLines — максимальный размер одного чанка кода в строках.
// Если блок больше — дополнительно разрезаем по пустым строкам.
const maxCodeChunkLines = 60

// codeFileExts — расширения, для которых используем code-aware chunking.
var codeFileExts = map[string]bool{
	".go": true, ".py": true, ".js": true, ".ts": true,
	".jsx": true, ".tsx": true, ".java": true, ".c": true,
	".cpp": true, ".cc": true, ".cxx": true, ".h": true, ".hpp": true,
	".cs": true, ".rb": true, ".rs": true, ".swift": true, ".kt": true,
	".php": true, ".lua": true, ".scala": true, ".hs": true,
	".sql": true, ".sh": true, ".bash": true,
}

// IsCodeFile возвращает true если файл является исходным кодом.
func IsCodeFile(filename string) bool {
	lower := strings.ToLower(filename)
	idx := strings.LastIndex(lower, ".")
	if idx < 0 {
		// Dockerfile, Makefile, Jenkinsfile
		base := lower
		if sl := strings.LastIndex(lower, "/"); sl >= 0 {
			base = lower[sl+1:]
		}
		return base == "dockerfile" || base == "makefile" ||
			base == "gnumakefile" || base == "jenkinsfile"
	}
	return codeFileExts[lower[idx:]]
}

// ChunkCodeFile разбивает исходный код на семантические чанки.
// Использует detectLanguage из tools_code.go (тот же пакет).
// Если язык не определён или файл не является кодом — возвращает nil.
func ChunkCodeFile(text, filename string) []string {
	lang, _ := detectLanguage(text, filename)
	if lang == "Неизвестно" {
		return nil
	}
	blocks := splitByTopLevel(text, lang)
	// Дополнительно дробим слишком большие блоки
	var result []string
	for _, b := range blocks {
		result = append(result, splitLargeBlock(b)...)
	}
	return result
}

// ── Разбивка по верхнеуровневым определениям ─────────────────────────────

// topLevelPatterns — регулярки для детектирования начала нового блока кода.
// Каждый паттерн должен срабатывать в начале строки (^).
var topLevelPatterns = map[string][]*regexp.Regexp{
	"Go": {
		regexp.MustCompile(`^func\s+`),
		regexp.MustCompile(`^type\s+\w+\s+(struct|interface)`),
		regexp.MustCompile(`^var\s+\(`),
		regexp.MustCompile(`^const\s+\(`),
	},
	"Python": {
		regexp.MustCompile(`^def\s+\w+`),
		regexp.MustCompile(`^class\s+\w+`),
		regexp.MustCompile(`^async\s+def\s+\w+`),
	},
	"JavaScript": {
		regexp.MustCompile(`^function\s+\w+`),
		regexp.MustCompile(`^class\s+\w+`),
		regexp.MustCompile(`^const\s+\w+\s*=\s*(async\s+)?(function|\()`),
		regexp.MustCompile(`^export\s+(default\s+)?(function|class|const)\s+`),
		regexp.MustCompile(`^module\.exports\s*=`),
	},
	"TypeScript": {
		regexp.MustCompile(`^function\s+\w+`),
		regexp.MustCompile(`^class\s+\w+`),
		regexp.MustCompile(`^interface\s+\w+`),
		regexp.MustCompile(`^type\s+\w+\s*=`),
		regexp.MustCompile(`^const\s+\w+\s*=\s*(async\s+)?(function|\()`),
		regexp.MustCompile(`^export\s+(default\s+)?(function|class|interface|type|const)\s+`),
	},
	"Rust": {
		regexp.MustCompile(`^(pub\s+)?fn\s+\w+`),
		regexp.MustCompile(`^(pub\s+)?impl\s+`),
		regexp.MustCompile(`^(pub\s+)?struct\s+\w+`),
		regexp.MustCompile(`^(pub\s+)?enum\s+\w+`),
		regexp.MustCompile(`^(pub\s+)?trait\s+\w+`),
	},
	"Java": {
		regexp.MustCompile(`^\s*(public|private|protected|static)\s+`),
		regexp.MustCompile(`^\s*class\s+\w+`),
		regexp.MustCompile(`^\s*interface\s+\w+`),
	},
	"C#": {
		regexp.MustCompile(`^\s*(public|private|protected|internal|static)\s+`),
		regexp.MustCompile(`^\s*(class|interface|struct|enum)\s+\w+`),
		regexp.MustCompile(`^\s*namespace\s+\w+`),
	},
	"C": {
		regexp.MustCompile(`^\w[\w\s\*]+\s+\w+\s*\(`),
		regexp.MustCompile(`^(struct|typedef|enum)\s+`),
		regexp.MustCompile(`^#define\s+\w+`),
	},
	"C++": {
		regexp.MustCompile(`^\w[\w\s\*:<>]+\s+\w+\s*\(`),
		regexp.MustCompile(`^(class|struct|namespace|template)\s+`),
	},
	"Ruby": {
		regexp.MustCompile(`^def\s+\w+`),
		regexp.MustCompile(`^class\s+\w+`),
		regexp.MustCompile(`^module\s+\w+`),
	},
	"PHP": {
		regexp.MustCompile(`^(function|class|interface|trait)\s+\w+`),
		regexp.MustCompile(`^\s*(public|private|protected)\s+function\s+\w+`),
	},
	"Swift": {
		regexp.MustCompile(`^(public\s+|private\s+|internal\s+|open\s+)?(func|class|struct|enum|protocol|extension)\s+\w+`),
		regexp.MustCompile(`^func\s+\w+`),
	},
	"Kotlin": {
		regexp.MustCompile(`^(fun|class|object|interface|data class|sealed class)\s+\w+`),
	},
	"SQL": {
		// SQL разбиваем по ;
	},
	"Bash": {
		regexp.MustCompile(`^\w[\w-]*\s*\(\s*\)\s*\{`),
		regexp.MustCompile(`^function\s+\w+`),
	},
}

// splitByTopLevel разбивает код на блоки по верхнеуровневым определениям.
func splitByTopLevel(text, lang string) []string {
	// SQL: разбиваем по точке с запятой
	if lang == "SQL" {
		return splitSQL(text)
	}

	patterns, ok := topLevelPatterns[lang]
	if !ok || len(patterns) == 0 {
		// Для неизвестных языков: разбиваем по двойным переносам строк
		return splitByBlankLines(text)
	}

	lines := strings.Split(text, "\n")
	var blocks []string
	var current []string

	isTopLevel := func(line string) bool {
		for _, re := range patterns {
			if re.MatchString(line) {
				return true
			}
		}
		return false
	}

	for _, line := range lines {
		if isTopLevel(line) && len(current) > 0 {
			// Начало нового блока — сохраняем предыдущий
			block := strings.TrimSpace(strings.Join(current, "\n"))
			if block != "" {
				blocks = append(blocks, block)
			}
			current = []string{line}
		} else {
			current = append(current, line)
		}
	}
	// Последний блок
	if len(current) > 0 {
		block := strings.TrimSpace(strings.Join(current, "\n"))
		if block != "" {
			blocks = append(blocks, block)
		}
	}

	// Объединяем слишком маленькие блоки (< 5 строк) с соседними
	return mergeSmallBlocks(blocks, 5)
}

// splitSQL разбивает SQL по операторам (разделитель — точка с запятой).
func splitSQL(text string) []string {
	statements := strings.Split(text, ";")
	var result []string
	var pending strings.Builder

	for _, stmt := range statements {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		pending.WriteString(stmt)
		pending.WriteString(";\n")

		// Накапливаем несколько коротких операторов вместе
		if pending.Len() > 500 {
			result = append(result, strings.TrimSpace(pending.String()))
			pending.Reset()
		}
	}
	if pending.Len() > 0 {
		result = append(result, strings.TrimSpace(pending.String()))
	}
	return result
}

// splitByBlankLines разбивает текст по двойным пустым строкам.
func splitByBlankLines(text string) []string {
	parts := strings.Split(text, "\n\n")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// mergeSmallBlocks объединяет блоки короче minLines с предыдущим.
func mergeSmallBlocks(blocks []string, minLines int) []string {
	if len(blocks) == 0 {
		return blocks
	}
	result := []string{blocks[0]}
	for _, b := range blocks[1:] {
		if strings.Count(b, "\n")+1 < minLines {
			// Присоединяем к предыдущему
			result[len(result)-1] += "\n\n" + b
		} else {
			result = append(result, b)
		}
	}
	return result
}

// splitLargeBlock разбивает слишком большой блок на части по maxCodeChunkLines.
func splitLargeBlock(block string) []string {
	lines := strings.Split(block, "\n")
	if len(lines) <= maxCodeChunkLines {
		return []string{block}
	}

	var result []string
	var current []string

	for _, line := range lines {
		current = append(current, line)
		// Разбиваем на границах пустых строк, если достигли лимита
		if len(current) >= maxCodeChunkLines && strings.TrimSpace(line) == "" {
			chunk := strings.TrimSpace(strings.Join(current, "\n"))
			if chunk != "" {
				result = append(result, chunk)
			}
			current = current[:0]
		}
	}
	if len(current) > 0 {
		chunk := strings.TrimSpace(strings.Join(current, "\n"))
		if chunk != "" {
			result = append(result, chunk)
		}
	}
	return result
}
