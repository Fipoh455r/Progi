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
package main

import (
	"flag"
	"fmt"
	"os"
)

const appVersion = "1.0.0"

func main() {
	// Флаги — переопределяют переменные окружения
	ollamaURL := flag.String("ollama", envOr("OLLAMA_URL", "http://localhost:11434"), "URL Ollama API")
	model := flag.String("model", envOr("LOCALAI_MODEL", "qwen2.5:0.5b"), "Языковая модель")
	port := flag.String("port", envOr("LOCALAI_PORT", "8080"), "Порт веб-сервера")
	dataDir := flag.String("data", envOr("LOCALAI_DATA", "./data"), "Каталог для хранения данных")
	flag.Usage = printHelp
	flag.Parse()

	args := flag.Args()
	cmd := "chat"
	if len(args) > 0 {
		cmd = args[0]
	}

	switch cmd {
	case "chat", "":
		runChat(*ollamaURL, *model)

	case "serve", "server", "web":
		runServer(*ollamaURL, *model, *port, *dataDir)

	case "models":
		runListModels(*ollamaURL)

	case "pull":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Использование: localai pull <имя-модели>")
			fmt.Fprintln(os.Stderr, "Пример:        localai pull qwen2.5:1.5b")
			os.Exit(1)
		}
		runPullModel(*ollamaURL, args[1])

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
  chat            чат в терминале (по умолчанию)
  serve           веб-интерфейс в браузере
  models          список загруженных моделей
  pull <модель>   скачать модель из Ollama
  version         версия программы

Флаги:
`, appVersion)
	flag.PrintDefaults()
	fmt.Print(`
Переменные окружения:
  OLLAMA_URL      URL Ollama API    (по умолч.: http://localhost:11434)
  LOCALAI_MODEL   Языковая модель   (по умолч.: qwen2.5:0.5b)
  LOCALAI_PORT    Порт веб-сервера  (по умолч.: 8080)

Примеры:
  localai                          # чат в терминале
  localai serve                    # открыть http://localhost:8080
  localai pull llama3.2:1b         # скачать модель
  localai -model mistral:7b chat   # чат с другой моделью
`)
}
