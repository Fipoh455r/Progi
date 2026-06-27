# TODO — LocalAI (текущие задачи)

> Обновляется после каждой сессии. ~150 токенов.

---

## СЕЙЧАС (v3.1 в PR#4)

- [x] Graceful shutdown (SIGTERM → 10s drain)
- [x] YAML config (`localai config init`, `-config path`)
- [x] Unit-тесты (26 шт: tools, compress, config)
- [x] PDF через pdftotext
- [x] Batch embedding (семафор ×4)
- [x] AGENTS.md + go/AGENTS.md (AI-навигация, экономия токенов)

**Статус PR#4:** https://github.com/Fipoh455r/Progi/pull/4

---

## СЛЕДУЮЩИЕ ЗАДАЧИ (v3.2)

### Приоритет 1 — Технический долг

- [ ] Логирование в файл с ротацией
  - `log/slog` → файл + ротация по размеру (>10MB → gzip)
  - флаг `-log path` или `log_file` в localai.yaml
- [ ] Исправить: агент-сессии (`agent_` prefix) видны отдельно от обычных
  - в `/api/sessions` → фильтровать или объединить
- [ ] DOCX поддержка (`antiword` или `docx2txt`)

### Приоритет 2 — UI

- [ ] Тёмная/светлая тема (CSS переменные + toggle)
- [ ] Мобильный вид (responsive breakpoints)
- [ ] Экспорт истории в Markdown/JSON
- [ ] Поиск по истории диалогов

### Приоритет 3 — Новые инструменты агента

- [ ] `shell_exec` — выполнение shell-команд (с подтверждением пользователя)
- [ ] `memory` — долгосрочная память (JSON-файл с фактами о пользователе)
- [ ] `code_run` — запуск Python в изолированной среде

---

## СДЕЛАНО (архив)

| Версия | PR | Что |
|--------|-----|-----|
| v1.0–v2.1 | main | чат, хранилище, UI, RAG, агент, OpenAI API |
| v2.2 | PR#1 | авторизация JWT+PBKDF2 |
| v2.3 | PR#2 | голос Whisper+piper |
| v3.0 | PR#3 | кластер: балансировщик+Prometheus+Helm |
| v3.1 | PR#4 | техдолг: shutdown+config+тесты+PDF+batch |
