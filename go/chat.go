// chat.go — интерактивный чат в терминале.
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
)

// ANSI цвета
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorPurple = "\033[35m"
	colorCyan   = "\033[36m"
	colorGray   = "\033[90m"
)

// systemPrompt — инструкция модели о её роли.
const systemPrompt = `Ты LocalAI — полезный, умный и дружелюбный AI-ассистент.
Ты работаешь полностью локально на компьютере пользователя, без интернета и облака.
Отвечай по-русски если пользователь пишет по-русски, иначе на языке пользователя.
Отвечай чётко и по делу, без лишней воды. Если не знаешь ответ — честно скажи об этом.`

// runChat запускает интерактивный чат в терминале.
func runChat(ollamaURL, model string) {
	client := NewOllamaClient(ollamaURL)

	printBanner()
	fmt.Printf("  Модель:  %s%s%s\n", colorYellow, model, colorReset)
	fmt.Printf("  Ollama:  %s%s%s\n", colorYellow, ollamaURL, colorReset)
	fmt.Printf("  Помощь:  введи %s/help%s\n\n", colorCyan, colorReset)

	if !client.IsAvailable() {
		fmt.Printf("%s[!] Ollama недоступен по адресу %s%s\n", colorRed, ollamaURL, colorReset)
		fmt.Println("    Запусти: docker run -d -p 11434:11434 ollama/ollama")
		os.Exit(1)
	}
	fmt.Printf("%s[✓] Ollama подключён%s\n\n", colorGreen, colorReset)

	// История диалога: начинаем с системного промпта
	history := []Message{
		{Role: "system", Content: systemPrompt},
	}

	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Printf("%sВы:%s ", colorCyan, colorReset)

		if !scanner.Scan() {
			// EOF (Ctrl+D) — выходим
			fmt.Printf("\n%sДо свидания!%s\n", colorGray, colorReset)
			return
		}

		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}

		// Обрабатываем команды
		if handled := handleCommand(input, &history, &model, client, ollamaURL); handled {
			continue
		}

		// Добавляем сообщение пользователя в историю
		history = append(history, Message{Role: "user", Content: input})

		// Получаем ответ от модели (с потоковой передачей)
		fmt.Printf("%sLocalAI:%s ", colorPurple, colorReset)

		response, ok := streamResponse(client, history, model)
		fmt.Println()
		fmt.Println()

		if !ok {
			// Ошибка — убираем сообщение пользователя из истории
			history = history[:len(history)-1]
			continue
		}

		// Добавляем ответ ассистента в историю
		history = append(history, Message{Role: "assistant", Content: response})
	}
}

// handleCommand обрабатывает команды, начинающиеся с '/'.
// Возвращает true если команда была обработана.
func handleCommand(input string, history *[]Message, model *string, client *OllamaClient, ollamaURL string) bool {
	if !strings.HasPrefix(input, "/") {
		return false
	}

	parts := strings.Fields(input)
	cmd := parts[0]

	switch cmd {
	case "/exit", "/quit", "/q":
		fmt.Printf("%sДо свидания!%s\n", colorGray, colorReset)
		os.Exit(0)

	case "/clear", "/new":
		*history = []Message{{Role: "system", Content: systemPrompt}}
		fmt.Printf("%s[История очищена]%s\n\n", colorYellow, colorReset)

	case "/model":
		if len(parts) < 2 {
			fmt.Printf("Текущая модель: %s%s%s\n\n", colorYellow, *model, colorReset)
			return true
		}
		*model = parts[1]
		fmt.Printf("%s[Модель: %s]%s\n\n", colorYellow, *model, colorReset)

	case "/models":
		runListModels(ollamaURL)

	case "/history":
		printHistory(*history)

	case "/help", "/?":
		printChatHelp()

	default:
		fmt.Printf("%s[Неизвестная команда: %s. Введи /help]%s\n\n", colorRed, cmd, colorReset)
	}

	return true
}

// streamResponse отправляет запрос и выводит токены по мере поступления.
// Возвращает полный текст ответа и флаг успеха.
func streamResponse(client *OllamaClient, history []Message, model string) (string, bool) {
	tokenCh, errCh := client.ChatStream(context.Background(), history, model)

	var sb strings.Builder
	for token := range tokenCh {
		fmt.Print(token)
		sb.WriteString(token)
	}

	if err := <-errCh; err != nil {
		fmt.Printf("\n%s[Ошибка] %s%s\n", colorRed, err, colorReset)
		return "", false
	}

	return sb.String(), true
}

// runListModels выводит список моделей доступных в Ollama.
func runListModels(ollamaURL string) {
	client := NewOllamaClient(ollamaURL)
	models, err := client.ListModels()
	if err != nil {
		fmt.Printf("%s[Ошибка] %s%s\n\n", colorRed, err, colorReset)
		return
	}

	if len(models) == 0 {
		fmt.Printf("%sНет загруженных моделей.%s\n", colorYellow, colorReset)
		fmt.Printf("Скачай модель: %slocalai pull qwen2.5:0.5b%s\n\n", colorCyan, colorReset)
		return
	}

	fmt.Printf("\n%sЗагруженные модели:%s\n", colorGreen, colorReset)
	for _, m := range models {
		sizeMB := m.Size / 1024 / 1024
		fmt.Printf("  %s%-30s%s %s(%d MB)%s\n",
			colorYellow, m.Name, colorReset,
			colorGray, sizeMB, colorReset)
	}
	fmt.Println()
}

// runPullModel скачивает модель из реестра Ollama с прогресс-баром.
func runPullModel(ollamaURL, modelName string) {
	client := NewOllamaClient(ollamaURL)

	fmt.Printf("Загрузка модели: %s%s%s\n", colorYellow, modelName, colorReset)

	lastStatus := ""
	err := client.PullModel(modelName, func(status string, pct int) {
		// Выводим только если статус изменился
		if status != lastStatus {
			fmt.Printf("\r%s%-50s%s", colorGray, status, colorReset)
			lastStatus = status
		}
		if pct > 0 {
			fmt.Printf(" %s%d%%%s", colorCyan, pct, colorReset)
		}
	})
	fmt.Println() // Перевод строки после прогресса

	if err != nil {
		fmt.Printf("%s[Ошибка] %s%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	fmt.Printf("%s[✓] Модель %s загружена%s\n", colorGreen, modelName, colorReset)
	fmt.Printf("    Запусти чат: %slocalai -model %s%s\n", colorCyan, modelName, colorReset)
}

// --- Вспомогательные функции вывода ---

func printBanner() {
	fmt.Printf("%s", colorCyan)
	fmt.Println("  ██╗      ██████╗  ██████╗ █████╗ ██╗      █████╗ ██╗")
	fmt.Println("  ██║     ██╔═══██╗██╔════╝██╔══██╗██║     ██╔══██╗██║")
	fmt.Println("  ██║     ██║   ██║██║     ███████║██║     ███████║██║")
	fmt.Println("  ██║     ██║   ██║██║     ██╔══██║██║     ██╔══██║██║")
	fmt.Println("  ███████╗╚██████╔╝╚██████╗██║  ██║███████╗██║  ██║██║")
	fmt.Println("  ╚══════╝ ╚═════╝  ╚═════╝╚═╝  ╚═╝╚══════╝╚═╝  ╚═╝╚═╝")
	fmt.Printf("%s\n", colorReset)
}

func printChatHelp() {
	fmt.Printf(`
%sКоманды:%s
  /help          — эта справка
  /clear, /new   — начать новый диалог
  /model         — показать текущую модель
  /model <name>  — сменить модель
  /models        — список загруженных моделей
  /history       — показать историю диалога
  /exit, /quit   — выход

%sСовет:%s Ctrl+C или Ctrl+D тоже выходят из программы.

`, colorYellow, colorReset, colorYellow, colorReset)
}

func printHistory(history []Message) {
	fmt.Printf("\n%sИстория диалога (%d сообщений):%s\n", colorYellow, len(history), colorReset)
	for i, msg := range history {
		if msg.Role == "system" {
			continue
		}
		preview := msg.Content
		if len(preview) > 80 {
			preview = preview[:77] + "..."
		}
		role := msg.Role
		color := colorCyan
		if role == "assistant" {
			color = colorPurple
			role = "LocalAI"
		}
		fmt.Printf("  %s%d. [%s]%s %s\n", color, i, role, colorReset, preview)
	}
	fmt.Println()
}
