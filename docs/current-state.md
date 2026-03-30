# Current State

> Last updated: 2026-03-30 — Security hardening: token rotation, trust_proxy flag, rate-limit lockout extension, CORS origin validation, webhook body fix, API key prefix guard

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
- `ServerConfig` — port, secret, **auth** (global auth wall), **allowed_origins** (CORS allowlist), **trust_proxy** (XFF trust flag), MCP, tray
- `AIConfig`
- `applyDefaults()` with sensible defaults for all fields
- `validate()` checking required fields per source type

### `pkg/task/` ✅

- `spec.go` — `Spec`, `TriggerConfig`, `ChainTrigger`, `Param`, `DockerConfig` structs
- `TriggerConfig` includes `Webhook string`, **`WebhookSecret string`** (HMAC auth), `Daemon bool`, `Restart string`
- `LoadDir(dir)` — reads and validates `task.yaml` from a directory
- `Script()` / `ScriptPath()` — reads task script source (returns `""` for non-JS runtimes)
- `validate()` — schema validation including Docker, daemon restart values, cycle detection stubs
- `hash.go` — `Hash(dir)` SHA256 over task.yaml + task.js
- `ScanDir(tasksDir)` — scans tasks directory, returns map[taskID]hash

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
- **Auth**: protected by `requireAPIKey` middleware when `server.auth: true`. Bearer token format: `dck_<32 random bytes hex>`.
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
- **New tables**: `sessions` (browser sessions + trusted device tokens), `api_keys` (MCP/programmatic keys, hashed)

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
  - **Webhook HMAC**: `verifyWebhookSignature(spec, r, body)` — HMAC-SHA256, `X-Hub-Signature-256` header (GitHub-compatible), optional replay protection via `X-Dicode-Timestamp` (5-minute window). Body capped at 5 MB. Backwards-compatible: open when `webhook_secret` is absent. Raw body bytes read **before** `ParseForm` (replayed via `bytes.NewReader`) so HMAC always covers actual request bytes for form-encoded bodies.
  - **Webhook Task UIs**: `WebhookHandler()` detects tasks with an `index.html` file; on browser GET it serves the page with SDK injection; on POST it either runs the task (JSON/API) or redirects browser form submissions to `/runs/{id}/result`
  - `injectDicodeSDK(html, hookPath, taskID)` — injects `<base href>` + meta tags + `<script src="/dicode.js">` after `<head>` open tag
  - `serveTaskAsset()` — sandboxed static asset serving with extension allowlist and path-traversal guard
  - `flatStringMap()` — converts POST body to `map[string]string` for `RunOptions.Params`
  - Audit logs: run started, run finished, kill requested, trigger types, daemon lifecycle
- 16 tests passing + 7 new HMAC/signature tests

### `pkg/webui/` ✅

- `server.go` — chi router, all REST + SPA endpoints, static assets embedded via `//go:embed static`
  - `New()` now accepts `db.DB` parameter for persistent session and key storage
  - Router restructured: always-public paths (login, static assets, webhooks) separated from the auth-gated group
- **`auth.go`** — `requireAuth` middleware (session cookie check → device token renewal → 401/redirect), `corsMiddleware` (explicit allowlist, Vary header; origins validated with `url.Parse()` at startup — malformed entries skipped), `securityHeaders` (X-Content-Type-Options, X-Frame-Options, Referrer-Policy, Permissions-Policy, **Content-Security-Policy**), `clientIP(r, trustProxy bool)` — XFF only trusted when `server.trust_proxy: true`
- **`sessions_db.go`** — SQLite-backed `dbSessionStore`: `issueDeviceToken`, `renewFromDevice` (wrapped in `db.Tx()`; implements atomic device token rotation after 24h — deletes old row, inserts new, returns new raw token to caller), `listDevices`, `revokeDevice`, `revokeAllDevices`. Device tokens: 30-day expiry, stored as SHA-256 hash, cookie is HttpOnly + SameSite=Strict. HTTP handlers: `apiAuthRefresh`, `apiListDevices`, `apiRevokeDevice`, `apiLogout`, `apiLogoutAll`.
- **`apikeys.go`** — `apiKeyStore`: `generate` (returns raw `dck_`-prefixed key once; prefix truncation bounds-checked), `validate` (hash-compare + `last_used` update), `list`, `revoke`. `requireAPIKey` middleware for MCP. HTTP handlers: `apiListAPIKeys`, `apiCreateAPIKey`, `apiRevokeAPIKey`.
- `apiSecretsUnlock` extended: accepts `trust: true` → issues device cookie alongside session cookie
- REST API endpoints including `POST /api/runs/{runID}/kill`, file editor, trigger editor, AI stream
- **New auth endpoints**: `POST /api/secrets/unlock` (with trust), `POST /api/auth/refresh`, `GET/DELETE /api/auth/devices/{id}`, `POST /api/auth/logout`, `POST /api/auth/logout-all`, `GET/POST/DELETE /api/auth/keys/{id}`
- **Source management** (`sources.go`): `SourceManager` (maps source name → `*taskset.Source`), `GET /api/sources`, `PATCH /api/sources/:name/dev`, `GET /api/sources/:name/branches`
- **MCP server** at `/mcp`: protected by `requireAPIKey` when auth enabled
- WebSocket hub (`/ws`) — real-time fan-out for log lines, run status, task events (`tasks:changed`); ring buffer (recent logs replayed on connect)
- `GET /dicode.js` — standalone webhook task UI SDK (public, no auth)
- Audit logs: run requested via API, kill requested via API
- Task table sorted stably; namespace headers rendered when namespaced IDs present
- Webhook trigger labels rendered as clickable links
- **Frontend** — Lit/LitElement SPA with ESM modules (no build step):
  - `static/app/app.js` — entry point, client-side router, WebSocket boot, auth overlay injection
  - `static/app/lib/` — `ws.js`, `router.js`, `api.js` (401 interceptor: silent refresh → auth overlay → retry), `styles.js`, `utils.js`, `ansi.js`
  - `static/app/components/` — `dc-task-list` (re-fetches on `tasks:changed` WS event), `dc-task-detail`, `dc-run-detail`, `dc-config`, `dc-secrets`, `dc-sources`, `dc-log-bar`, `dc-notif-panel`
  - `components/dc-auth-overlay.js` — modal injected by `app.js`; passphrase + "Trust this browser for 30 days" checkbox
  - `components/dc-security.js` — `/security` page: trusted devices, API key management, logout-all
  - `static/dicode.js` — standalone IIFE SDK for webhook task UIs; `window.dicode` with `run()`, `stream()`, `execute()`, `result()`, `ansiToHtml()`
  - **Security nav link** added to `index.html`
- 11 existing + 16 new auth/security tests (public path gate, 401 enforcement, session lifecycle, device cookie, rate limiting, **extended lockout**, CORS allowlist, **malformed origin skipping**, security headers, CSP, API key generate/validate/revoke, MCP key check, **device token rotation**, **XFF trust flag**)

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
- `buildTaskSetSource()` returns `*taskset.Source` so it can be stored in the source map for runtime dev mode control
- Startup sequence: `CleanupOrphanedContainers` → `CleanupStaleRuns` → register tasks → `engine.Start`
- `db.DB` passed to `webui.New()` for persistent sessions and API key storage

### `examples/` ✅

| Example | Trigger | Runtime |
| --- | --- | --- |
| `hello-cron/` | cron | deno |
| `github-stars/` | manual | deno |
| `gmail-to-slack/` | cron | deno |
| `google-login/` | manual | deno |
| `hello-docker/` | manual | docker |
| `hello-podman/` | manual | podman |
| `hello-python/` | manual | python |
| `nginx-start/` | daemon | docker |
| **`github-push-webhook/`** | **webhook + HMAC auth** | **deno** |

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
| MCP tools: `validate_task`, `test_task`, `dry_run_task`, `commit_task` | Advanced agent workflow tools |
| Multi-user RBAC | `users` table, argon2id passwords, role-based access (north star) |

---

## Configuration files

| File | Status |
|---|---|
| `go.mod` | ✅ All dependencies declared and resolved |
| `dicode.yaml` | ✅ Example config with all sections and comments |
| `BUSINESSPLAN.md` | ✅ Full business model documentation |
| `README.md` | ✅ Comprehensive user documentation |
| `docs/` | ✅ This documentation tree |
| `docs/security-plan.md` | ✅ Security design document (phases 1–4 implemented + hardened) |
| `docs/concepts/security.md` | ✅ Security developer reference (implementation details, DB schema, config reference) |
| `pkg/agent/skill.md` | ✅ Agent skill document |

---

## Test coverage

94+ tests across: db, secrets, source/local, registry, runtime/js, trigger (including HMAC), taskset, and webui (including auth) packages.

All packages compile with `go test -race ./...` as of 2026-03-30.
