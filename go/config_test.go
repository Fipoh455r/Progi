// config_test.go — unit-тесты для загрузки YAML-конфигурации.
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// ── DefaultConfig ─────────────────────────────────────────────────────────────

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.OllamaURL == "" {
		t.Error("OllamaURL не должен быть пустым")
	}
	if cfg.Port == "" {
		t.Error("Port не должен быть пустым")
	}
	if cfg.Model == "" {
		t.Error("Model не должен быть пустым")
	}
	if !cfg.MetricsEnabled {
		t.Error("MetricsEnabled должен быть true по умолчанию")
	}
}

// ── LoadConfig ────────────────────────────────────────────────────────────────

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "localai-*.yaml")
	if err != nil {
		t.Fatalf("создание temp-файла: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("запись yaml: %v", err)
	}
	return f.Name()
}

func TestLoadConfig_Basic(t *testing.T) {
	path := writeYAML(t, `
# комментарий
ollama_url: http://myserver:11434
model: llama3.2:1b
port: "9090"
data_dir: /tmp/data
metrics_enabled: false
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.OllamaURL != "http://myserver:11434" {
		t.Errorf("OllamaURL = %q", cfg.OllamaURL)
	}
	if cfg.Model != "llama3.2:1b" {
		t.Errorf("Model = %q", cfg.Model)
	}
	if cfg.Port != "9090" {
		t.Errorf("Port = %q", cfg.Port)
	}
	if cfg.DataDir != "/tmp/data" {
		t.Errorf("DataDir = %q", cfg.DataDir)
	}
	if cfg.MetricsEnabled {
		t.Error("MetricsEnabled должен быть false")
	}
}

func TestLoadConfig_EmptyValues(t *testing.T) {
	path := writeYAML(t, `
ollama_url: http://localhost:11434
piper_bin: ""
jwt_secret: ""
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.PiperBin != "" {
		t.Errorf("PiperBin должен быть пустым, получили %q", cfg.PiperBin)
	}
	if cfg.JWTSecret != "" {
		t.Errorf("JWTSecret должен быть пустым, получили %q", cfg.JWTSecret)
	}
}

func TestLoadConfig_Comments(t *testing.T) {
	path := writeYAML(t, `
# Это комментарий
ollama_url: http://localhost:11434 # инлайн комментарий
model: qwen2.5:0.5b
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.OllamaURL != "http://localhost:11434" {
		t.Errorf("инлайн комментарий не отрезан: %q", cfg.OllamaURL)
	}
}

func TestLoadConfig_NotFound(t *testing.T) {
	_, err := LoadConfig("/tmp/не_существует_77777.yaml")
	if err == nil {
		t.Error("должна вернуть ошибку если файл не найден")
	}
}

func TestLoadConfig_UnknownKeysIgnored(t *testing.T) {
	// Неизвестные ключи должны молча игнорироваться
	path := writeYAML(t, `
ollama_url: http://localhost:11434
unknown_key: some_value
another_key: 42
`)
	_, err := LoadConfig(path)
	if err != nil {
		t.Errorf("неизвестные ключи не должны вызывать ошибку: %v", err)
	}
}

// ── WriteExample ──────────────────────────────────────────────────────────────

func TestWriteExample(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "localai.yaml")

	if err := WriteExample(path); err != nil {
		t.Fatalf("WriteExample: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("файл не создан: %v", err)
	}
	if info.Size() == 0 {
		t.Error("файл пустой")
	}

	// Должен быть валидным YAML
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("WriteExample создал невалидный YAML: %v", err)
	}
	if cfg.OllamaURL == "" {
		t.Error("OllamaURL должен быть задан в примере")
	}
}

// ── parseBool ─────────────────────────────────────────────────────────────────

func TestParseBool(t *testing.T) {
	trueVals := []string{"true", "True", "TRUE", "yes", "Yes", "1", "on", "On"}
	for _, v := range trueVals {
		if !parseBool(v, false) {
			t.Errorf("parseBool(%q) должен возвращать true", v)
		}
	}

	falseVals := []string{"false", "False", "FALSE", "no", "No", "0", "off", "Off"}
	for _, v := range falseVals {
		if parseBool(v, true) {
			t.Errorf("parseBool(%q) должен возвращать false", v)
		}
	}

	// Неизвестное значение → fallback
	if !parseBool("maybe", true) {
		t.Error("parseBool('maybe') должен вернуть fallback=true")
	}
}

// ── MergeEnv ──────────────────────────────────────────────────────────────────

func TestMergeEnv(t *testing.T) {
	t.Setenv("LOCALAI_PORT", "9999")
	t.Setenv("LOCALAI_MODEL", "test-model")

	cfg := DefaultConfig()
	cfg.MergeEnv()

	if cfg.Port != "9999" {
		t.Errorf("Port = %q, ожидалось 9999", cfg.Port)
	}
	if cfg.Model != "test-model" {
		t.Errorf("Model = %q, ожидалось test-model", cfg.Model)
	}
}
