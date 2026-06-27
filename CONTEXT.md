# CONTEXT — LocalAI v3.1 (читать первым, ~600 токенов)

```
Репо:  github.com/Fipoh455r/Progi  ветка: main (PR#4 stacked on PR#3)
Цель:  Локальный AI-агент (Go, stdlib only), без облака
Бинарь: ~6 MB  Образ: ~15 MB alpine  Зависимости: 0  Версия: 3.1.0
```

## ФАЙЛЫ go/

```
main.go     CLI: chat|serve|models|pull|config  флаги: -ollama -model -port -data -config
            DefaultConfig/LoadConfig/MergeEnv → приоритет: CLI > env > localai.yaml > defaults
ollama.go   OllamaClient: ChatStreamWithTemp / ListModels / PullModel / IsAvailable
chat.go     runChat() — цветной ANSI терминал, /help /clear /model
storage.go  Storage.GetOrCreate/AppendAndSave  Session{SessionMeta,Messages[]}
compress.go CompressHistory / TrimToTokenBudget / EstimateTokens
            maxHistoryMessages=24  keepRecentMessages=8  agentTokenBudget=3500
tools.go    AllTools: calculator|datetime|web_search|read_file|write_file|http_get
            RunTool(name,args)  ToolsPrompt()→string
agent.go    RunAgent(ctx,client,msgs,model,temp,stepCh)→(string,error)
            ReAct: maxAgentSteps=8  AgentStep{Kind,Content,ToolName,ToolArgs,Duration}
rag.go      RAG.AddDocument/Search/ListDocs/DeleteDoc
            embedChunks(parallel sem=4)  chunkText(400,60)  topK=4  порог=0.3
server.go   runServer() v3.1: graceful shutdown (SIGTERM→10s drain)
            extractText+extractPDF(pdftotext)  jsonOK  withLogging
openai.go   /v1/models  /v1/chat/completions (stream+sync)
auth.go     UserStore  HashPassword(PBKDF2)  JWT HS256  RateLimiter  RequireAuth
audio.go    WhisperClient.Transcribe / PiperTTS.Synthesize / registerAudioRoutes
balancer.go OllamaBalancer: Pick/ReportSuccess/Failure  round-robin+circuit breaker
            env LOCALAI_OLLAMA_NODES  healthCheck 15s  recovery 60s
metrics.go  Metrics Prometheus text 0.0.4  GET /metrics
config.go   AppConfig  LoadConfig(yaml)  MergeEnv()  WriteExample(path)
os_exec.go  runCommand / runCommandExists / lookupEnvBool
*_test.go   26 тестов: tools_test, compress_test, config_test
```

## API

```
Public:
  GET  /health                    → {"ollama":bool,"docs":N}
  POST /api/auth/setup            → первый admin
  POST /api/auth/login            → {token,role}
  GET  /api/auth/status           → {needs_setup:bool}

Auth (Bearer JWT):
  POST /api/chat    {message,model,session_id,temperature,use_rag} → SSE
  POST /api/agent   {message,model,session_id,temperature}         → SSE AgentStep
  POST /api/upload  multipart(.txt/.md/.pdf/.json/...) → {name,chunks,size}
  GET/DELETE /api/docs/{id}
  GET/PATCH/DELETE /api/sessions/{id}
  GET  /api/models  /api/tools  /api/auth/me

Admin: POST /api/auth/register  DELETE /api/auth/users/{id}
OpenAI: GET /v1/models  POST /v1/chat/completions
Audio:  POST /api/audio/transcriptions  POST /api/audio/speech
Metrics: GET /metrics
```

## ВЕРСИИ

```
v1.0-v2.1  ✅  чат, хранилище, UI, RAG, агент, OpenAI API
v2.2       ✅  Авторизация (JWT+PBKDF2+RateLimiting)
v2.3       ✅  Голос (Whisper STT + piper TTS + MediaRecorder)
v3.0       ✅  Кластер (OllamaBalancer + Prometheus + Helm)
v3.1       ✅  Техдолг (graceful shutdown + YAML config + тесты + PDF + batch embed)
Далее      ⬜  v3.2 UI улучшения / логирование в файл
```

## БЫСТРЫЙ СТАРТ

```bash
cd /workspace/Progi/go
go build -o /dev/null ./... && echo BUILD_OK
go test ./... && echo TESTS_OK
go build -ldflags="-w -s" -o /tmp/localai . && ls -lh /tmp/localai
localai config init   # создать localai.yaml
```

## ШАБЛОНЫ

```
Инструмент:   tools.go → AllTools["name"] = &ToolDef{..., Run: func(args)(string,error)}
API-маршрут:  server.go → mux.Handle("/api/...", protected(handler))
Конфиг-поле:  config.go AppConfig + LoadConfig kv + MergeEnv os.Getenv
Лимиты:       compress.go maxHistoryMessages/keepRecentMessages/agentTokenBudget
```
