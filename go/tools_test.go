// tools_test.go — unit-тесты для инструментов агента.
package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── calculator ────────────────────────────────────────────────────────────────

func TestEvalExpr_Basic(t *testing.T) {
	cases := []struct {
		expr string
		want float64
	}{
		{"2 + 2", 4},
		{"10 - 3", 7},
		{"3 * 4", 12},
		{"10 / 4", 2.5},
		{"2 ^ 10", 1024},
		{"10 % 3", 1},
		{"-5 + 3", -2},
		{"(2 + 3) * 4", 20},
	}

	for _, tc := range cases {
		got, err := evalExpr(tc.expr)
		if err != nil {
			t.Errorf("evalExpr(%q) вернул ошибку: %v", tc.expr, err)
			continue
		}
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("evalExpr(%q) = %v, ожидалось %v", tc.expr, got, tc.want)
		}
	}
}

func TestEvalExpr_Functions(t *testing.T) {
	cases := []struct {
		expr string
		want float64
		eps  float64
	}{
		{"sqrt(16)", 4, 1e-9},
		{"abs(-7)", 7, 1e-9},
		{"floor(3.9)", 3, 1e-9},
		{"ceil(3.1)", 4, 1e-9},
		{"round(3.5)", 4, 1e-9},
		{"sin(0)", 0, 1e-9},
		{"cos(0)", 1, 1e-9},
		{"log(1)", 0, 1e-9},
		{"exp(0)", 1, 1e-9},
		{"log10(100)", 2, 1e-9},
	}

	for _, tc := range cases {
		got, err := evalExpr(tc.expr)
		if err != nil {
			t.Errorf("evalExpr(%q) вернул ошибку: %v", tc.expr, err)
			continue
		}
		if math.Abs(got-tc.want) > tc.eps {
			t.Errorf("evalExpr(%q) = %v, ожидалось %v", tc.expr, got, tc.want)
		}
	}
}

func TestEvalExpr_Constants(t *testing.T) {
	pi, err := evalExpr("pi")
	if err != nil {
		t.Fatalf("evalExpr(\"pi\") ошибка: %v", err)
	}
	if math.Abs(pi-math.Pi) > 1e-9 {
		t.Errorf("pi = %v, ожидалось %v", pi, math.Pi)
	}

	e, err := evalExpr("e")
	if err != nil {
		t.Fatalf("evalExpr(\"e\") ошибка: %v", err)
	}
	if math.Abs(e-math.E) > 1e-9 {
		t.Errorf("e = %v, ожидалось %v", e, math.E)
	}
}

func TestEvalExpr_Errors(t *testing.T) {
	badExprs := []string{
		"10 / 0",
		"sqrt(",
		"unknownfn(1)",
		"",
	}

	for _, expr := range badExprs {
		_, err := evalExpr(expr)
		if err == nil {
			t.Errorf("evalExpr(%q) должна вернуть ошибку", expr)
		}
	}
}

func TestToolCalculator(t *testing.T) {
	result, err := toolCalculator(map[string]any{"expr": "sqrt(144)"})
	if err != nil {
		t.Fatalf("toolCalculator: %v", err)
	}
	if result != "12" {
		t.Errorf("результат = %q, ожидалось %q", result, "12")
	}

	_, err = toolCalculator(map[string]any{})
	if err == nil {
		t.Error("пустые аргументы должны вернуть ошибку")
	}
}

// ── read_file / write_file ────────────────────────────────────────────────────

func TestToolReadWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	content := "привет мир\nстрока 2\n"

	// Запись
	result, err := toolWriteFile(map[string]any{"path": path, "content": content})
	if err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if !strings.Contains(result, "записан") {
		t.Errorf("ожидалось 'записан' в ответе: %q", result)
	}

	// Чтение
	got, err := toolReadFile(map[string]any{"path": path})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if got != content {
		t.Errorf("содержимое = %q, ожидалось %q", got, content)
	}
}

func TestToolReadFile_NotFound(t *testing.T) {
	_, err := toolReadFile(map[string]any{"path": "/tmp/не_существует_12345.txt"})
	if err == nil {
		t.Error("должна вернуть ошибку для несуществующего файла")
	}
}

func TestToolReadFile_PathTraversal(t *testing.T) {
	_, err := toolReadFile(map[string]any{"path": "../../etc/passwd"})
	if err == nil {
		t.Error("path traversal должен быть заблокирован")
	}
}

func TestToolWriteFile_MkdirAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "dir", "file.txt")

	_, err := toolWriteFile(map[string]any{"path": path, "content": "test"})
	if err != nil {
		t.Fatalf("write_file с созданием директорий: %v", err)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("файл не создан")
	}
}

// ── RunTool ───────────────────────────────────────────────────────────────────

func TestRunTool_UnknownTool(t *testing.T) {
	_, err := RunTool("no_such_tool", map[string]any{})
	if err == nil {
		t.Error("должна вернуть ошибку для неизвестного инструмента")
	}
	if !strings.Contains(err.Error(), "не найден") {
		t.Errorf("ошибка должна содержать 'не найден': %v", err)
	}
}

func TestRunTool_KnownTools(t *testing.T) {
	knownTools := []string{"calculator", "datetime", "read_file", "write_file", "http_get", "web_search"}
	for _, name := range knownTools {
		if _, ok := AllTools[name]; !ok {
			t.Errorf("инструмент %q не зарегистрирован в AllTools", name)
		}
	}
}

// ── ToolsPrompt ───────────────────────────────────────────────────────────────

func TestToolsPrompt(t *testing.T) {
	prompt := ToolsPrompt()
	for _, name := range []string{"calculator", "datetime", "web_search"} {
		if !strings.Contains(prompt, name) {
			t.Errorf("ToolsPrompt не содержит инструмент %q", name)
		}
	}
}
