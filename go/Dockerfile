# ============================================================
#  LocalAI Go — multi-stage Docker build
#  Итоговый образ: ~15 MB, нет зависимостей
# ============================================================

# ── Этап 1: сборка бинарника ──────────────────────────────
FROM golang:1.21-alpine AS builder

# Нужен только git для go mod (если зависимости с VCS)
RUN apk add --no-cache git

WORKDIR /src

# Сначала копируем только go.mod для кэша слоёв
COPY go.mod ./
RUN go mod download

# Копируем весь исходный код
COPY . .

# Собираем статический бинарник
# -w -s  — убираем отладочную информацию (меньше размер)
# CGO_ENABLED=0 — статическая линковка (работает на любом Linux)
RUN CGO_ENABLED=0 GOOS=linux go build \
      -ldflags="-w -s" \
      -o /localai \
      .

# ── Этап 2: минимальный runtime-образ ─────────────────────
FROM alpine:3.19

# ca-certificates нужен для HTTPS-запросов к Ollama (если TLS)
# tzdata — корректное время в логах
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /localai .

# Создаём директорию для данных (история чатов)
RUN mkdir -p /app/data

# Порт веб-интерфейса
EXPOSE 8080

# Переменные окружения (можно переопределить в docker-compose)
ENV OLLAMA_URL=http://ollama:11434
ENV LOCALAI_MODEL=qwen2.5:0.5b
ENV LOCALAI_PORT=8080
ENV LOCALAI_DATA=/app/data

# /app/data монтируется как volume снаружи для сохранения истории
VOLUME ["/app/data"]

# Запускаем в режиме веб-сервера
CMD ["./localai", "serve"]
