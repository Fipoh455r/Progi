# TODO — LocalAI (текущие задачи)

> Обновляется после каждой сессии. ~150 токенов.

---

## СЕЙЧАС (v3.2 в PR#5)

- [x] Логирование в файл с ротацией
  - `logger.go`: `InitLogger(path)` + `fileLogger.Write` с ротацией >10MB → gzip
  - флаг `-log path` и `log_file` в localai.yaml; env `LOCALAI_LOG_FILE`
- [x] Исправлен баг agent-сессий (`agent_` prefix)
  - `agent_default` если пустой ID, `agent_<id>` если задан
  - `/api/sessions` фильтрует agent_ по умолчанию; `?include_agent=true` — показать
- [x] DOCX поддержка (stdlib ZIP+XML, без зависимостей)
  - `.docx` — pure Go: `archive/zip` + `encoding/xml`
  - `.doc` — через `antiword` (если установлен)
- [x] Инструмент агента `memory`
  - операции: `save | load | list | delete`
  - хранит факты в `data/memory/facts.json`

**Статус PR#5:** https://github.com/Fipoh455r/Progi/pull/5 (стакован: PR#4 → PR#5)

---

## СЛЕДУЮЩИЕ ЗАДАЧИ (v3.3)

### Приоритет 1 — UI

- [ ] Тёмная/светлая тема (CSS переменные + toggle)
- [ ] Мобильный вид (responsive breakpoints)
- [ ] Экспорт истории в Markdown/JSON
- [ ] Поиск по истории диалогов

### Приоритет 2 — Новые инструменты агента

- [ ] `shell_exec` — выполнение shell-команд (с подтверждением пользователя)
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
| v3.2 | PR#5 | лог в файл, фикс agent-сессий, DOCX, memory |
