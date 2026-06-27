// storage.go — персистентное хранилище сессий на основе JSON-файлов.
// Не требует внешних зависимостей (только stdlib).
//
// Структура каталога данных:
//
//	/app/data/
//	  sessions/
//	    <id>.json   — один файл на сессию
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// SessionSettings — настройки конкретной сессии.
type SessionSettings struct {
	SystemPrompt string  `json:"system_prompt"`
	Temperature  float64 `json:"temperature"`
	Model        string  `json:"model"`
}

// SessionMeta — краткая информация о сессии (без сообщений).
type SessionMeta struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Settings  SessionSettings `json:"settings"`
	MsgCount  int             `json:"msg_count"`
}

// Session — полная сессия включая историю.
type Session struct {
	SessionMeta
	Messages []Message `json:"messages"`
}

// Storage управляет сессиями на диске.
type Storage struct {
	dir string
	mu  sync.RWMutex
}

// NewStorage создаёт или открывает хранилище по указанному пути.
func NewStorage(dataDir string) (*Storage, error) {
	sessDir := filepath.Join(dataDir, "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		return nil, fmt.Errorf("не удалось создать директорию данных %s: %w", sessDir, err)
	}
	return &Storage{dir: sessDir}, nil
}

// sessPath возвращает путь к файлу сессии.
func (s *Storage) sessPath(id string) string {
	// Очищаем id от опасных символов
	safe := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == '.' {
			return '_'
		}
		return r
	}, id)
	return filepath.Join(s.dir, safe+".json")
}

// Load загружает сессию из файла. Если файл не найден — возвращает nil, nil.
func (s *Storage) Load(id string) (*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(s.sessPath(id))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("чтение сессии %s: %w", id, err)
	}

	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, fmt.Errorf("парсинг сессии %s: %w", id, err)
	}
	return &sess, nil
}

// Save сохраняет сессию на диск (атомарная запись через temp-файл).
func (s *Storage) Save(sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess.UpdatedAt = time.Now()
	sess.MsgCount = len(sess.Messages)

	// Автозаголовок по первому сообщению пользователя
	if sess.Title == "" || sess.Title == "Новый чат" {
		for _, m := range sess.Messages {
			if m.Role == "user" {
				sess.Title = truncate(m.Content, 50)
				break
			}
		}
	}

	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}

	// Пишем во временный файл, затем атомарно переименовываем
	tmp := s.sessPath(sess.ID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("запись сессии %s: %w", sess.ID, err)
	}
	return os.Rename(tmp, s.sessPath(sess.ID))
}

// Delete удаляет сессию с диска.
func (s *Storage) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.sessPath(id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// List возвращает метаданные всех сессий, отсортированные по дате изменения (новые первые).
func (s *Storage) List() ([]SessionMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}

	var metas []SessionMeta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue // Пропускаем повреждённые файлы
		}

		var sess Session
		if err := json.Unmarshal(data, &sess); err != nil {
			continue
		}
		metas = append(metas, sess.SessionMeta)
	}

	// Сортировка: новые сверху
	sort.Slice(metas, func(i, j int) bool {
		return metas[i].UpdatedAt.After(metas[j].UpdatedAt)
	})

	return metas, nil
}

// GetOrCreate загружает сессию, или создаёт новую с дефолтными настройками.
func (s *Storage) GetOrCreate(id, defaultModel, customPrompt string) (*Session, error) {
	sess, err := s.Load(id)
	if err != nil {
		return nil, err
	}
	if sess != nil {
		return sess, nil
	}

	// Новая сессия
	prompt := customPrompt
	if prompt == "" {
		prompt = systemPrompt
	}

	now := time.Now()
	sess = &Session{
		SessionMeta: SessionMeta{
			ID:        id,
			Title:     "Новый чат",
			CreatedAt: now,
			UpdatedAt: now,
			Settings: SessionSettings{
				SystemPrompt: prompt,
				Temperature:  0.7,
				Model:        defaultModel,
			},
		},
		Messages: []Message{
			{Role: "system", Content: prompt},
		},
	}
	return sess, nil
}

// AppendAndSave добавляет сообщения в сессию и сохраняет на диск.
func (s *Storage) AppendAndSave(sess *Session, msgs ...Message) error {
	sess.Messages = append(sess.Messages, msgs...)
	return s.Save(sess)
}

// CleanupOldSessions удаляет сессии, которые не обновлялись дольше maxAge.
// Возвращает количество удалённых сессий.
// Защищённые сессии (id == "default") не удаляются.
func (s *Storage) CleanupOldSessions(maxAge time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, fmt.Errorf("CleanupOldSessions: чтение директории: %w", err)
	}

	cutoff := time.Now().Add(-maxAge)
	deleted := 0

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}

		p := filepath.Join(s.dir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}

		var sess Session
		if err := json.Unmarshal(data, &sess); err != nil {
			continue
		}

		// Не трогаем дефолтную сессию и совсем свежие
		if sess.ID == "default" {
			continue
		}

		if sess.UpdatedAt.Before(cutoff) {
			if err := os.Remove(p); err == nil {
				deleted++
			}
		}
	}
	return deleted, nil
}

// DiskUsage возвращает суммарный размер всех файлов сессий в байтах и их количество.
func (s *Storage) DiskUsage() (totalBytes int64, count int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if info, err := e.Info(); err == nil {
			totalBytes += info.Size()
			count++
		}
	}
	return totalBytes, count
}

// truncate обрезает строку до maxLen рун, добавляя "…" если обрезана.
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen-1]) + "…"
}
