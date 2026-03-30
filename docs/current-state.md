# Current State

> Last updated: 2026-03-29 — TaskSet architecture, MCP server, Sources web UI, Webhook Task UIs

This document describes exactly what exists in the codebase today — what is fully implemented, what is stubbed with interfaces and TODOs, and what exists only as documentation.

---

## Status legend

| Symbol | Meaning |
|---|---|
| ✅ | Fully implemented |
| 🟡 | Stubbed — interface/struct defined, logic not yet written |
| 📄 | Documented only — no code yet |
| 🔧 | Partially implemented |

---

## What is built

### `pkg/config/` ✅

Full configuration loading. All structs defined and validated:

- `Config`, `SourceConfig`, `DatabaseConfig`, `RelayConfig`
- `SecretsConfig`, `SecretProviderConfig`
- `NotificationsConfig`, `NotifyProviderConfig`
- `ServerConfig` (port, secret, MCP, tray)
- `AIConfig`
- `applyDefaults()` with sensible defaults for all fields
- `validate()` checking required fields per source type

### `pkg/task/` ✅

- `spec.go` — `Spec`, `TriggerConfig`, `ChainTrigger`, `Param`, `DockerConfig` structs
- `LoadDir(dir)` — reads and validates `task.yaml` from a directory
- `Script()` / `ScriptPath()` — reads task script source (returns `""` for non-JS runtimes)
- `validate()` — schema validation including Docker (requires `docker.image`), daemon restart values, cycle detection stubs
- `hash.go` — `Hash(dir)` SHA256 over task.yaml + task.js
- `ScanDir(tasksDir)` — scans tasks directory, returns map[taskID]hash
- `RuntimeDocker = "docker"` constant; `DockerConfig` (image, command, entrypoint, volumes, ports, working_dir, env_vars, pull_policy)
- `TriggerConfig` includes `Daemon bool` and `Restart string`

### `pkg/source/` ✅

- `source.go` — `Source` interface (`ID()`, `Start()`, `Sync()`), `Event` type, `EventKind` constants
- `source/local/` — fsnotify watcher with 150ms debounce, recursive subdir watching, snapshot-based diff. 6 tests passing.
- `source/git/` — go-git poll, `ListBranches()`, HTTP token auth, deterministic clone path.

### `pkg/secrets/` ✅

- `provider.go` — `Provider` interface, `Chain`, `ResolveAll()`, `NotFoundError`
- `env.go` — `EnvProvider` (reads host env vars)
- `local.go` — `LocalProvider` — ChaCha20-Poly1305 + Argon2id, master key management
- `localdb.go` — `SQLiteSecretDB` — sqlite-backed Set/Get/Delete/List

### `pkg/notify/` 🔧

- `notify.go` — `Notifier` interface, `Message`, `Priority`, `Action`, `NoopNotifier` ✅
- `ntfy.go` — `NtfyNotifier` full HTTP implementation ✅
- `gotify.go` — **not yet created**
- `desktop.go` — **not yet created** (OS desktop notifications)

### `pkg/taskset/` ✅

Full TaskSet architecture — hierarchical task composition inspired by ArgoCD App-of-Apps.

- `spec.go` — `TaskSetSpec`, `TaskSetEntry`, `Ref`, `Defaults`, `TaskOverrides` structs. `kind` field required on all yaml files (Task, TaskSet, Config). `Ref` encodes local vs git: `url` present = git ref, `path` only = local ref.
- `loader.go` — `LoadTaskSet(path)`, `LoadConfig(path)` — loads and validates `kind:TaskSet` and `kind:Config` yaml files
- `resolver.go` — `Resolver` struct (per `(url, branch)` repo dedup), `Resolve(ctx, namespace, rootRef, configDefaults, parentOverrides) []*ResolvedTask`. Implements 6-level precedence stack: task.yaml base → kind:Config defaults → spec.defaults → parent overrides.defaults → parent overrides.entries[key] → entry overrides (leaf wins). `SetDevMode(bool)` / `DevMode() bool`.
- `source.go` — `Source` implementing `source.Source`: polls by re-resolving the full task tree and diffing against snapshot. `SetDevMode(ctx, enabled, localPath)` — swaps the root ref to a local path and triggers immediate re-sync. `DevMode() bool`, `DevRootPath() string`.
- 11 tests passing (resolver override ordering, nested overrides, source event emission).

**Namespace-scoped task IDs**: tasks from a TaskSet source use `/`-separated IDs: `infra/backend/deploy`. Namespaces map to parent TaskSet names.

### `pkg/mcp/` ✅

- `server.go` — JSON-RPC 2.0 MCP server at `POST /mcp`. Protocol version `2024-11-05`. `GET /mcp` returns server info.
- `SourceLister` interface (`List() []SourceEntry`, `SetDevMode(...)`) — avoids import cycle with webui.
- `SourceEntry` struct: Name, Type, URL, Path, Branch, DevMode, DevPath.
- **Implemented tools**: `list_tasks`, `get_task`, `run_task`, `list_sources`, `switch_dev_mode`.
- `New(registry, sourceLister)` constructor.

### `pkg/agent/` ✅

- `skill.go` — `//go:embed skill.md` + exported `Skill` string
- `skill.md` — complete agent skill document (workflow, rules, globals reference, test format, common mistakes)

### `pkg/relay/` 🟡

- `relay.go` — `Client` struct, `Start()`, `WebhookURL()`, `WebhookHandler` type
- WebSocket tunnel logic — **not yet implemented**

### `pkg/service/` 🟡

- `service.go` — `Manager` interface (Install, Uninstall, Start, Stop, Restart, Status, Logs)
- Platform-specific implementations — **not yet created**

### `pkg/db/` ✅

- `db.go` — `DB` interface, `Scanner`, `Config`, `Open()` dispatcher
- `sqlite.go` — WAL mode, full schema migration, Tx with rollback

### `pkg/registry/` ✅

- `registry.go` — Register/Unregister/Get/All (sorted by ID), StartRun/StartRunWithID/FinishRun/AppendLog/GetRun/ListRuns/GetRunLogs
- `CleanupStaleRuns(ctx)` — marks orphaned `running` rows as `cancelled` on startup, returns affected task IDs
- `reconciler.go` — fan-in multi-source, OnRegister/OnUnregister callbacks, AddSource/RemoveSource for live hot-add. 13 tests passing.

### `pkg/runtime/js/` ✅

- `runtime.go` — goja + goja_nodejs event loop, context timeout, run record lifecycle
- All MVP globals: `log`, `env`, `params`, `http`, `kv`, `output`, `fs`
- RunOptions carries `RunID` (pre-generated by engine), `Params`, `Input`, `ParentRunID`
- `FinishRun` uses `context.Background()` in defer — succeeds even if run context cancelled
- `ctx.Err() != nil` → `StatusCancelled` (not failure) on kill
- 14 tests passing

### `pkg/runtime/docker/` ✅

- `docker.go` — runs tasks as Docker containers with live log streaming
  - `Run(ctx, spec, opts)` blocks until container exits or ctx cancelled
  - Labels every container `dicode.run-id` / `dicode.task-id` for orphan detection
  - `ContainerLogs` uses `context.Background()` + explicit `sync.Once` close — prevents kill from blocking stdcopy
  - Kill watcher goroutine: `<-ctx.Done()` → `closeLog()` → `ContainerStop` (10s SIGTERM timeout)
  - Port bindings via `nat.PortMap`; `pull_policy: always | missing | never`
  - Audit logs: container created, container started, container finished (with exit code)
- `cleanup.go` — `CleanupOrphanedContainers(ctx, log)` — finds all containers with `label=dicode.run-id`, stops running ones, removes all. Called at startup.

### `pkg/trigger/` ✅

- `engine.go` — cron (robfig/cron), webhook, manual `FireManual()`, chain `FireChain()`, daemon lifecycle
  - `fireAsync(ctx, spec, opts, source)` — pre-generates runID, starts goroutine, returns immediately
  - `dispatch(ctx, spec, opts) string` — routes to JS or Docker runtime, returns final status string
  - `KillRun(runID)` — cancels run via `runCancels sync.Map`
  - Daemon: `startDaemon`, `onDaemonRunFinished` with restart policy (always/on-failure/never)
  - Shutdown: kills all active daemon runs via `shutdownCtx`
  - Audit logs: run started (task, run, trigger source, runtime), run finished (status, duration), kill requested, manual trigger, chain trigger (from/to/on), webhook trigger (path/task), task registered/unregistered, daemon restart lifecycle events
  - **Webhook Task UIs**: `WebhookHandler()` detects tasks with an `index.html` file; on browser GET it serves the page with SDK injection; on POST it either runs the task (JSON/API) or redirects browser form submissions to `/runs/{id}/result`
  - `injectDicodeSDK(html, hookPath, taskID)` — injects `<base href>` + meta tags + `<script src="/dicode.js">` after `<head>` open tag
  - `serveTaskAsset()` — sandboxed static asset serving with extension allowlist (`.html/.css/.js/.json/.svg/.png/.jpg/.ico/.woff/.woff2`) and path-traversal guard
  - `flatStringMap()` — converts JSON POST body `map[string]interface{}` to `map[string]string` for `RunOptions.Params`
- 16 tests passing

### `pkg/webui/` ✅

- `server.go` — chi router, all REST + SPA endpoints, static assets embedded via `//go:embed static`
- REST API endpoints including `POST /api/runs/{runID}/kill`, file editor, trigger editor, AI stream
- **Source management** (`sources.go`): `SourceManager` (maps source name → `*taskset.Source`), HTTP handlers: `GET /api/sources`, `PATCH /api/sources/:name/dev`, `GET /api/sources/:name/branches`
- **MCP server mounted** at `/mcp`: `mcp.New(registry, sourceMgr)` wired and served via `r.Mount("/mcp", ...)`
- WebSocket hub (`/ws`) — real-time fan-out for log lines, run status changes, task events; ring buffer (recent logs replayed on connect)
- Session store, secrets manager UI (unlock/lock with passphrase), AI chat, config editor
- SPA routing: `/app/*` serves static assets; `/*` catch-all serves `index.html`
- `GET /dicode.js` — serves the standalone webhook task UI SDK (embedded from `static/dicode.js`)
- Audit logs: run requested via API, kill requested via API
- Task table sorted stably (registry.All() sorts by ID); namespace headers rendered when namespaced IDs present
- Webhook trigger labels rendered as clickable links (using `t.trigger?.Webhook` — capital W matches Go's JSON marshalling of untagged struct fields)
- **Frontend** — Lit/LitElement SPA with ESM modules (no build step):
  - `static/app/app.js` — entry point, client-side router, WebSocket boot
  - `static/app/lib/` — `ws.js` (WebSocket client + auto-reconnect), `router.js`, `api.js` (get/post/patch), `styles.js`, `utils.js`, `ansi.js` (ansi-to-html wrapper, Catppuccin Mocha palette)
  - `static/app/components/` — `dc-task-list` (namespace-grouped, webhook links), `dc-task-detail`, `dc-run-detail` (ANSI log rendering via `unsafeHTML(ansiToHtml(...))`), `dc-config`, `dc-secrets`, `dc-sources` (dev mode toggle, branch picker), `dc-log-bar`, `dc-notif-panel`
  - `static/dicode.js` — standalone IIFE SDK for webhook task UIs; exposes `window.dicode` with `run()`, `stream()`, `execute()`, `result()`, `ansiToHtml()`; auto-enhances `<form data-dicode>` elements
- 11 tests passing

### `pkg/tray/` ✅

- `tray.go` — fyne.io/systray, Open Dashboard / Quit menu items
- `icon.go` — generated 32×32 purple icon at init time (no binary asset)
- Controlled by `server.tray` in config

### `pkg/onboarding/` 🔧

- `onboarding.go` — `Required()`, `DefaultLocalConfig()` (with Docker examples), `WriteConfig()` ✅
- Browser wizard (HTTP server + HTML page) — **not yet implemented**

### `cmd/dicode/main.go` ✅

- Full component wiring: db → secrets → registry → JS runtime → Docker runtime → trigger engine → reconciler → webui → tray
- `buildSources()` returns `([]source.Source, *webui.SourceManager, error)` — builds both the source slice and the `SourceManager` for dev mode control
- `buildTaskSetSource()` returns `*taskset.Source` (not `source.Source`) so it can be stored in the source map for runtime dev mode control
- Startup sequence: `CleanupOrphanedContainers` → `CleanupStaleRuns` → register tasks → `engine.Start`
- `task` + `version` subcommands; secrets CLI subcommand missing

---

## What is not yet created

| Package | What it will contain |
|---|---|
| `pkg/testing/` | Task test harness (mock globals, assert, runTask) |
| `pkg/store/` | Task store installer (`dicode task install`) |
| `pkg/notify/desktop.go` | OS desktop notifications |
| `pkg/notify/gotify.go` | Gotify push notification provider |
| `pkg/db/postgres.go` | PostgreSQL implementation |
| `pkg/db/mysql.go` | MySQL implementation |
| `pkg/runtime/js/globals/server.go` | `server` global (daemon tasks serving HTTP) |
| MCP tools: `validate_task`, `test_task`, `dry_run_task`, `commit_task` | Advanced agent workflow tools (list_tasks/get_task/run_task/list_sources/switch_dev_mode are implemented) |

---

## Configuration files

| File | Status |
|---|---|
| `go.mod` | ✅ All dependencies declared and resolved |
| `dicode.yaml` | ✅ Example config with all sections and comments |
| `BUSINESSPLAN.md` | ✅ Full business model documentation |
| `README.md` | ✅ Comprehensive user documentation |
| `docs/` | ✅ This documentation tree |
| `pkg/agent/skill.md` | ✅ Agent skill document |
| `~/dicode-tasks/nginx-start/task.yaml` | ✅ Example Docker daemon task |

---

## Test coverage

70+ tests across: db, secrets, source/local, registry, runtime/js, trigger, taskset, and webui packages.

All packages compile and all tests pass as of 2026-03-29.
