# LocalAI v3.1 — локальный AI-ассистент

Работает без интернета и облака. Go-приложение, **6.5 MB**, **0 зависимостей**.

---

## Быстрый старт

### Вариант 1 — Docker (рекомендуется)

```bash
# Клонировать и запустить
git clone https://github.com/Fipoh455r/Progi.git && cd Progi
docker compose up -d

# Открыть браузер
open http://localhost:8080
```

### Вариант 2 — Go-бинарник

```bash
cd go
go build -ldflags="-w -s" -o localai .
./localai serve         # веб-интерфейс на http://localhost:8080
./localai chat          # чат в терминале
```

### Вариант 3 — Авто-установка Linux

```bash
curl -sSL https://raw.githubusercontent.com/Fipoh455r/Progi/main/install.sh | bash
```

---

## Требования

| Компонент | Минимум | Рекомендуется |
|-----------|---------|---------------|
| ОС | Linux / macOS / Windows (WSL2) | Ubuntu 22.04 LTS |
| RAM | 512 MB | 4 GB+ |
| Диск | 2 GB | 10 GB+ |
| Go | 1.21+ | последняя |
| Ollama | любая | последняя |

---

## Команды

```bash
localai                          # чат в терминале (по умолчанию)
localai serve                    # веб-интерфейс http://localhost:8080
localai serve -config prod.yaml  # запуск с файлом конфигурации
localai models                   # список загруженных моделей
localai pull qwen2.5:1.5b        # скачать модель
localai config init              # создать localai.yaml с настройками
localai version                  # версия
```

---

## Конфигурация

```bash
# Создать файл конфигурации
localai config init

# Отредактировать localai.yaml, затем запустить
localai serve -config localai.yaml
```

Переменные окружения (альтернатива конфиг-файлу):

```bash
OLLAMA_URL=http://localhost:11434
LOCALAI_MODEL=qwen2.5:0.5b
LOCALAI_PORT=8080
LOCALAI_DATA=./data
```

---

## Возможности

| Функция | Описание |
|---------|----------|
| Чат | Потоковый чат с любой Ollama-моделью |
| Агент | ReAct-агент с 6 инструментами (calculator, web_search, read/write_file, http_get, datetime) |
| RAG | Загрузка документов (.txt, .md, .pdf, .json, .csv, .html) и поиск по ним |
| Авторизация | JWT + PBKDF2, роли admin/user, rate limiting |
| Голос | Whisper STT (транскрипция) + piper TTS (озвучка) |
| Кластер | Балансировщик нескольких Ollama-нод + circuit breaker |
| Метрики | Prometheus `/metrics` |
| OpenAI API | Совместимый `/v1/chat/completions` и `/v1/models` |

---

## Модели

| Модель | Размер | RAM | Качество |
|--------|--------|-----|----------|
| `qwen2.5:0.5b` | 400 MB | 512 MB | Базовое |
| `qwen2.5:1.5b` | 1 GB | 1 GB | Хорошее |
| `phi3:mini` | 2.3 GB | 2 GB | Хорошее |
| `llama3.2:3b` | 2 GB | 3 GB | Отличное |
| `mistral:7b` | 4.1 GB | 6 GB | Высокое |

```bash
localai pull llama3.2:3b
```

---

## Docker Compose профили

```bash
# Стандарт: LocalAI Go-app + Ollama
docker compose up -d

# Несколько нод Ollama (балансировка)
docker compose --profile multi up -d

# + Whisper STT
docker compose --profile multi --profile whisper up -d

# + Prometheus мониторинг
docker compose --profile multi --profile monitoring up -d
```

---

## Kubernetes (Helm)

```bash
helm install localai ./helm/localai \
  --set ollama.url=http://my-ollama:11434 \
  --set ingress.enabled=true \
  --set ingress.hosts[0].host=localai.example.com
```

---

## Мульти-нодовый режим

```bash
# Запуск с несколькими нодами Ollama
LOCALAI_OLLAMA_NODES=http://node1:11434,http://node2:11434 localai serve

# Метрики балансировщика
curl http://localhost:8080/metrics | grep localai_healthy_nodes
```

---

## Файлы проекта

```
go/                  Go-приложение (main логика)
  main.go            CLI точка входа
  server.go          HTTP-сервер с graceful shutdown
  config.go          YAML-конфигурация
  agent.go           ReAct-агент
  rag.go             RAG (поиск по документам)
  balancer.go        Балансировщик нод Ollama
  metrics.go         Prometheus метрики
  auth.go            Авторизация (JWT)
  audio.go           Голос (Whisper + piper)
helm/localai/        Helm-чарт для Kubernetes
docker-compose.yml   Docker-стек
install.sh           Авто-установщик Linux
localai.yaml         (создаётся через: localai config init)
```

---

## Для разработчиков

```bash
cd go
go build -o /dev/null ./...  # проверка компиляции
go test ./...                # 26 unit-тестов
```

Смотри `AGENTS.md` и `go/AGENTS.md` — навигация по коду для AI-агентов.

---

## Лицензия

MIT — используй свободно.
