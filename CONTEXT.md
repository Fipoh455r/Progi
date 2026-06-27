# CONTEXT — LocalAI (читать первым, ~600 токенов)

```
Репо:  github.com/Fipoh455r/Progi  ветка: main
Цель:  Локальный AI-агент (Go, stdlib only), без облака
Бинарь: ~6 MB  Образ: ~15 MB alpine  Зависимости: 0
```

## ФАЙЛЫ go/

```
main.go     CLI: chat|serve|models|pull|version  флаги: -ollama -model -port -data
ollama.go   OllamaClient: ChatStreamWithTemp / ListModels / PullModel / IsAvailable
            Типы: Message{Role,Content}  ModelInfo{Name,Size,ModifiedAt}
chat.go     runChat() — цветной ANSI терминал, /help /clear /model
storage.go  Storage.GetOrCreate/AppendAndSave/Load/Save/Delete/List
            Session{SessionMeta{ID,Title,Settings,CreatedAt,UpdatedAt},Messages[]}
compress.go CompressHistory(ctx,client,msgs,model)→(msgs,bool,err)
            TrimToTokenBudget(msgs,budget)  EstimateTokens(msgs)→int
            Константы: maxHistoryMessages=24  keepRecentMessages=8
tools.go    AllTools map[string]*ToolDef{Name,Description,ArgsSchema,Run}
            Инструменты: calculator|datetime|web_search|read_file|write_file|http_get
            RunTool(name,args)  ToolsPrompt()→string
agent.go    RunAgent(ctx,client,msgs,model,temp,stepCh)→(string,error)
            ReAct-цикл maxAgentSteps=8  agentTokenBudget=3500
            AgentStep{Kind,Content,ToolName,ToolArgs,Duration}
rag.go      RAG.AddDocument/Search/ListDocs/DeleteDoc
            chunkText(text,400,60)  cosineSimilarity  topK=4  порог=0.3
server.go   runServer(url,model,port,dataDir)  v2.0
            jsonOK(w,v)  withLogging(mux)  firstNonEmpty(vals...)
openai.go   registerOpenAIRoutes(mux,client,model)  /v1/models /v1/chat/completions
auth.go     (v2.2) UserStore  HashPassword  JWT HS256  RateLimiter  RequireAuth
static/index.html  SPA: sidebar+агент+RAG+настройки+логин (v2.2)
Dockerfile  multi-stage golang:1.21→alpine:3.19  VOLUME /app/data
go.mod      module github.com/Fipoh455r/Progi/go  go 1.21
```

## API

```
Public:
  GET  /                          → index.html
  GET  /health                    → {"ollama":bool,"docs":N}
  POST /api/auth/setup            → создать первого admin (только если 0 юзеров)
  POST /api/auth/login            → {username,password} → {token,role}

Auth required (Bearer JWT):
  GET  /api/models                → []ModelInfo
  GET  /api/tools                 → []ToolInfo
  GET  /api/sessions              → []SessionMeta
  GET/PATCH/DELETE /api/sessions/{id}
  POST /api/chat    {message,model,session_id,temperature,use_rag} → SSE tokens
  POST /api/agent   {message,model,session_id,temperature}         → SSE AgentStep
  POST /api/upload  multipart file → {name,chunks,size}
  GET  /api/docs / DELETE /api/docs/{id}
  POST /api/clear?session=id
  GET  /api/auth/me               → {username,role}

Admin only:
  POST /api/auth/register         → {username,password,role}
  DELETE /api/auth/users/{id}

OpenAI compat (auth required):
  GET  /v1/models
  POST /v1/chat/completions       → OpenAI format, stream+sync
```

## ВЕРСИИ

```
v1.0-v2.1  ✅  (чат, хранилище, UI, RAG, агент, OpenAI API)
v2.2       🔄  Авторизация ← ТЕКУЩАЯ
v2.3       ⬜  Голос (Whisper STT + piper TTS)
v3.0       ⬜  Кластер (балансировка + Helm + Prometheus)
```

## БЫСТРЫЙ СТАРТ

```bash
cd /workspace/Progi/go && go build -o /dev/null ./... && echo OK
go build -ldflags="-w -s" -o /tmp/localai . && ls -lh /tmp/localai
```

## ШАБЛОНЫ

```
Добавить инструмент:  tools.go → AllTools["name"] = &ToolDef{...}
Добавить API-маршрут: server.go → mux.HandleFunc("/api/...", handler)
Добавить защиту:      server.go → mux.Handle("/api/...", requireAuth(secret, store)(handler))
Изменить лимиты:      compress.go maxHistoryMessages/keepRecentMessages/agentTokenBudget
```
