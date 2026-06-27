// os_exec.go — обёртки для запуска внешних команд (stdlib only).
// Используется для PDF-извлечения (pdftotext) и проверки наличия команд.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runCommand запускает внешнюю команду и возвращает её stdout.
// Таймаут не установлен — вызывающий код должен использовать context если нужно.
func runCommand(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", err, msg)
	}
	return stdout.Bytes(), nil
}

// runCommandExists возвращает true если команда name найдена в PATH.
func runCommandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// lookupEnvBool читает булеву переменную окружения; при отсутствии возвращает fallback.
func lookupEnvBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	return parseBool(v, fallback)
}
