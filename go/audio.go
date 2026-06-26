// audio.go — голосовой ввод/вывод v2.3
//
// STT (Speech-to-Text):
//   WhisperClient проксирует запросы к Whisper-совместимому HTTP-сервису.
//   Поддерживаемые форматы входного аудио: webm, ogg, wav, mp4, m4a, mp3, flac.
//   Совместимые сервисы: whisper.cpp (--server), faster-whisper, whisper-asr-webservice.
//   Запуск whisper.cpp-сервера:
//     docker run -p 8081:8080 onerahmet/openai-whisper-asr-webservice:latest
//
// TTS (Text-to-Speech):
//   Вызывает бинарник piper через os/exec.
//   Установка: https://github.com/rhasspy/piper
//   Пример: piper --model /data/voices/en_US-lessac-medium.onnx --output-raw < text.txt > audio.raw
//
// Конфигурация:
//   LOCALAI_WHISPER_URL  — URL Whisper-сервиса (умолч: http://localhost:8081)
//   LOCALAI_PIPER_BIN    — путь к бинарнику piper (умолч: ищем в PATH)
//   LOCALAI_PIPER_VOICE  — модель голоса (умолч: en_US-lessac-medium)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ── WhisperClient ─────────────────────────────────────────────────────────────

// WhisperClient — клиент для Whisper-совместимого STT-сервиса.
type WhisperClient struct {
	baseURL string
	http    *http.Client
}

// NewWhisperClient создаёт клиент с заданным базовым URL.
func NewWhisperClient(baseURL string) *WhisperClient {
	return &WhisperClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 120 * time.Second},
	}
}

// IsAvailable проверяет, доступен ли Whisper-сервис.
func (c *WhisperClient) IsAvailable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// whisperResponse — ответ Whisper-совместимого API.
type whisperResponse struct {
	Text string `json:"text"`
}

// Transcribe отправляет аудио в Whisper-сервис и возвращает расшифровку.
//
// audioData — сырые байты аудиофайла.
// filename  — имя файла (нужно для MIME-типа).
// model     — имя модели (например, "whisper-1" или "base").
// language  — язык транскрипции (например, "ru"), пустая строка = авто.
func (c *WhisperClient) Transcribe(
	ctx context.Context,
	audioData []byte,
	filename string,
	model string,
	language string,
) (string, error) {
	if model == "" {
		model = "whisper-1"
	}

	// Собираем multipart/form-data
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	// Поле "file"
	mimeType := audioMIME(filename)
	part, err := createFormFile(mw, "file", filename, mimeType)
	if err != nil {
		return "", fmt.Errorf("создание multipart поля file: %w", err)
	}
	if _, err := part.Write(audioData); err != nil {
		return "", fmt.Errorf("запись аудио в multipart: %w", err)
	}

	// Поле "model"
	if err := mw.WriteField("model", model); err != nil {
		return "", err
	}

	// Поле "language" (опционально)
	if language != "" {
		if err := mw.WriteField("language", language); err != nil {
			return "", err
		}
	}

	// Поле "response_format" — всегда json
	if err := mw.WriteField("response_format", "json"); err != nil {
		return "", err
	}

	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("закрытие multipart writer: %w", err)
	}

	// Отправляем запрос
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/audio/transcriptions", &body)
	if err != nil {
		return "", fmt.Errorf("создание запроса к Whisper: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("Whisper недоступен (%s): %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // макс 1 MB
	if err != nil {
		return "", fmt.Errorf("чтение ответа Whisper: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Whisper вернул статус %d: %s", resp.StatusCode, string(respBody))
	}

	var result whisperResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		// Некоторые реализации возвращают просто текст без JSON
		text := strings.TrimSpace(string(respBody))
		if text != "" {
			return text, nil
		}
		return "", fmt.Errorf("парсинг ответа Whisper: %w", err)
	}

	return strings.TrimSpace(result.Text), nil
}

// createFormFile создаёт part с корректным Content-Type для аудиофайла.
func createFormFile(mw *multipart.Writer, fieldname, filename, contentType string) (io.Writer, error) {
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="%s"; filename="%s"`, fieldname, filename))
	h.Set("Content-Type", contentType)
	return mw.CreatePart(h)
}

// audioMIME определяет MIME-тип аудиофайла по расширению.
func audioMIME(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".webm":
		return "audio/webm"
	case ".ogg", ".oga":
		return "audio/ogg"
	case ".mp3":
		return "audio/mpeg"
	case ".mp4", ".m4a":
		return "audio/mp4"
	case ".wav":
		return "audio/wav"
	case ".flac":
		return "audio/flac"
	default:
		return "application/octet-stream"
	}
}

// ── PiperTTS ─────────────────────────────────────────────────────────────────

// PiperTTS управляет синтезом речи через бинарник piper.
type PiperTTS struct {
	binPath    string // путь к piper (пустой = не найден)
	voicesDir  string // директория с ONNX-моделями голосов
	defaultVoice string
}

// NewPiperTTS создаёт экземпляр PiperTTS.
// binPath — путь к бинарнику piper (или "", тогда ищем в PATH).
// voicesDir — директория с .onnx файлами голосов.
// defaultVoice — имя голоса по умолчанию (без расширения).
func NewPiperTTS(binPath, voicesDir, defaultVoice string) *PiperTTS {
	// Ищем бинарник в PATH если не задан явно
	if binPath == "" {
		if found, err := exec.LookPath("piper"); err == nil {
			binPath = found
		}
	}
	if defaultVoice == "" {
		defaultVoice = "en_US-lessac-medium"
	}
	return &PiperTTS{
		binPath:      binPath,
		voicesDir:    voicesDir,
		defaultVoice: defaultVoice,
	}
}

// IsAvailable возвращает true если piper найден.
func (p *PiperTTS) IsAvailable() bool {
	return p.binPath != ""
}

// VoicePath возвращает полный путь к файлу модели голоса.
// Если voicesDir задан — ищет там, иначе использует имя как есть.
func (p *PiperTTS) VoicePath(voice string) string {
	if voice == "" {
		voice = p.defaultVoice
	}
	// Если задана директория голосов, составляем путь
	if p.voicesDir != "" {
		candidate := filepath.Join(p.voicesDir, voice+".onnx")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	// Пробуем использовать имя как путь напрямую
	if strings.HasSuffix(voice, ".onnx") {
		return voice
	}
	return voice + ".onnx"
}

// Synthesize синтезирует речь из текста.
// Возвращает WAV-байты или ошибку.
// voice — имя голоса (пустое = defaultVoice).
// speed — скорость (1.0 = нормальная).
func (p *PiperTTS) Synthesize(ctx context.Context, text, voice string, speed float64) ([]byte, error) {
	if !p.IsAvailable() {
		return nil, fmt.Errorf("piper не установлен. Установи: https://github.com/rhasspy/piper")
	}

	if speed <= 0 {
		speed = 1.0
	}

	voicePath := p.VoicePath(voice)

	// piper читает текст из stdin, пишет WAV в stdout
	//   piper --model <voice.onnx> --length-scale <speed> --output-raw
	// --output-raw даёт сырой PCM, нам нужен WAV → используем без --output-raw
	// Вместо этого используем временный файл или stdout WAV
	args := []string{
		"--model", voicePath,
		"--output_file", "/dev/stdout",
	}
	if speed != 1.0 {
		args = append(args, "--length-scale", fmt.Sprintf("%.2f", 1.0/speed))
	}

	cmd := exec.CommandContext(ctx, p.binPath, args...)
	cmd.Stdin = strings.NewReader(text)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("piper ошибка: %s", msg)
	}

	if stdout.Len() == 0 {
		return nil, fmt.Errorf("piper не вернул аудио (stderr: %s)", strings.TrimSpace(stderr.String()))
	}

	return stdout.Bytes(), nil
}

// ── HTTP-обработчики ──────────────────────────────────────────────────────────

// registerAudioRoutes регистрирует маршруты STT и TTS.
func registerAudioRoutes(mux *http.ServeMux, whisper *WhisperClient, piper *PiperTTS, jwtSecret []byte) {
	authMw := RequireAuth(jwtSecret)
	maxAudio := int64(25 << 20) // 25 MB

	// ── STT: POST /api/audio/transcriptions ──────────────────────────────
	// Принимает: multipart/form-data с полем "file" (аудиофайл),
	//            опционально "model", "language"
	// Возвращает: {"text": "расшифровка"}
	transcribeHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxAudio)
		if err := r.ParseMultipartForm(maxAudio); err != nil {
			http.Error(w, "файл слишком большой или неверный формат (макс 25 MB)", 413)
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "поле 'file' не найдено", 400)
			return
		}
		defer file.Close()

		audioData, err := io.ReadAll(io.LimitReader(file, maxAudio))
		if err != nil {
			http.Error(w, "ошибка чтения аудио", 500)
			return
		}

		model    := r.FormValue("model")
		language := r.FormValue("language")
		ctx := r.Context()

		text, err := whisper.Transcribe(ctx, audioData, header.Filename, model, language)
		if err != nil {
			http.Error(w, "ошибка транскрибации: "+err.Error(), 502)
			return
		}

		jsonOK(w, map[string]string{"text": text})
	}

	mux.Handle("/api/audio/transcriptions", authMw(http.HandlerFunc(transcribeHandler)))
	mux.Handle("/v1/audio/transcriptions",  authMw(http.HandlerFunc(transcribeHandler)))

	// ── TTS: POST /api/audio/speech ──────────────────────────────────────
	// Принимает: JSON {text, voice, speed}
	// Возвращает: audio/wav (бинарные байты)
	mux.Handle("/api/audio/speech", authMw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		if !piper.IsAvailable() {
			http.Error(w, `{"error":"piper не установлен. Установи piper-tts: https://github.com/rhasspy/piper"}`, 501)
			return
		}

		var body struct {
			Text  string  `json:"text"`
			Voice string  `json:"voice"`
			Speed float64 `json:"speed"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Text) == "" {
			http.Error(w, "нужно поле text", 400)
			return
		}

		ctx := r.Context()
		wav, err := piper.Synthesize(ctx, body.Text, body.Voice, body.Speed)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}

		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(wav)))
		w.Write(wav)
	})))

	// ── Статус аудио-сервисов: GET /api/audio/status ─────────────────────
	mux.Handle("/api/audio/status", authMw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		jsonOK(w, map[string]any{
			"whisper_available": whisper.IsAvailable(),
			"whisper_url":       whisper.baseURL,
			"piper_available":   piper.IsAvailable(),
			"piper_bin":         piper.binPath,
		})
	})))
}
