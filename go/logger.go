// logger.go — логирование в файл с ротацией по размеру (stdlib only).
//
// Использование:
//
//	InitLogger("/var/log/localai.log")  // инициализация
//	log.Println("...")                  // стандартный пакет log → в файл
//
// Ротация: при превышении rotateSize (10 MB) текущий файл сжимается в .gz
// (например, localai.log.1.gz), затем открывается новый localai.log.
// Сохраняется не более maxBackups (5) архивов.
package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	rotateSize = 10 << 20 // 10 MB — порог ротации
	maxBackups = 5        // максимум .gz-архивов рядом с основным файлом
)

// fileLogger реализует io.Writer с ротацией.
type fileLogger struct {
	mu      sync.Mutex
	path    string
	file    *os.File
	written int64 // байт записано в текущий файл
}

// Write пишет данные в лог-файл, инициирует ротацию если нужно.
func (l *fileLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Ротация если файл вырос
	if l.written+int64(len(p)) > rotateSize {
		if err := l.rotate(); err != nil {
			// При ошибке ротации — пишем в stderr и продолжаем в текущий файл
			fmt.Fprintf(os.Stderr, "[logger] ошибка ротации: %v\n", err)
		}
	}

	n, err := l.file.Write(p)
	l.written += int64(n)
	return n, err
}

// rotate сжимает текущий файл в .gz и открывает новый.
func (l *fileLogger) rotate() error {
	// Закрываем текущий файл
	if err := l.file.Close(); err != nil {
		return fmt.Errorf("закрытие лог-файла: %w", err)
	}

	// Сжимаем в архив
	if err := gzipFile(l.path); err != nil {
		return fmt.Errorf("gzip лога: %w", err)
	}

	// Удаляем старые архивы если их слишком много
	pruneBackups(l.path, maxBackups)

	// Открываем новый файл
	f, err := openLogFile(l.path)
	if err != nil {
		return fmt.Errorf("открытие нового лог-файла: %w", err)
	}
	l.file = f
	l.written = 0
	return nil
}

// InitLogger настраивает стандартный пакет log писать в файл path.
// Дополнительно: запись дублируется в stderr (удобно для отладки в Docker).
// Если path пустой — ничего не делает (лог остаётся в stderr).
func InitLogger(path string) error {
	if path == "" {
		return nil
	}

	// Создаём директорию если нет
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("создание директории логов %s: %w", dir, err)
		}
	}

	f, err := openLogFile(path)
	if err != nil {
		return fmt.Errorf("открытие лог-файла %s: %w", path, err)
	}

	// Проверяем размер существующего файла
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}

	fl := &fileLogger{
		path:    path,
		file:    f,
		written: info.Size(),
	}

	// Пишем и в файл, и в stderr (полезно при запуске в контейнере)
	multi := io.MultiWriter(fl, os.Stderr)
	log.SetOutput(multi)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmsgprefix)

	log.Printf("[logger] логирование в файл: %s (макс %d MB)", path, rotateSize>>20)
	return nil
}

// ── Вспомогательные ──────────────────────────────────────────────────────────

// openLogFile открывает файл для дозаписи (создаёт если нет).
func openLogFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// gzipFile сжимает srcPath в srcPath.<n>.gz и удаляет оригинал.
// Номер n выбирается как max существующего +1.
func gzipFile(srcPath string) error {
	// Вычисляем следующий номер архива
	n := nextBackupNum(srcPath)
	dstPath := fmt.Sprintf("%s.%d.gz", srcPath, n)

	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dst.Close()

	gz := gzip.NewWriter(dst)
	gz.Name = filepath.Base(srcPath)

	if _, err := io.Copy(gz, src); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}

	// Удаляем исходный файл после успешного сжатия
	return os.Remove(srcPath)
}

// nextBackupNum возвращает следующий номер для архива.
func nextBackupNum(basePath string) int {
	dir := filepath.Dir(basePath)
	base := filepath.Base(basePath)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return 1
	}

	max := 0
	prefix := base + "."
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".gz") {
			continue
		}
		// Формат: basename.<n>.gz
		inner := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".gz")
		var n int
		if _, err := fmt.Sscanf(inner, "%d", &n); err == nil && n > max {
			max = n
		}
	}
	return max + 1
}

// pruneBackups удаляет старые .gz-архивы, оставляя только keep штук.
func pruneBackups(basePath string, keep int) {
	dir := filepath.Dir(basePath)
	base := filepath.Base(basePath)
	prefix := base + "."

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	var archives []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".gz") {
			archives = append(archives, filepath.Join(dir, name))
		}
	}

	if len(archives) <= keep {
		return
	}

	// Сортируем по имени (содержит номер) — удаляем самые старые
	sort.Strings(archives)
	for _, old := range archives[:len(archives)-keep] {
		_ = os.Remove(old)
	}
}
