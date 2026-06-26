# LocalAI — локальный AI-ассистент

Работает без интернета и облака. Устанавливается одной командой на любой Linux.

---

## Быстрая установка

```bash
curl -sSL https://raw.githubusercontent.com/Fipoh455r/Progi/main/install.sh | bash
```

После установки открой браузер: **http://localhost:3000**

---

## Требования

| Компонент | Минимум | Рекомендуется |
|-----------|---------|---------------|
| ОС | Linux (Ubuntu 20+, Debian 11+, CentOS 8+, Arch) | Ubuntu 22.04 LTS |
| RAM | 512 MB | 4 GB+ |
| Диск | 2 GB | 10 GB+ |
| Docker | 20.10+ | Последняя версия |
| GPU | Не нужен | NVIDIA (для скорости) |

Docker устанавливается **автоматически** если его нет.

---

## Как это работает

```
Браузер → Open WebUI (порт 3000) → Ollama (порт 11434) → Языковая модель
```

- **Ollama** — движок для запуска LLM локально
- **Open WebUI** — веб-интерфейс (как ChatGPT, только у тебя на компьютере)
- **qwen2.5:0.5b** — лёгкая модель (~400MB), работает на слабом железе

---

## Управление

После установки доступна команда `localai`:

```bash
localai start       # запустить
localai stop        # остановить
localai restart     # перезапустить
localai status      # статус контейнеров
localai logs        # логи веб-интерфейса
localai models      # список загруженных моделей
localai update      # обновить до последней версии

# Скачать другую модель:
localai pull llama3.2:1b
localai pull mistral:7b
```

---

## Модели

Выбирай модель под своё железо:

| Модель | Размер | RAM | Качество |
|--------|--------|-----|----------|
| `qwen2.5:0.5b` | ~400 MB | 512 MB | Базовое |
| `qwen2.5:1.5b` | ~1 GB | 1 GB | Хорошее |
| `phi3:mini` | ~2.3 GB | 2 GB | Хорошее |
| `llama3.2:3b` | ~2 GB | 3 GB | Отличное |
| `mistral:7b` | ~4.1 GB | 6 GB | Отличное |

Скачать модель:
```bash
localai pull qwen2.5:1.5b
```

---

## Ручная установка (без скрипта)

```bash
# 1. Клонируй репозиторий
git clone https://github.com/Fipoh455r/Progi.git localai
cd localai

# 2. Скопируй настройки
cp .env.example .env

# 3. Запусти (CPU)
docker compose -f docker-compose.cpu.yml up -d

# 4. Или с GPU (NVIDIA)
docker compose up -d
```

---

## Файлы проекта

```
.
├── docker-compose.yml        # Основной (с поддержкой GPU)
├── docker-compose.cpu.yml    # Для CPU без GPU
├── install.sh                # Авто-установщик
└── .env.example              # Пример настроек
```

---

## Автозапуск

Скрипт автоматически создаёт systemd-сервис — LocalAI запускается при старте системы.

Управление сервисом:
```bash
sudo systemctl status localai
sudo systemctl stop localai
sudo systemctl disable localai  # отключить автозапуск
```

---

## Удаление

```bash
# Остановить и удалить контейнеры
localai stop
docker rm localai-ollama localai-webui

# Удалить модели и данные (осторожно — все данные удалятся)
docker volume rm progi_ollama-models progi_webui-data

# Удалить файлы
rm -rf ~/.localai
sudo rm /usr/local/bin/localai
sudo rm /etc/systemd/system/localai.service
```

---

## Лицензия

MIT — используй свободно.
