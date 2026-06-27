// LocalAI — локальный AI-ассистент на Go.
// Работает без интернета: подключается к Ollama для запуска LLM-моделей.
//
// Использование:
//
//	localai              — чат в терминале
//	localai serve        — веб-сервер на http://localhost:8080
//	localai models       — список загруженных моделей
//	localai pull <model> — скачать модель
//	localai version      — версия
//	localai config init  — создать localai.yaml с настройками
package main

import (
	"flag"
	"fmt"
	"os"
)

const appVersion = "3.1.0"

func main() {
	// Флаги — переопределяют и config-файл, и переменные окружения
	configFile := flag.String("config", "", "Путь к YAML-файлу конфигурации (напр. localai.yaml)")
	ollamaURL := flag.String("ollama", "", "URL Ollama API (переопределяет config и OLLAMA_URL)")
	model := flag.String("model", "", "Языковая модель (переопределяет config и LOCALAI_MODEL)")
	port := flag.String("port", "", "Порт веб-сервера (переопределяет config и LOCALAI_PORT)")
	dataDir := flag.String("data", "", "Каталог для данных (переопределяет config и LOCALAI_DATA)")
	flag.Usage = printHelp
	flag.Parse()

	args := flag.Args()
	cmd := "chat"
	if len(args) > 0 {
		cmd = args[0]
	}

	// ── Специальная команда: config init ────────────────────────────────
	if cmd == "config" {
		sub := ""
		if len(args) > 1 {
			sub = args[1]
		}
		switch sub {
		case "init":
			dest := "localai.yaml"
			if len(args) > 2 {
				dest = args[2]
			}
			if err := WriteExample(dest); err != nil {
				fmt.Fprintf(os.Stderr, "Ошибка создания файла: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Файл конфигурации создан: %s\n", dest)
			fmt.Println("Отредактируй его и запусти: localai serve -config", dest)
		default:
			fmt.Fprintf(os.Stderr, "Неизвестная подкоманда: config %s\n", sub)
			fmt.Fprintln(os.Stderr, "Доступно: localai config init [путь.yaml]")
			os.Exit(1)
		}
		return
	}

	// ── Загрузка конфигурации ────────────────────────────────────────────
	// Порядок приоритетов: CLI-флаг > env > config-файл > умолчания
	cfg := DefaultConfig()

	// 1. config-файл (если указан или существует localai.yaml рядом)
	cfgPath := *configFile
	if cfgPath == "" {
		if _, err := os.Stat("localai.yaml"); err == nil {
			cfgPath = "localai.yaml"
		}
	}
	if cfgPath != "" {
		loaded, err := LoadConfig(cfgPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[!] Ошибка чтения конфига %s: %v\n", cfgPath, err)
			os.Exit(1)
		}
		cfg = loaded
	}

	// 2. Переменные окружения поверх config-файла
	cfg.MergeEnv()

	// 3. CLI-флаги поверх всего (только если явно заданы)
	if *ollamaURL != "" {
		cfg.OllamaURL = *ollamaURL
	}
	if *model != "" {
		cfg.Model = *model
	}
	if *port != "" {
		cfg.Port = *port
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}

	// ── Диспетчер команд ─────────────────────────────────────────────────
	switch cmd {
	case "chat", "":
		runChat(cfg.OllamaURL, cfg.Model)

	case "serve", "server", "web":
		runServer(cfg.OllamaURL, cfg.Model, cfg.Port, cfg.DataDir)

	case "models":
		runListModels(cfg.OllamaURL)

	case "pull":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Использование: localai pull <имя-модели>")
			fmt.Fprintln(os.Stderr, "Пример:        localai pull qwen2.5:1.5b")
			os.Exit(1)
		}
		runPullModel(cfg.OllamaURL, args[1])

	case "version":
		fmt.Printf("LocalAI v%s\n", appVersion)

	default:
		fmt.Fprintf(os.Stderr, "Неизвестная команда: %q\n\n", cmd)
		printHelp()
		os.Exit(1)
	}
}

// envOr возвращает значение переменной окружения key, или def если она не задана.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func printHelp() {
	fmt.Printf(`LocalAI v%s — локальный AI-ассистент (без облака)

Использование:
  localai [флаги] [команда] [аргументы]

Команды:
  chat               чат в терминале (по умолчанию)
  serve              веб-интерфейс в браузере
  models             список загруженных моделей
  pull <модель>      скачать модель из Ollama
  config init        создать localai.yaml с умолчаниями
  version            версия программы

Флаги:
`, appVersion)
	flag.PrintDefaults()
	fmt.Print(`
Конфиг-файл (localai.yaml или -config путь):
  Автоматически загружается если localai.yaml есть в текущей директории.
  Создать:  localai config init

Переменные окружения:
  OLLAMA_URL      URL Ollama API    (по умолч.: http://localhost:11434)
  LOCALAI_MODEL   Языковая модель   (по умолч.: qwen2.5:0.5b)
  LOCALAI_PORT    Порт веб-сервера  (по умолч.: 8080)
  LOCALAI_DATA    Каталог данных    (по умолч.: ./data)

Примеры:
  localai                              # чат в терминале
  localai serve                        # открыть http://localhost:8080
  localai serve -config prod.yaml      # запуск с файлом конфига
  localai pull llama3.2:1b             # скачать модель
  localai -model mistral:7b chat       # чат с другой моделью
  localai config init                  # создать localai.yaml
`)
}
