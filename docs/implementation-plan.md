# Implementation Plan

> Last updated: 2026-03-22

This document is the ordered build roadmap. Each milestone produces something runnable. Work top-to-bottom; later milestones depend on earlier ones.

---

## Milestone 0 — Environment setup

**Goal**: Go toolchain installed, module dependencies resolved.

```bash
mise use go@1.23
cd /home/dr14/dicode
go mod tidy
go build ./...   # should compile with zero errors (all stubs are valid Go)
```

Nothing to write — just get the toolchain working.

---

## Milestone 1 — Storage layer

**Goal**: SQLite backend working. Everything else needs storage.

### `pkg/db/sqlite.go`
Implement the `DB` interface for SQLite using `modernc.org/sqlite` (pure Go, no CGo).

Tables needed at this stage:
- `runs` — `(id TEXT PK, task_id TEXT, status TEXT, started_at INTEGER, finished_at INTEGER, parent_run_id TEXT)`
- `run_logs` — `(id INTEGER PK, run_id TEXT, ts INTEGER, level TEXT, message TEXT)`
- `kv` — `(key TEXT PK, value TEXT)` — task KV store
- `secrets` — `(key TEXT PK, ciphertext BLOB)` — encrypted secrets

```go
type SQLiteDB struct {
    db *sql.DB
}
func openSQLite(cfg Config) (DB, error)
```

### `pkg/secrets/local.go` — complete sqlite backend
Wire the `localDB` interface to the new `SQLiteDB`. Unblock `LocalProvider.Set/Get/Delete/List`.

**Deliverable**: `dicode secrets set/get/list/delete` commands work.

---

## Milestone 2 — Local source

**Goal**: dicode can watch a local directory and detect task changes.

### `pkg/source/local/local.go`
Implement `Source` interface using `fsnotify`.

```go
type LocalSource struct {
    id      string
    path    string
    watch   bool
}

func New(cfg config.SourceConfig) (*LocalSource, error)
func (s *LocalSource) ID() string
func (s *LocalSource) Start(ctx context.Context) (<-chan source.Event, error)
func (s *LocalSource) Sync(ctx context.Context) error
```

Logic:
1. `Sync()` — calls `task.ScanDir(path)`, emits Added/Updated/Removed events by diffing against previous snapshot
2. `Start()` — launches `Sync()` once, then (if `watch: true`) starts fsnotify watcher that calls `Sync()` on each relevant change
3. Debounce writes (100ms) to avoid partial-write events

**Deliverable**: local source emits correct events when task files are added/changed/deleted.

---

## Milestone 3 — Task registry

**Goal**: in-memory task registry backed by sqlite run log.

### `pkg/registry/registry.go`

```go
type Registry struct {
    mu    sync.RWMutex
    tasks map[string]*task.Spec   // taskID → spec
    db    db.DB
}

func New(db db.DB) *Registry
func (r *Registry) Register(spec *task.Spec) error   // upsert
func (r *Registry) Unregister(id string)
func (r *Registry) Get(id string) (*task.Spec, bool)
func (r *Registry) All() []*task.Spec
func (r *Registry) StartRun(taskID string, parentRunID string) (runID string, err error)
func (r *Registry) FinishRun(runID string, status string) error
func (r *Registry) AppendLog(runID string, level string, msg string) error
func (r *Registry) GetRun(runID string) (*Run, error)
func (r *Registry) ListRuns(taskID string, limit int) ([]*Run, error)
```

### `pkg/registry/reconciler.go`

Consumes events from one or more `Source` channels and applies them to the registry.

```go
type Reconciler struct {
    registry *Registry
    sources  []source.Source
}

func NewReconciler(registry *Registry, sources []source.Source) *Reconciler
func (r *Reconciler) Run(ctx context.Context) error
```

Logic: fan-in all source channels, handle Added/Updated → `registry.Register()`, Removed → `registry.Unregister()`.

**Deliverable**: dicode loads tasks from a local directory into the registry on startup and keeps it live.

---

## Milestone 4 — JS runtime

**Goal**: tasks can execute. The most complex milestone.

### `pkg/runtime/js/runtime.go`

```go
type Runtime struct {
    registry *registry.Registry
    secrets  secrets.Chain
    db       db.DB
}

func New(registry *Registry, secrets secrets.Chain, db db.DB) *Runtime
func (r *Runtime) Run(ctx context.Context, spec *task.Spec, input interface{}) (*RunResult, error)
```

Each call to `Run()`:
1. Creates a fresh `goja.Runtime`
2. Resolves secrets for all `spec.Env` keys
3. Injects globals (see below)
4. Compiles + runs `task.js`
5. Returns the JS return value + captured logs

### Globals to implement (in order of priority)

| Global | File | Priority |
|---|---|---|
| `log` | `globals/log.go` | MVP |
| `env` | `globals/env.go` | MVP |
| `params` | `globals/params.go` | MVP |
| `http` | `globals/http.go` | MVP |
| `kv` | `globals/kv.go` | MVP |
| `output` | `globals/output.go` | MVP |
| `fs` | `globals/fs.go` | MVP |
| `notify` | `globals/notify.go` | Post-MVP |
| `input` | `globals/input.go` | Post-MVP (chain) |
| `dicode` | `dicode.go` | Post-MVP (progress, trigger, isRunning, query methods) |
| `server` | `globals/server.go` | North star (daemon tasks) |

### `pkg/runtime/js/globals/output.go`

Wraps return values with a content type for rich WebUI rendering:
```javascript
return output.html("<h1>Report</h1>...")      // rendered iframe
return output.text("Done: 42 items")           // monospace pre block
return output.image("image/png", base64)       // img tag
return output.file("r.csv", csv, "text/csv")   // download button
return output.html(html, { data: { count } })  // html for humans, data for chains
```

Returns a plain Go struct `{ ContentType, Content, Data }`. The runner stores it in sqlite alongside the run. The WebUI reads `ContentType` to decide how to render.

### `pkg/runtime/js/globals/fs.go`

Filesystem access. Only injected when `task.yaml` declares `fs:`. Security enforced in Go before every call:
1. `filepath.Abs` → resolve to absolute path
2. `filepath.EvalSymlinks` → resolve symlinks
3. Check resolved path has a declared entry as prefix
4. Check operation matches declared permission (`r`/`w`/`rw`)

Methods: `read`, `readJSON`, `write`, `writeJSON`, `append`, `list`, `glob`, `stat`, `exists`, `mkdir`, `copy`, `move`, `delete`

### `pkg/runtime/js/globals/log.go`
```javascript
log.info("message")
log.warn("message")
log.error("message")
log.debug("message")
```
Captures to run log in sqlite, also forwards to zap logger.

### `pkg/runtime/js/globals/http.go`
```javascript
const res = await http.get("https://...", { headers: {}, timeout: "30s" })
const res = await http.post("https://...", { body: {}, headers: {} })
// res: { status, headers, body (parsed JSON or string) }
```
Standard `net/http` client. No filesystem or shell access.

### `pkg/runtime/js/globals/kv.go`
```javascript
await kv.set("key", value)
const val = await kv.get("key")
await kv.delete("key")
const keys = await kv.list("prefix")
```
Backed by sqlite `kv` table. Namespaced per task ID.

### `pkg/runtime/js/globals/env.go`
```javascript
const token = env.get("SLACK_TOKEN")  // resolved from secrets chain
```

### `pkg/runtime/js/globals/params.go`
```javascript
const channel = params.get("slack_channel")  // from task.yaml params + run-time overrides
```

**Deliverable**: `dicode task run <id>` executes a task and logs output.

---

## Milestone 5 — Trigger engine

**Goal**: tasks fire automatically on schedule or webhook.

### `pkg/trigger/engine.go`

```go
type Engine struct {
    registry *registry.Registry
    runtime  *js.Runtime
    cron     *cron.Cron
}

func New(registry *Registry, runtime *js.Runtime) *Engine
func (e *Engine) Start(ctx context.Context) error
func (e *Engine) FireManual(ctx context.Context, taskID string, params map[string]string) (string, error)
func (e *Engine) WebhookHandler() http.Handler
```

### `pkg/trigger/cron.go`
- On `Start()`, iterate all registry tasks with `trigger.cron` set
- Register each with `robfig/cron`
- On registry change (Added/Updated/Removed), update cron registrations

### `pkg/trigger/webhook.go`
- HTTP handler at `/hooks/{path}`
- Looks up task by webhook path, fires it with POST body as input

### `pkg/trigger/chain.go`
- After each run completes, check registry for tasks with `trigger.chain.from == completedTaskID`
- If `chain.on` condition matches run status, fire chained task with run output as `input`

**Deliverable**: cron tasks fire on schedule, webhooks trigger tasks, chains propagate.

---

## Milestone 6 — Web UI & REST API

**Goal**: visible interface. Running tasks, viewing logs.

### `pkg/webui/server.go`

```go
type Server struct {
    registry *registry.Registry
    trigger  *trigger.Engine
    db       db.DB
}

func New(cfg config.ServerConfig, ...) *Server
func (s *Server) Handler() http.Handler   // chi router, all routes mounted
func (s *Server) Shutdown(ctx context.Context) error
```

### REST API endpoints (MVP)

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/tasks` | List all tasks |
| `GET` | `/api/tasks/{id}` | Get task detail |
| `POST` | `/api/tasks/{id}/run` | Manual trigger |
| `GET` | `/api/tasks/{id}/runs` | Run history |
| `GET` | `/api/runs/{runID}` | Run detail |
| `GET` | `/api/runs/{runID}/logs` | Run logs (polling) |
| `GET` | `/api/runs/{runID}/logs/stream` | Run logs (SSE) |
| `POST` | `/hooks/{path}` | Webhook trigger |

### UI pages (HTMX, minimal)
- `/` — task list with status badges
- `/tasks/{id}` — task detail + run history + manual trigger button
- `/runs/{runID}` — live log viewer (SSE)
- `/generate` — AI generation prompt box (post-MVP, wired up in Milestone 9)

All HTML templates embedded via `//go:embed templates/`.

**Deliverable**: working browser UI. Tasks visible, runnable, logs viewable.

---

## Milestone 7 — Wire `run()` in `main.go`

**Goal**: dicode starts up and everything connects.

Replace the `run()` stub:
```go
func run(ctx context.Context, cfg *config.Config, log *zap.Logger) error {
    database, err := db.Open(cfg.Database)
    registry := registry.New(database)
    secrets := buildSecretsChain(cfg.Secrets)
    runtime := js.New(registry, secrets, database)
    sources := buildSources(cfg.Sources)
    reconciler := registry.NewReconciler(registry, sources)
    engine := trigger.New(registry, runtime)
    webui := webui.New(cfg.Server, registry, engine, database)

    g, ctx := errgroup.WithContext(ctx)
    g.Go(func() error { return reconciler.Run(ctx) })
    g.Go(func() error { return engine.Start(ctx) })
    g.Go(func() error { return webui.Start(ctx) })
    return g.Wait()
}
```

**Deliverable**: `dicode` binary is fully functional in local-only mode. Add a local source, write a task, see it in the UI, run it, see logs.

---

## Milestone 8 — Git source

**Goal**: tasks from a git repository.

### `pkg/source/git/git.go`
Implement `Source` interface using `go-git`.

```go
type GitSource struct {
    id     string
    cfg    config.SourceConfig
    repo   *git.Repository
}

func New(cfg config.SourceConfig) (*GitSource, error)
func (s *GitSource) ID() string
func (s *GitSource) Start(ctx context.Context) (<-chan source.Event, error)
func (s *GitSource) Sync(ctx context.Context) error
```

Logic:
1. On first `Start()`: clone repo to `~/.dicode/repos/{source-id}/`
2. Poll every `poll_interval`: `git fetch` + `git diff` to detect changed task folders
3. Webhook handler: HTTP endpoint that triggers immediate `Sync()` on push
4. Auth: token in Authorization header (for HTTPS) or SSH key (future)

**Deliverable**: tasks load from a GitHub/GitLab repo. Changes reconcile on push or poll.

---

## Milestone 9 — Testing harness

**Goal**: `dicode task test` works.

### `pkg/testing/harness.go`

Mock globals injected in place of real ones:
```go
type MockHTTP struct { ... }   // http.mock(), intercepts calls
type MockKV  struct { ... }    // in-memory map
type MockEnv struct { ... }    // env.set()
type MockLog struct { ... }    // captures output

func RunTests(spec *task.Spec) (*TestResult, error)
```

`runTask()` in test context: evaluates `task.js` inside a `test()` block's goja runtime with mock globals injected.

**Deliverable**: `dicode task test <id>` runs `task.test.js` with mocked globals, reports pass/fail per case.

---

## Milestone 10 — Secrets sqlite backend

**Goal**: `dicode secrets` CLI commands work with persistent encrypted storage.

This unblocks `LocalProvider.Set/Get/Delete/List` (already written, just missing the sqlite backing).

- Implement `localDB` interface in `pkg/secrets/local.go` using `pkg/db/sqlite.go`
- Add `secrets` subcommand to `main.go`:
  ```bash
  dicode secrets set SLACK_TOKEN xoxb-...
  dicode secrets get SLACK_TOKEN
  dicode secrets list
  dicode secrets delete SLACK_TOKEN
  ```

**Deliverable**: secrets stored encrypted in sqlite, resolved at task runtime.

---

## Milestone 11 — Notifications

**Goal**: task failure/success push notifications.

### `pkg/notify/gotify.go`
Gotify HTTP implementation (similar to ntfy).

### `pkg/notify/desktop.go`
OS desktop notifications:
- Linux: `libnotify` via `beeep` or direct D-Bus call
- macOS: `osascript` or `NSUserNotification` via CGo (optional, can skip for headless)
- Windows: Toast notification via `go-toast`

Wire notifier into JS runtime's `notify` global and trigger engine (on-failure/on-success).

**Deliverable**: task failures send push notifications to mobile and/or desktop.

---

## Milestone 12 — System tray

**Goal**: desktop tray icon with status and quick actions.

### `pkg/tray/tray.go`
Using `github.com/getlantern/systray`:
- Green/yellow/red status icon
- Right-click menu: Open WebUI, Run task submenu, Pause/Resume, Quit
- Left-click: open browser to `http://localhost:{port}`

Only compiled when `server.tray: true` and platform supports it.

**Deliverable**: dicode has a tray icon on desktop systems.

---

## Milestone 13 — MCP server

**Goal**: AI agents can develop tasks via MCP tools.

### `pkg/mcp/server.go` — implement all tools

Wire existing components into MCP handlers:
- `list_tasks` → `registry.All()`
- `get_task` → read task files from disk
- `validate_task` → `task.LoadDir()` + `js.Compile()`
- `test_task` → `testing.RunTests()`
- `dry_run_task` → `runtime.Run()` with HTTP interception
- `run_task` → `engine.FireManual()`
- `get_run_log` → `registry.GetRun()` + logs
- `commit_task` → `git.CommitAndPush()`
- `write_task_file` → write to local source dir
- `list_secrets` → `secrets.List()` (names only)
- `get_js_api` → return JS globals reference markdown
- `get_example_tasks` → return embedded example tasks

**Deliverable**: Claude Code (or any MCP agent) can develop dicode tasks end-to-end.

---

## Milestone 14 — AI generation

**Goal**: describe a task in natural language, dicode generates and deploys it.

### `pkg/ai/generator.go`

```go
type Generator struct {
    client *anthropic.Client
    model  string
}

func (g *Generator) Generate(ctx context.Context, prompt string, existing []task.Spec) (*GeneratedTask, error)
```

Flow:
1. Build prompt: system context (JS globals reference, example tasks, existing task IDs) + user prompt
2. Call Claude API (claude-sonnet-4-6)
3. Extract `task.yaml` + `task.js` + `task.test.js` from response
4. Validate (Layer 1) — if invalid, retry with error feedback (max 3 attempts)
5. Return `GeneratedTask` for UI diff display

Wire into WebUI `/generate` endpoint. Show diff, let user confirm, write to local source.

**Deliverable**: WebUI AI generation prompt box works end-to-end.

---

## Milestone 15 — Webhook relay

**Goal**: public webhook URLs for laptop users.

### `pkg/relay/relay.go` — implement WebSocket tunnel

```go
func (c *Client) Start(ctx context.Context, handler WebhookHandler) error
```

Logic:
1. Connect to `wss://relay.dicode.app` with token in Authorization header
2. Server assigns stable URL `dicode.app/u/{uid}/hooks/{path}`
3. On incoming webhook: relay server sends it over WebSocket
4. Client calls `handler(w, r)` — forwarded to local trigger engine
5. Response streamed back over WebSocket

**Deliverable**: webhooks work on laptops without port forwarding.

---

## Milestone 16 — Service management

**Goal**: `dicode service install` runs dicode on startup.

### Platform implementations of `pkg/service/service.go`

- `service_linux.go` — systemd unit file generator
- `service_darwin.go` — LaunchAgent plist generator
- `service_windows.go` — Windows Service via `golang.org/x/sys/windows/svc`

Add `service` subcommand to `main.go`:
```bash
dicode service install [--headless]
dicode service uninstall
dicode service start / stop / restart
dicode service status
dicode service logs
```

**Deliverable**: `dicode service install` makes dicode survive reboots.

---

## Milestone 17 — Task store

**Goal**: `dicode task install` installs community tasks.

### `pkg/store/store.go`

```go
func Install(source string, targetDir string, params map[string]string) error
```

- Parse `github.com/owner/repo/path` URLs
- Download task folder (tarball or sparse checkout)
- Apply params (substitute `{{ param }}` placeholders in task.yaml)
- Write to local tasks directory
- Reconciler picks it up automatically

Future: index at `dicode.app/store` for discovery.

**Deliverable**: `dicode task install github.com/dicode/tasks/morning-email-check` works.

---

## Milestone 18 — Daemon tasks + `server` global

**Goal**: long-running tasks that serve HTTP. Enables the WebUI-as-task pattern.

### Daemon task lifecycle in the trigger engine

- On reconciler `added`/`updated`: if `trigger.daemon`, start the task immediately
- Track daemon runs separately — they don't appear in normal run history pagination
- On `removed` or `updated`: send a cancellation signal to the running instance
- `restart` policy: `always` → restart after any exit; `on-failure` → restart only on error; `never` → don't restart

### `pkg/runtime/js/globals/server.go`

Two modes:

**`server.mount(path)`** — registers routes on the dicode HTTP server (chi router):
```javascript
const app = server.mount("/")
app.get("/api/tasks", handler)
app.static("/", "./dist")
await app.start()
```

**`server.listen(port)`** — starts a standalone HTTP server in a goroutine, blocks until context cancelled:
```javascript
server.get("/api/v1/data", handler)
await server.listen(9090)
```

### Bootstrap page

When no daemon task has mounted `/`, the dicode binary serves a minimal page pointing to `dicode task install github.com/dicode/webui`. The REST API at `/api/` is always served by the binary.

**Deliverable**: a TypeScript/React WebUI can run as a daemon task, replacing the embedded Go UI.

---

## Milestone 19 — Onboarding wizard

**Goal**: first-run browser wizard instead of config file editing.

### `pkg/onboarding/onboarding.go` — browser wizard

On first run (no config):
1. Start temporary HTTP server on a random port
2. Open browser to wizard page (single HTML page)
3. User chooses local-only or git, fills in details
4. Wizard submits form → handler writes `dicode.yaml` → HTTP server shuts down
5. dicode starts normally with new config

**Deliverable**: smooth first-run experience without editing YAML by hand.

---

## Summary: what each milestone unlocks

| Milestone | What becomes possible |
|---|---|
| 0 | Compile the codebase |
| 1 | Persistent storage |
| 2 | Local task watching |
| 3 | Task registry + reconciliation |
| 4 | Task execution |
| 5 | Automatic triggers (cron, webhook, chain) |
| 6 | Browser UI |
| 7 | **Full local-only mode** — end-to-end working binary |
| 8 | Git-backed tasks |
| 9 | `dicode task test` |
| 10 | `dicode secrets` CLI |
| 11 | Push notifications |
| 12 | System tray |
| 13 | AI agent development via MCP |
| 14 | AI task generation in WebUI |
| 15 | Public webhook URLs on laptops |
| 16 | Run on startup |
| 17 | Community task install |
| 18 | WebUI-as-daemon-task, `server` global, standalone UIs |
| 19 | Smooth first-run onboarding |

Milestones 0–7 are the **MVP**. Everything after is additive.
