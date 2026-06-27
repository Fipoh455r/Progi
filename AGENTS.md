# AGENTS.md — Инструкции для AI-агентов

## Структура проекта

```
Progi/
├── go/                  # Go-сервер (основное приложение)
│   ├── main.go          # точка входа, CLI-флаги, версия (appVersion)
│   ├── server.go        # HTTP-сервер, маршруты API
│   ├── rag.go           # RAG: индекс документов, чанкинг, эмбеддинги, Search/ListChunks
│   ├── hybrid_search.go # Гибридный поиск: BM25 + cosine similarity
│   ├── router.go        # Model Router: классификатор задач (без LLM)
│   ├── agent.go         # Агент с инструментами (ReAct-цикл)
│   ├── tools.go         # Инструменты агента (calculator, datetime, web_search, read/write_file, http_get)
│   ├── tools_code.go    # Инструменты кода (run_code, shell, list_dir, grep_file, detect_lang)
│   ├── tools_dev.go     # Dev-инструменты (git, http_request, json_query, diff, regex, encode)
│   ├── tools_data.go    # Data-инструменты (fetch_page, memory, sqlite)
│   ├── chat.go          # Интерактивный чат в терминале
│   ├── compress.go      # Сжатие истории (экономия токенов)
│   ├── ollama.go        # Клиент Ollama API
│   ├── openai.go        # OpenAI-совместимые маршруты (/v1/...)
│   ├── storage.go       # Хранилище сессий (JSON на диске)
│   └── static/          # Веб-интерфейс (index.html)
├── docker-compose.yml   # Продакшн: Go + Ollama
├── docker-compose.cpu.yml
├── docker-compose.goapp.yml
└── install.sh           # Установка
```

## Версионирование

- Версия хранится в `go/main.go`: `const appVersion = "X.Y.Z"`
- Баннер сервера в `go/server.go`: `LocalAI vX.Y`
- При изменении мажорных фич — обновлять обе строки

## Версия

Текущая: **v4.0.0**

## API эндпоинты

| Метод | URL | Описание |
|-------|-----|----------|
| POST | `/api/chat` | SSE-стрим чата. Тело: `{message, model, session_id, temperature, use_rag}` |
| POST | `/api/agent` | SSE-стрим агента с шагами. Тело: `{message, model, session_id, temperature}` |
| POST | `/api/upload` | Загрузка документа в RAG (multipart, поле `file`) |
| GET  | `/api/docs`   | Список документов в RAG |
| DELETE | `/api/docs/{id}` | Удалить документ из RAG |
| POST | `/api/router/classify` | Классификация задачи. Тело: `{query}`. Возврат: `{task, confidence, reason}` |
| GET  | `/api/sessions/cleanup` | Статистика сессий (кол-во, размер на диске) |
| POST | `/api/sessions/cleanup?days=30` | Удалить сессии старше N дней |
| GET  | `/api/models` | Список моделей Ollama |
| GET  | `/api/sessions` | Список сессий |
| POST `/v1/...` | | OpenAI-совместимый API |

## Гибридный поиск (v3.7+)

- `HybridSearcher` в `hybrid_search.go` объединяет BM25 и cosine similarity
- `alpha` параметр: 0.0 = только BM25, 1.0 = только cosine, -1 = дефолт (0.6)
- `RebuildBM25()` вызывается автоматически после каждого upload и delete
- BM25 параметры: k1=1.5, b=0.75 (стандартный Okapi BM25)

## Роутер задач (v3.7+)

- `ClassifyTask(query string) RouteResult` в `router.go`
- Работает без LLM: ключевые слова + эвристики (~microseconds)
- Типы: `chat`, `rag`, `agent`, `code`, `math`
- `confidence` в диапазоне [0.5, 0.98]

## Правила разработки

1. **Go build обязателен**: `cd go && go build -o /dev/null ./...` должен проходить без ошибок
2. **go vet**: запускать `go vet ./...` после каждого изменения
3. **Нет внешних зависимостей**: проект намеренно без сторонних библиотек (только stdlib)
4. **Русские комментарии**: основной язык комментариев в коде — русский
5. **Логи**: сервер логирует только не-streaming запросы (исключения: `/api/chat`, `/api/agent`, `/health`)
6. **Данные**: индекс RAG хранится в `{dataDir}/rag/rag_index.json`

## Запуск для разработки

```bash
cd go
go run .                      # чат в терминале
go run . serve                # веб-сервер на :8080
go build -o localai . && ./localai serve  # собрать и запустить
```

## Docker

```bash
docker compose up -d          # запуск с Ollama
docker compose logs -f goapp  # логи Go-сервера
```
