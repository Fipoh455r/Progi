// auth.go — авторизация v2.2: пользователи, JWT, rate limiting.
//
// Реализовано только на stdlib:
//   crypto/hmac + crypto/sha256 — хэширование паролей (100k итераций) и подпись JWT
//   crypto/rand                 — генерация соли и JWT-секрета
//   encoding/base64             — кодирование JWT и хэшей
//   encoding/json               — хранение пользователей в JSON-файле
//
// Схема пароля: "sha256i$100000$<base64url(salt)>$<base64url(hash)>"
// Схема JWT:    HS256, claims {sub, role, exp, iat}
package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ── Константы ────────────────────────────────────────────────────────────────

const (
	RoleAdmin = "admin"
	RoleUser  = "user"

	// Количество итераций хэширования пароля.
	hashIterations = 100_000

	// Размер соли в байтах.
	saltSize = 32

	// Время жизни JWT-токена.
	tokenTTL = 24 * time.Hour

	// Rate limiting: не более rateLimitMax запросов за rateLimitWindow.
	rateLimitMax    = 60
	rateLimitWindow = time.Minute
)

// ── Пользователь ─────────────────────────────────────────────────────────────

// User — один пользователь системы.
type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"password_hash"` // sha256i$iter$salt$hash
	Role         string    `json:"role"`           // "admin" | "user"
	CreatedAt    time.Time `json:"created_at"`
}

// ── UserStore ─────────────────────────────────────────────────────────────────

// UserStore управляет списком пользователей (хранит в JSON-файле).
type UserStore struct {
	path string
	mu   sync.RWMutex
}

// NewUserStore открывает или создаёт хранилище пользователей.
func NewUserStore(dataDir string) (*UserStore, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("не удалось создать директорию %s: %w", dataDir, err)
	}
	return &UserStore{path: filepath.Join(dataDir, "users.json")}, nil
}

// loadAll читает всех пользователей из файла (без блокировки).
func (s *UserStore) loadAll() ([]User, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("чтение users.json: %w", err)
	}
	var users []User
	if err := json.Unmarshal(data, &users); err != nil {
		return nil, fmt.Errorf("парсинг users.json: %w", err)
	}
	return users, nil
}

// saveAll записывает всех пользователей в файл (без блокировки).
func (s *UserStore) saveAll(users []User) error {
	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("запись users.json: %w", err)
	}
	return os.Rename(tmp, s.path)
}

// Count возвращает количество пользователей.
func (s *UserStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	users, _ := s.loadAll()
	return len(users)
}

// GetByUsername ищет пользователя по имени (регистронезависимо).
func (s *UserStore) GetByUsername(username string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	users, err := s.loadAll()
	if err != nil {
		return nil, err
	}
	lower := strings.ToLower(username)
	for i := range users {
		if strings.ToLower(users[i].Username) == lower {
			return &users[i], nil
		}
	}
	return nil, nil
}

// GetByID ищет пользователя по ID.
func (s *UserStore) GetByID(id string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	users, err := s.loadAll()
	if err != nil {
		return nil, err
	}
	for i := range users {
		if users[i].ID == id {
			return &users[i], nil
		}
	}
	return nil, nil
}

// Create добавляет нового пользователя.
func (s *UserStore) Create(username, password, role string) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	users, err := s.loadAll()
	if err != nil {
		return nil, err
	}

	// Проверяем уникальность имени
	lower := strings.ToLower(username)
	for _, u := range users {
		if strings.ToLower(u.Username) == lower {
			return nil, fmt.Errorf("пользователь %q уже существует", username)
		}
	}

	hash, err := HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("хэширование пароля: %w", err)
	}

	id, err := randomHex(16)
	if err != nil {
		return nil, err
	}

	u := User{
		ID:           id,
		Username:     username,
		PasswordHash: hash,
		Role:         role,
		CreatedAt:    time.Now(),
	}
	users = append(users, u)
	if err := s.saveAll(users); err != nil {
		return nil, err
	}
	return &u, nil
}

// Delete удаляет пользователя по ID.
func (s *UserStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	users, err := s.loadAll()
	if err != nil {
		return err
	}
	filtered := users[:0]
	for _, u := range users {
		if u.ID != id {
			filtered = append(filtered, u)
		}
	}
	return s.saveAll(filtered)
}

// List возвращает всех пользователей (без хэшей паролей).
func (s *UserStore) List() ([]User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	users, err := s.loadAll()
	if err != nil {
		return nil, err
	}
	// Не отдаём хэши паролей наружу
	clean := make([]User, len(users))
	for i, u := range users {
		clean[i] = u
		clean[i].PasswordHash = ""
	}
	return clean, nil
}

// ── Пароли ───────────────────────────────────────────────────────────────────

// HashPassword хэширует пароль с использованием HMAC-SHA256 (100k итераций + соль).
// Формат результата: "sha256i$100000$<base64url(salt)>$<base64url(hash)>"
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("генерация соли: %w", err)
	}
	h := iterHash([]byte(password), salt, hashIterations)
	return fmt.Sprintf("sha256i$%d$%s$%s",
		hashIterations,
		base64.RawURLEncoding.EncodeToString(salt),
		base64.RawURLEncoding.EncodeToString(h),
	), nil
}

// VerifyPassword проверяет пароль против сохранённого хэша.
func VerifyPassword(stored, password string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "sha256i" {
		return false
	}
	var iterations int
	if _, err := fmt.Sscanf(parts[1], "%d", &iterations); err != nil || iterations <= 0 {
		return false
	}
	salt, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	expected, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	actual := iterHash([]byte(password), salt, iterations)
	return hmac.Equal(actual, expected)
}

// iterHash выполняет N итераций HMAC-SHA256(password, salt).
// Каждая итерация: result = HMAC-SHA256(result_prev, salt).
func iterHash(password, salt []byte, iterations int) []byte {
	mac := hmac.New(sha256.New, salt)
	mac.Write(password)
	result := mac.Sum(nil)
	for i := 1; i < iterations; i++ {
		mac.Reset()
		mac.Write(result)
		result = mac.Sum(nil)
	}
	return result
}

// ── JWT ──────────────────────────────────────────────────────────────────────

// jwtClaims — полезная нагрузка JWT-токена.
type jwtClaims struct {
	Sub  string `json:"sub"`  // username
	Role string `json:"role"` // "admin" | "user"
	Exp  int64  `json:"exp"`  // unix timestamp истечения
	Iat  int64  `json:"iat"`  // unix timestamp создания
}

var jwtHeader = base64url([]byte(`{"alg":"HS256","typ":"JWT"}`))

// GenerateJWT создаёт подписанный HS256 JWT-токен.
func GenerateJWT(username, role string, secret []byte) (string, error) {
	now := time.Now()
	claims := jwtClaims{
		Sub:  username,
		Role: role,
		Exp:  now.Add(tokenTTL).Unix(),
		Iat:  now.Unix(),
	}
	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	payload := base64url(payloadJSON)
	unsigned := jwtHeader + "." + payload
	sig := jwtSign(unsigned, secret)
	return unsigned + "." + sig, nil
}

// ValidateJWT проверяет подпись и срок действия токена.
// Возвращает claims при успехе или ошибку.
func ValidateJWT(token string, secret []byte) (*jwtClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("неверный формат токена")
	}
	unsigned := parts[0] + "." + parts[1]
	expected := jwtSign(unsigned, secret)
	if !hmac.Equal([]byte(parts[2]), []byte(expected)) {
		return nil, errors.New("неверная подпись")
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("декодирование payload: %w", err)
	}
	var claims jwtClaims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return nil, fmt.Errorf("парсинг claims: %w", err)
	}
	if time.Now().Unix() > claims.Exp {
		return nil, errors.New("токен истёк")
	}
	return &claims, nil
}

// jwtSign возвращает base64url(HMAC-SHA256(data, secret)).
func jwtSign(data string, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(data))
	return base64url(mac.Sum(nil))
}

// base64url кодирует байты в base64url без паддинга.
func base64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// ── Контекст запроса ─────────────────────────────────────────────────────────

type contextKey string

const ctxUserKey contextKey = "auth_user"

// contextUser извлекает пользователя из контекста запроса.
// Возвращает nil если пользователь не авторизован.
func contextUser(ctx context.Context) *jwtClaims {
	v, _ := ctx.Value(ctxUserKey).(*jwtClaims)
	return v
}

// ── Middleware ────────────────────────────────────────────────────────────────

// RequireAuth возвращает middleware, которое проверяет JWT Bearer-токен.
// При успехе инжектирует claims в контекст.
func RequireAuth(secret []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, err := extractBearerClaims(r, secret)
			if err != nil {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), ctxUserKey, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireAdmin возвращает middleware, которое требует роль admin.
func RequireAdmin(secret []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, err := extractBearerClaims(r, secret)
			if err != nil {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			if claims.Role != RoleAdmin {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			ctx := context.WithValue(r.Context(), ctxUserKey, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// extractBearerClaims извлекает и валидирует Bearer-токен из заголовка Authorization.
func extractBearerClaims(r *http.Request, secret []byte) (*jwtClaims, error) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return nil, errors.New("нет Bearer-токена")
	}
	return ValidateJWT(strings.TrimPrefix(auth, "Bearer "), secret)
}

// ── Rate Limiter ──────────────────────────────────────────────────────────────

// RateLimiter — ограничитель запросов по IP (скользящее окно в памяти).
type RateLimiter struct {
	mu      sync.Mutex
	windows map[string][]time.Time // IP → список меток запросов
	max     int
	window  time.Duration
}

// NewRateLimiter создаёт rate limiter с заданными параметрами.
// Периодически чистит устаревшие записи.
func NewRateLimiter(max int, window time.Duration) *RateLimiter {
	rl := &RateLimiter{
		windows: make(map[string][]time.Time),
		max:     max,
		window:  window,
	}
	go rl.cleanupLoop()
	return rl
}

// Allow возвращает true если IP не превысил лимит запросов.
func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

	// Оставляем только метки в рамках окна
	existing := rl.windows[ip]
	valid := existing[:0]
	for _, t := range existing {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	if len(valid) >= rl.max {
		rl.windows[ip] = valid
		return false
	}
	rl.windows[ip] = append(valid, now)
	return true
}

// Middleware возвращает HTTP middleware для rate limiting.
// IP извлекается из X-Forwarded-For или RemoteAddr.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !rl.Allow(ip) {
			http.Error(w, `{"error":"too many requests"}`, http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// cleanupLoop периодически удаляет устаревшие IP-записи.
func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		cutoff := time.Now().Add(-rl.window)
		for ip, times := range rl.windows {
			valid := times[:0]
			for _, t := range times {
				if t.After(cutoff) {
					valid = append(valid, t)
				}
			}
			if len(valid) == 0 {
				delete(rl.windows, ip)
			} else {
				rl.windows[ip] = valid
			}
		}
		rl.mu.Unlock()
	}
}

// clientIP извлекает IP клиента из запроса.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if idx := strings.Index(xff, ","); idx >= 0 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}
	// RemoteAddr формат "IP:port"
	addr := r.RemoteAddr
	if idx := strings.LastIndex(addr, ":"); idx >= 0 {
		return addr[:idx]
	}
	return addr
}

// ── Вспомогательные ──────────────────────────────────────────────────────────

// randomHex генерирует случайную hex-строку длиной n байт.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("генерация ID: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}

// GenerateSecret генерирует случайный 32-байтный секрет.
// Используется для автоматической генерации JWT-секрета при первом запуске.
func GenerateSecret() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}
