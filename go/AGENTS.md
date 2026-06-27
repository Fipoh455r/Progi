# go/AGENTS.md — Go-проект LocalAI

> Область действия: всё в директории `go/`. ~300 токенов.
> Прочитал `../AGENTS.md`? Если нет — прочитай его первым.

---

## ФАЙЛОВАЯ КАРТА

```
main.go            CLI точка входа. AppVersion="3.5.0". Команды: chat|serve|models|pull|config
                   Приоритет конфига: -flag > ENV > localai.yaml > DefaultConfig()
                   Флаги: -config -ollama -model -port -data -log
config.go          AppConfig{OllamaURL,Model,Port,DataDir,...,LogFile,CacheEnabled,CacheTTLHours}
                   LoadConfig(path) → MergeEnv() → CLI-флаги
                   WriteExample(path) — создаёт localai.yaml
logger.go          InitLogger(path) → error   — лог в файл + stderr, ротация >10MB → .gz
                   rotateSize=10MB  maxBackups=5   gzipFile / pruneBackups
cache.go           LLMCache{dir,ttl}   InitCache(dataDir,ttl) → error
                   CacheKey(model,temp,messages) → sha256   CachedChat(ctx,client,...) → (string,bool,err)
                   globalCache *LLMCache   pruneLoop (фоновая горутина)   TTL по умолч. 1 час
agent_pool.go      BuiltinRoles: 12 ролей (coder,debugger,reviewer,planner,researcher,writer,
                   summarizer,critic,translator,analyst,math,security)
                   GetRole(name) / AllRoles() / SaveCustomRole / DeleteCustomRole
                   RunRoleAgent(ctx,client,role,task,model,stepCh) — специализированный вызов
                   InitAgentPool(dataDir) — загружает data/agents/*.json
orchestrator.go    OrchestrateTask(ctx,client,task,model,progressCh) → (string,error)
                   planTask → planner декомпозирует, runSubtask (параллельно), mergeResults
                   OrchestratorEvent{Kind,Message,Agent,Index,Total,Elapsed}
os_exec.go         runCommand(name,args) → []byte,error
                   runCommandExists(name) → bool
ollama.go          OllamaClient{baseURL,http}
                   ChatStreamWithTemp(ctx,msgs,model,temp) → (tokenCh,errCh)
                   ListModels() → []ModelInfo    IsAvailable() → bool
storage.go         Session{SessionMeta{ID,Title,Settings,CreatedAt},Messages[]Message}
                   Storage.GetOrCreate / AppendAndSave / Load / Save / Delete / List
compress.go        CompressHistory(ctx,client,msgs,model) → (msgs,bool,err)
                   TrimToTokenBudget(msgs,budget)   EstimateTokens(msgs) → int
                   maxHistoryMessages=24  keepRecentMessages=8  agentTokenBudget=3500
context_manager.go FilterByRelevance(messages,query,topN) → []Message   [TF keyword scoring]
                   SmartContext(messages,query,budget,topN) → (msgs,origTok,filteredTok)
                   tokenize(text) → map[string]bool   relevanceScore(content,queryWords) → float64
                   defaultTopN=12  minWordLen=3  pairBonus=0.15  свежесть: +0.5 на последние 4
templates.go       PromptTemplate{Name,Description,Prompt,Tokens}   TemplateInfo (листинг)
                   GetTemplate(name) → (PromptTemplate,bool)   ListTemplates() → []TemplateInfo
                   ApplyTemplate(messages,name) → []Message   — заменяет system prompt
                   12 шаблонов: default,code,debug,review,explain,translate,brief,
                                tutor,writer,analyst,security,devops
token_stats.go     RecordCompression(before,after int)   RecordContextFilter(before,after)
                   RecordTemplateUsage(saved int)   GetTokenStats() → TokenSavingsStats
                   InitTokenStats(dataDir) — загружает/сохраняет data/token_stats.json
                   globalTokenStats *tokenStatsState — атомарные счётчики, flush каждые 5 мин
tools.go           AllTools map[string]*ToolDef   RunTool(name,args) → (string,error)
                   ToolsPrompt() → string   (8 инструментов: +memory +agent_call)
                   SetMemoryDir(dataDir) / SetToolsClient(client,model)
                   memory: save|load|list|delete → data/memory/facts.json
                   agent_call: делегирует подзадачу роли → RunRoleAgent (init() регистрирует)
agent.go           RunAgent(ctx,client,msgs,model,temp,stepCh) → (string,error)
                   ReAct-цикл: maxAgentSteps=8   AgentStep{Kind,Content,ToolName,...}
rag.go             RAG.AddDocument(ctx,name,text) → (chunks,err)   [batch parallel ×4]
                   RAG.Search(ctx,query,topK) → []SearchResult   порог=0.3
                   BuildContextString(results) → string
swarm.go           RunSwarm(ctx,client,job,progressCh) → (SwarmResult,error)  — рой 100 агентов
                   SwarmJob{Text,Question,Model,MaxAgents}   SwarmResult{Answer,ChunkCount,...}
                   SwarmEvent{Kind,Index,Total,Pass,Remaining,Message,Elapsed,Result}
                   splitTextIntoChunks(text,targetWords,overlap) → []string  [overlap=25 слов]
                   swarmSelectChunks(chunks,question,maxN) → []string  [TF keyword, из context_manager]
                   swarmProcessChunks — параллельно: semaphore swarmMaxAgents=100 горутин
                   swarmPyramidalMerge — O(log₂ N) проходов, swarmMergeConcurrency=12 параллельно
                   Промпты: swarmAnalystPrompt(~25 tok) / swarmMergePrompt(~20) / swarmFinalPrompt(~30)
                   Экономия: 100M токенов задача → ~50K через relevance filter + compact prompts
server.go          runServer(url,model,port,dataDir,cacheEnabled,cacheTTLHours)  v3.5
                   HTTP сервер с graceful shutdown (SIGTERM/SIGINT → 10s drain)
                   extractText: .pdf(pdftotext) .docx(stdlib ZIP+XML) .doc(antiword) .html(strip)
                   /api/chat: +smart_context(bool) +template(string) → SmartContext/ApplyTemplate
                   /api/swarm: POST SSE рой агентов (text+question → ответ через 100 агентов)
                   /api/sessions: фильтрует agent_ сессии; ?include_agent=true — показать все
                   /api/agents: список ролей агентов (?tag=code)
                   /api/multiagent: SSE OrchestrateTask (plan→parallel→merge)
                   /api/cache/stats: статистика кэша (hits/misses/entries/hit_rate)
                   /api/templates: GET список / GET /{name} полный шаблон
                   /api/token-stats: сводка экономии токенов (compression/context/templates)
openai.go          registerOpenAIRoutes → GET /v1/models  POST /v1/chat/completions
auth.go            UserStore(JSON)  HashPassword(PBKDF2-HMAC-SHA256-100k)
                   GenerateJWT / ValidateJWT (HS256, 24h)
                   RateLimiter(per-IP sliding window)
                   RequireAuth(secret) / RequireAdmin(secret) → http.Handler middleware
audio.go           WhisperClient.Transcribe(ctx,file) → string   [proxy к Whisper HTTP]
                   PiperTTS.Synthesize(text,voice) → []byte      [exec piper binary]
                   registerAudioRoutes: /api/audio/transcriptions, /speech, /status
balancer.go        OllamaBalancer: round-robin + circuit breaker (3 fails → down, 60s recovery)
                   NewOllamaBalancer(primaryURL) — читает LOCALAI_OLLAMA_NODES (запятая)
                   Pick() / ReportSuccess(client) / ReportFailure(client) / HealthyCount()
                   health check: каждые 15 сек в фоне
metrics.go         NewMetrics(balancer) → *Metrics   registerMetricsRoute → GET /metrics
                   Prometheus text 0.0.4, 0 зависимостей
                   Счётчики: ChatReqs,AgentReqs,Tokens,Uploads,LoginOK,LoginFail,ChatErrs,AgentErrs
                   Гистограммы: ChatDuration,AgentDuration   Gauge: ActiveConns,HealthyNodes
```

---

## ПЕРЕМЕННЫЕ ОКРУЖЕНИЯ

```
OLLAMA_URL              http://localhost:11434
LOCALAI_MODEL           qwen2.5:0.5b
LOCALAI_PORT            8080
LOCALAI_DATA            ./data
LOCALAI_OLLAMA_NODES    url1,url2,url3   (балансировщик)
LOCALAI_WHISPER_URL     http://localhost:8081
LOCALAI_PIPER_BIN       /usr/bin/piper
LOCALAI_PIPER_VOICES_DIR ./data/voices
LOCALAI_PIPER_VOICE     en_US-lessac-medium
LOCALAI_JWT_SECRET       (пустое = авто-генерация)
LOCALAI_METRICS_ENABLED  true
LOCALAI_LOG_FILE         (пустое = только stderr; пример: /var/log/localai/app.log)
LOCALAI_CACHE_ENABLED    true
LOCALAI_CACHE_TTL_HOURS  1   (0 = бессрочный кэш)
```

---

## КОМАНДЫ

```bash
go build -o /dev/null ./...          # проверка компиляции (ВСЕГДА перед коммитом)
go test ./...                        # 26 тестов (ВСЕГДА перед коммитом)
go test -run TestXxx -v              # запустить один тест
go build -ldflags="-w -s" -o /tmp/localai .   # итоговый бинарник ~6.5MB
```

---

## ТИПИЧНЫЕ ЗАДАЧИ

```
Новый инструмент агента:
  → tools.go: AllTools["name"] = &ToolDef{Name, Description, ArgsSchema, Run: func}
  → агент подхватит автоматически (ToolsPrompt включает его в системный промпт)

Новый API-маршрут (с авторизацией):
  → server.go: mux.Handle("/api/...", protected(func(w, r) { ... }))
  → для admin: adminOnly(handler)   для публичного: mux.HandleFunc(...)

Новый конфиг-параметр:
  → config.go: добавь поле в AppConfig → парси в LoadConfig kv["ключ"] → добавь в MergeEnv
  → main.go: добавь флаг и передай в runServer если нужно

Новый тест:
  → *_test.go рядом с тестируемым файлом, пакет main
  → go test -run TestИмя -v

Изменить RAG-параметры:
  → rag.go: chunkText(400, 60) → (targetWords, overlap)
  → server.go: rag.Search(ctx, msg, 4) → topK
```

---

## ЧАСТЫЕ ОШИБКИ

```
duplicate func     → проверь все *_test.go и os_exec.go
go:embed missing   → static/index.html должен существовать до go build
SSE не работает    → X-Accel-Buffering: no (уже в server.go)
JWT 401            → Bearer токен в заголовке Authorization
PDF пустой         → pdftotext не установлен (apt install poppler-utils)
config parse error → двоеточие в значении → взять в кавычки: "http://host:port"
```

---

## СТРУКТУРА ТЕСТОВ

```
tools_test.go    TestEvalExpr_* / TestTool* / TestRunTool* / TestToolsPrompt
compress_test.go TestEstimateTokens_* / TestTrimToTokenBudget_*
config_test.go   TestDefaultConfig / TestLoadConfig_* / TestWriteExample / TestParseBool / TestMergeEnv
```
