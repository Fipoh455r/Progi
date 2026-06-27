// config.go — загрузка конфигурации из YAML-файла (stdlib only).
//
// Формат файла: плоский YAML (ключ: значение), без вложенности.
// Пример файла создаётся командой: localai config init
//
// Приоритет: CLI-флаг > переменная окружения > config-файл > умолчание.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// AppConfig хранит все настройки приложения.
type AppConfig struct {
	// Основные
	OllamaURL  string `yaml:"ollama_url"`
	Model      string `yaml:"model"`
	Port       string `yaml:"port"`
	DataDir    string `yaml:"data_dir"`
	ConfigFile string `yaml:"-"` // путь к самому файлу конфига, не сохраняется

	// Голос
	WhisperURL     string `yaml:"whisper_url"`
	PiperBin       string `yaml:"piper_bin"`
	PiperVoicesDir string `yaml:"piper_voices_dir"`
	PiperVoice     string `yaml:"piper_voice"`

	// Кластер
	OllamaNodes string `yaml:"ollama_nodes"` // запятая-разделённый список URL

	// Метрики
	MetricsEnabled bool `yaml:"metrics_enabled"`

	// Авторизация
	JWTSecret string `yaml:"jwt_secret"` // пустая = auto-generate

	// Логирование
	LogFile string `yaml:"log_file"` // путь к лог-файлу; пустая = только stderr
}

// DefaultConfig возвращает конфигурацию с умолчаниями.
func DefaultConfig() AppConfig {
	return AppConfig{
		OllamaURL:      "http://localhost:11434",
		Model:          "qwen2.5:0.5b",
		Port:           "8080",
		DataDir:        "./data",
		WhisperURL:     "http://localhost:8081",
		PiperBin:       "",
		PiperVoicesDir: "",
		PiperVoice:     "en_US-lessac-medium",
		OllamaNodes:    "",
		MetricsEnabled: true,
		JWTSecret:      "",
	}
}

// LoadConfig читает YAML-файл и возвращает конфиг.
// Поддерживаемый подмножество YAML: плоские пары ключ: значение,
// комментарии (#), булевы значения (true/false), строки (с кавычками или без).
func LoadConfig(path string) (AppConfig, error) {
	cfg := DefaultConfig()
	cfg.ConfigFile = path

	f, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer f.Close()

	kv, err := parseSimpleYAML(f)
	if err != nil {
		return cfg, fmt.Errorf("ошибка разбора %s: %w", path, err)
	}

	// Применяем значения из файла
	if v, ok := kv["ollama_url"]; ok {
		cfg.OllamaURL = v
	}
	if v, ok := kv["model"]; ok {
		cfg.Model = v
	}
	if v, ok := kv["port"]; ok {
		cfg.Port = v
	}
	if v, ok := kv["data_dir"]; ok {
		cfg.DataDir = v
	}
	if v, ok := kv["whisper_url"]; ok {
		cfg.WhisperURL = v
	}
	if v, ok := kv["piper_bin"]; ok {
		cfg.PiperBin = v
	}
	if v, ok := kv["piper_voices_dir"]; ok {
		cfg.PiperVoicesDir = v
	}
	if v, ok := kv["piper_voice"]; ok {
		cfg.PiperVoice = v
	}
	if v, ok := kv["ollama_nodes"]; ok {
		cfg.OllamaNodes = v
	}
	if v, ok := kv["jwt_secret"]; ok {
		cfg.JWTSecret = v
	}
	if v, ok := kv["metrics_enabled"]; ok {
		cfg.MetricsEnabled = parseBool(v, true)
	}
	if v, ok := kv["log_file"]; ok {
		cfg.LogFile = v
	}

	return cfg, nil
}

// MergeEnv переписывает поля из переменных окружения (если заданы).
func (c *AppConfig) MergeEnv() {
	if v := os.Getenv("OLLAMA_URL"); v != "" {
		c.OllamaURL = v
	}
	if v := os.Getenv("LOCALAI_MODEL"); v != "" {
		c.Model = v
	}
	if v := os.Getenv("LOCALAI_PORT"); v != "" {
		c.Port = v
	}
	if v := os.Getenv("LOCALAI_DATA"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("LOCALAI_WHISPER_URL"); v != "" {
		c.WhisperURL = v
	}
	if v := os.Getenv("LOCALAI_PIPER_BIN"); v != "" {
		c.PiperBin = v
	}
	if v := os.Getenv("LOCALAI_PIPER_VOICES_DIR"); v != "" {
		c.PiperVoicesDir = v
	}
	if v := os.Getenv("LOCALAI_PIPER_VOICE"); v != "" {
		c.PiperVoice = v
	}
	if v := os.Getenv("LOCALAI_OLLAMA_NODES"); v != "" {
		c.OllamaNodes = v
	}
	if v := os.Getenv("LOCALAI_JWT_SECRET"); v != "" {
		c.JWTSecret = v
	}
	if v := os.Getenv("LOCALAI_METRICS_ENABLED"); v != "" {
		c.MetricsEnabled = parseBool(v, c.MetricsEnabled)
	}
	if v := os.Getenv("LOCALAI_LOG_FILE"); v != "" {
		c.LogFile = v
	}
}

// WriteExample создаёт пример config-файла по указанному пути.
func WriteExample(path string) error {
	const template = `# LocalAI — файл конфигурации
# Приоритет: CLI-флаги > переменные окружения > этот файл > умолчания
# Строки можно писать без кавычек или в двойных кавычках.

# Основные настройки
ollama_url: http://localhost:11434
model: qwen2.5:0.5b
port: "8080"
data_dir: ./data

# Кластеризация: несколько нод Ollama через запятую
# ollama_nodes: http://node1:11434,http://node2:11434

# Голосовые функции (Whisper STT + piper TTS)
whisper_url: http://localhost:8081
piper_bin: ""
piper_voices_dir: ""
piper_voice: en_US-lessac-medium

# Метрики (Prometheus на /metrics)
metrics_enabled: true

# JWT-секрет: оставь пустым — ключ будет сгенерирован автоматически
jwt_secret: ""

# Лог-файл: путь к файлу; пусто = только stderr; ротация при >10MB
# log_file: /var/log/localai/app.log
`
	return os.WriteFile(path, []byte(template), 0o644)
}

// ── Вспомогательные ──────────────────────────────────────────────────────────

// parseSimpleYAML разбирает плоский YAML формата "ключ: значение".
// Поддерживаются: комментарии (#), пустые строки, строки с/без кавычек.
func parseSimpleYAML(f *os.File) (map[string]string, error) {
	result := make(map[string]string)
	scanner := bufio.NewScanner(f)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		// Пропускаем пустые строки и комментарии
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			return nil, fmt.Errorf("строка %d: нет двоеточия: %q", lineNum, line)
		}

		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])

		// Убираем inline-комментарий (# после значения)
		if ci := strings.Index(val, " #"); ci >= 0 {
			val = strings.TrimSpace(val[:ci])
		}

		// Снимаем кавычки
		val = unquote(val)

		if key != "" {
			result[key] = val
		}
	}

	return result, scanner.Err()
}

// unquote снимает двойные кавычки (если есть).
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		u, err := strconv.Unquote(s)
		if err == nil {
			return u
		}
		return s[1 : len(s)-1]
	}
	return s
}

// parseBool разбирает булево значение; при ошибке возвращает fallback.
func parseBool(s string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "1", "on":
		return true
	case "false", "no", "0", "off":
		return false
	}
	return fallback
}
