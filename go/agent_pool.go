// agent_pool.go — пул специализированных агентов с ультракомпактными промптами.
//
// Каждый агент имеет роль и оптимизированный системный промпт (~50-80 токенов
// вместо ~400 у общего агента). Экономия ≈ 5-8x токенов на запрос.
//
// Встроенные роли (12):
//
//	coder      — написание кода
//	debugger   — поиск и исправление ошибок
//	reviewer   — ревью кода и текста
//	planner    — декомпозиция задач
//	researcher — анализ и поиск информации
//	writer     — написание документации и текстов
//	summarizer — краткое изложение
//	critic     — критический анализ
//	translator — перевод
//	analyst    — анализ данных
//	math       — математика и вычисления
//	security   — аудит безопасности
//
// Расширение: положи JSON-файл в data/agents/<name>.json.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// AgentRole описывает специализацию агента.
type AgentRole struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Prompt      string  `json:"prompt"`        // системный промпт (~50-80 токенов)
	Temperature float64 `json:"temperature"`   // оптимальная для роли температура
	MaxSteps    int     `json:"max_steps"`     // максимум шагов ReAct-цикла
	UseTools    bool    `json:"use_tools"`     // подключать ли инструменты
	Tags        []string `json:"tags,omitempty"` // теги для поиска/фильтрации
}

// BuiltinRoles — встроенные специализации агентов.
// Промпты намеренно лаконичны: только роль + формат вывода.
var BuiltinRoles = map[string]*AgentRole{
	"coder": {
		Name:        "coder",
		Description: "Пишет чистый, рабочий код с комментариями",
		Prompt:      "Ты эксперт-программист. Пиши чистый код с комментариями. Только рабочий код — никаких заглушек. Формат: ```язык ... ```",
		Temperature: 0.2,
		MaxSteps:    5,
		UseTools:    true,
		Tags:        []string{"code", "dev"},
	},
	"debugger": {
		Name:        "debugger",
		Description: "Находит и исправляет ошибки в коде",
		Prompt:      "Ты отладчик. Анализируй код, находи баги, объясняй причину кратко, давай исправленный код. Формат: ПРОБЛЕМА / ПРИЧИНА / ИСПРАВЛЕНИЕ.",
		Temperature: 0.1,
		MaxSteps:    4,
		UseTools:    false,
		Tags:        []string{"code", "debug"},
	},
	"reviewer": {
		Name:        "reviewer",
		Description: "Ревьюит код или текст: находит проблемы и предлагает улучшения",
		Prompt:      "Ты code reviewer. Оценивай: корректность, читаемость, безопасность, производительность. Список: [КРИТИЧНО] / [ВАЖНО] / [СТИЛЬ]. Краткие комментарии.",
		Temperature: 0.3,
		MaxSteps:    3,
		UseTools:    false,
		Tags:        []string{"code", "review"},
	},
	"planner": {
		Name:        "planner",
		Description: "Разбивает задачу на конкретные шаги",
		Prompt:      "Ты планировщик. Декомпозируй задачу на атомарные шаги. Формат: 1. Шаг (цель, инструмент, критерий готовности). Не более 7 шагов.",
		Temperature: 0.4,
		MaxSteps:    2,
		UseTools:    false,
		Tags:        []string{"planning", "task"},
	},
	"researcher": {
		Name:        "researcher",
		Description: "Исследует тему, собирает и систематизирует информацию",
		Prompt:      "Ты исследователь. Собирай факты, систематизируй. Используй web_search для свежих данных. Структура: ФАКТЫ / ИСТОЧНИКИ / ВЫВОД.",
		Temperature: 0.3,
		MaxSteps:    6,
		UseTools:    true,
		Tags:        []string{"research", "analysis"},
	},
	"writer": {
		Name:        "writer",
		Description: "Пишет документацию, README, статьи и тексты",
		Prompt:      "Ты технический писатель. Пиши ясно, структурированно, без воды. Markdown для документации. Целевая аудитория — разработчики.",
		Temperature: 0.6,
		MaxSteps:    3,
		UseTools:    false,
		Tags:        []string{"writing", "docs"},
	},
	"summarizer": {
		Name:        "summarizer",
		Description: "Сжимает длинный текст до ключевых тезисов",
		Prompt:      "Ты суммаризатор. Извлекай только главное. Формат: 3-7 тезисов маркированным списком. Без лишних слов.",
		Temperature: 0.2,
		MaxSteps:    1,
		UseTools:    false,
		Tags:        []string{"compression", "summary"},
	},
	"critic": {
		Name:        "critic",
		Description: "Критически анализирует идеи, решения, архитектуру",
		Prompt:      "Ты критик. Находи слабые места, риски, альтернативы. Будь конкретен. Формат: СЛАБОЕ МЕСТО / РИСК / АЛЬТЕРНАТИВА.",
		Temperature: 0.5,
		MaxSteps:    2,
		UseTools:    false,
		Tags:        []string{"analysis", "review"},
	},
	"translator": {
		Name:        "translator",
		Description: "Переводит тексты, сохраняя смысл и стиль",
		Prompt:      "Ты переводчик. Переводи точно, сохраняй стиль оригинала. Технические термины переводи с пояснением при первом использовании.",
		Temperature: 0.2,
		MaxSteps:    1,
		UseTools:    false,
		Tags:        []string{"translation"},
	},
	"analyst": {
		Name:        "analyst",
		Description: "Анализирует данные, находит паттерны и тренды",
		Prompt:      "Ты аналитик данных. Ищи паттерны, аномалии, тренды. Формат: НАБЛЮДЕНИЕ / ИНТЕРПРЕТАЦИЯ / РЕКОМЕНДАЦИЯ. Используй calculator для вычислений.",
		Temperature: 0.3,
		MaxSteps:    4,
		UseTools:    true,
		Tags:        []string{"analysis", "data"},
	},
	"math": {
		Name:        "math",
		Description: "Решает математические задачи пошагово",
		Prompt:      "Ты математик. Решай задачи пошагово. Проверяй результат. Используй calculator. Формат: ДАНО / РЕШЕНИЕ / ОТВЕТ.",
		Temperature: 0.05,
		MaxSteps:    5,
		UseTools:    true,
		Tags:        []string{"math", "calculation"},
	},
	"security": {
		Name:        "security",
		Description: "Аудит безопасности: ищет уязвимости в коде и конфигах",
		Prompt:      "Ты security эксперт. Ищи: SQL injection, XSS, path traversal, secrets в коде, уязвимые зависимости, misconfiguration. Формат: УЯЗВИМОСТЬ / СЕРЬЁЗНОСТЬ(критично|высокая|средняя) / ИСПРАВЛЕНИЕ.",
		Temperature: 0.1,
		MaxSteps:    4,
		UseTools:    false,
		Tags:        []string{"security", "audit"},
	},
}

// agentPoolMu защищает customRoles при параллельном доступе.
var agentPoolMu sync.RWMutex

// customRoles — пользовательские роли, загруженные из data/agents/*.json.
var customRoles = map[string]*AgentRole{}

// agentsDataDir — директория с пользовательскими ролями.
var agentsDataDir string

// InitAgentPool загружает пользовательские роли из dataDir/agents/.
// Вызывается из runServer.
func InitAgentPool(dataDir string) {
	agentsDataDir = filepath.Join(dataDir, "agents")
	_ = os.MkdirAll(agentsDataDir, 0o755)

	// Загружаем пользовательские роли из JSON-файлов
	entries, err := os.ReadDir(agentsDataDir)
	if err != nil {
		return
	}

	loaded := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(agentsDataDir, e.Name()))
		if err != nil {
			continue
		}
		var role AgentRole
		if err := json.Unmarshal(data, &role); err != nil || role.Name == "" {
			continue
		}
		// Нормализуем дефолты
		if role.Temperature == 0 {
			role.Temperature = 0.5
		}
		if role.MaxSteps == 0 {
			role.MaxSteps = 4
		}

		agentPoolMu.Lock()
		customRoles[role.Name] = &role
		agentPoolMu.Unlock()
		loaded++
	}

	if loaded > 0 {
		fmt.Printf("[agent_pool] загружено пользовательских ролей: %d\n", loaded)
	}
}

// GetRole возвращает роль по имени (сначала встроенные, потом пользовательские).
func GetRole(name string) (*AgentRole, bool) {
	// Встроенные роли
	if r, ok := BuiltinRoles[name]; ok {
		return r, true
	}
	// Пользовательские роли
	agentPoolMu.RLock()
	defer agentPoolMu.RUnlock()
	r, ok := customRoles[name]
	return r, ok
}

// AllRoles возвращает все доступные роли (встроенные + пользовательские).
func AllRoles() []*AgentRole {
	agentPoolMu.RLock()
	defer agentPoolMu.RUnlock()

	roles := make([]*AgentRole, 0, len(BuiltinRoles)+len(customRoles))
	for _, r := range BuiltinRoles {
		roles = append(roles, r)
	}
	for _, r := range customRoles {
		roles = append(roles, r)
	}
	return roles
}

// SaveCustomRole сохраняет пользовательскую роль на диск и в память.
func SaveCustomRole(role *AgentRole) error {
	if role.Name == "" {
		return fmt.Errorf("поле name обязательно")
	}
	if role.Prompt == "" {
		return fmt.Errorf("поле prompt обязательно")
	}
	// Нельзя перезаписывать встроенные
	if _, ok := BuiltinRoles[role.Name]; ok {
		return fmt.Errorf("роль %q встроенная, нельзя перезаписать", role.Name)
	}

	// Нормализуем дефолты
	if role.Temperature == 0 {
		role.Temperature = 0.5
	}
	if role.MaxSteps == 0 {
		role.MaxSteps = 4
	}

	data, err := json.MarshalIndent(role, "", "  ")
	if err != nil {
		return err
	}

	path := filepath.Join(agentsDataDir, role.Name+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("сохранение роли: %w", err)
	}

	agentPoolMu.Lock()
	customRoles[role.Name] = role
	agentPoolMu.Unlock()
	return nil
}

// DeleteCustomRole удаляет пользовательскую роль.
func DeleteCustomRole(name string) error {
	if _, ok := BuiltinRoles[name]; ok {
		return fmt.Errorf("встроенную роль %q нельзя удалить", name)
	}

	agentPoolMu.Lock()
	delete(customRoles, name)
	agentPoolMu.Unlock()

	path := filepath.Join(agentsDataDir, name+".json")
	if err := os.Remove(path); os.IsNotExist(err) {
		return nil // уже не существует — ок
	} else {
		return err
	}
}

// RunRoleAgent запускает специализированного агента для выполнения задачи.
// Возвращает финальный ответ. stepCh опционально — для SSE-стриминга (nil = не нужен).
func RunRoleAgent(
	ctx context.Context,
	client *OllamaClient,
	roleName string,
	task string,
	model string,
	stepCh chan<- AgentStep,
) (string, error) {
	role, ok := GetRole(roleName)
	if !ok {
		return "", fmt.Errorf("роль %q не найдена", roleName)
	}

	// Строим минимальный контекст для специалиста
	messages := []Message{
		{Role: "system", Content: role.Prompt},
		{Role: "user", Content: task},
	}

	// Выбираем температуру роли
	temp := role.Temperature

	// Специалисты с инструментами идут через полный ReAct-цикл
	if role.UseTools && stepCh != nil {
		// Добавляем список инструментов к промпту
		messages[0].Content = role.Prompt + "\n\n" + ToolsPrompt()
		return RunAgent(ctx, client, messages, model, temp, stepCh)
	}

	// Специалисты без инструментов — один вызов LLM (минимум токенов)
	resp, fromCache, err := CachedChat(ctx, client, messages, model, temp)
	if err != nil {
		return "", err
	}

	// Отправляем в канал если он есть
	if stepCh != nil {
		if fromCache {
			stepCh <- AgentStep{Kind: StepThought, Content: "[cache hit]"}
		}
		stepCh <- AgentStep{Kind: StepFinalAnswer, Content: resp}
		close(stepCh)
	}

	return resp, nil
}

// RolesSummary возвращает компактный список ролей для промпта агента.
// Используется в ToolsPrompt чтобы агент знал какие специалисты доступны.
func RolesSummary() string {
	var sb strings.Builder
	sb.WriteString("Доступные роли агентов:\n")
	for name, r := range BuiltinRoles {
		sb.WriteString(fmt.Sprintf("• %s: %s\n", name, r.Description))
	}
	agentPoolMu.RLock()
	for name, r := range customRoles {
		sb.WriteString(fmt.Sprintf("• %s: %s [custom]\n", name, r.Description))
	}
	agentPoolMu.RUnlock()
	return sb.String()
}
