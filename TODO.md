# TODO — LocalAI (текущие задачи)

> Обновляется после каждой сессии. ~150 токенов.

---

## СЕЙЧАС (v3.3 в PR#6)

- [x] Кэш ответов LLM (`cache.go`)
  - SHA256-ключ(модель+temp+сообщения) → JSON в `data/cache/<2hex>/<sha256>.json`
  - TTL 1 час (настраивается), фоновая очистка, статистика `/api/cache/stats`
  - `CachedChat(ctx,client,msgs,model,temp)` — автоматически кэширует / отдаёт из кэша
  - Конфиг: `cache_enabled`, `cache_ttl_hours`; env `LOCALAI_CACHE_ENABLED/TTL_HOURS`
- [x] Пул специализированных агентов (`agent_pool.go`)
  - 12 встроенных ролей: coder, debugger, reviewer, planner, researcher, writer,
    summarizer, critic, translator, analyst, math, security
  - Компактные промпты (~50-80 токенов vs ~400 у общего агента)
  - Пользовательские роли: `data/agents/<name>.json` (CRUD)
  - `RunRoleAgent(ctx,client,role,task,model,stepCh)` — выполнение специалиста
- [x] Оркестратор (`orchestrator.go`)
  - `OrchestrateTask`: planner декомпозирует → параллельный запуск → merge
  - SSE-прогресс: plan/assigned/done/cache_hit/merge/result/error
  - `matchRole(task)` — автоматический выбор лучшей роли по ключевым словам
- [x] Инструмент `agent_call` (`tools.go`)
  - делегирует подзадачу специалисту: `{"role":"coder","task":"..."}`
  - регистрируется через `init()` (избегает цикла инициализации)
- [x] Новые API-маршруты (`server.go`)
  - `GET  /api/agents[?tag=code]` — список ролей
  - `POST /api/multiagent` — SSE оркестрация
  - `GET  /api/cache/stats` — статистика кэша

**Статус PR#6:** https://github.com/Fipoh455r/Progi/pull/6 (стакован: PR#5 → PR#6)

---

## СЛЕДУЮЩИЕ ЗАДАЧИ (v3.4)

### Приоритет 1 — UI

- [ ] Тёмная/светлая тема (CSS переменные + toggle)
- [ ] Мобильный вид (responsive breakpoints)
- [ ] Экспорт истории в Markdown/JSON
- [ ] Поиск по истории диалогов
- [ ] Панель мульти-агента в браузере (прогресс оркестратора в реальном времени)

### Приоритет 2 — Инструменты

- [ ] `shell_exec` — выполнение shell-команд (с подтверждением пользователя)
- [ ] `code_run` — запуск Python в изолированной среде

### Приоритет 3 — Производительность

- [ ] Семантический кэш (embedding-ближайшие соседи вместо точного SHA256)
- [ ] Очередь задач для фонового агента (task_queue.go)

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
| v3.3 | PR#6 | кэш LLM + пул 12 агентов + оркестратор + agent_call |
