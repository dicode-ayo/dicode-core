# Current State

> Last updated: 2026-04-09 — Relay client restored and hardened (`pkg/relay`): ECDSA P-256 identity, transparent HTTP proxy to local daemon, X-Relay-Base header injection, path restriction (`/hooks/*` + `/dicode.js`), hop-by-hop header filtering; `dicode-relay` (Node.js) is the production relay server; Go `relay.Server` kept for self-hosting and integration tests; config variables (`${HOME}`, `${DATADIR}`, `${CONFIGDIR}`) in `dicode.yaml`

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
- `AIConfig` — `BaseURL`, `Model`, `APIKey`, `APIKeyEnv`
- **`DefaultsConfig`** — `OnFailureChain string` — global failure handler task ID
- `applyDefaults()` with sensible defaults for all fields
- `validate()` checking required fields per source type

### `pkg/task/` ✅

- `spec.go` — `Spec`, `TriggerConfig`, `ChainTrigger`, `Param`, `DockerConfig` structs
- `TriggerConfig` includes `Webhook string`, **`WebhookSecret string`** (HMAC auth), `Daemon bool`, `Restart string`
- **`SecurityConfig`** — `AllowedTasks []string` + `AllowedMCP []string`; attached as `Security *SecurityConfig` on `Spec`
- **`MCPPort int`** — declares the port an MCP daemon task listens on
- **`OnFailureChain *string`** — per-task override (`nil` = inherit global default, `""` = disable, `"task-id"` = override)
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

- `provider.go` — `Provider` interface, `Chain`, `ResolveAll()`, `NotFoundError`; **`Manager` interface** (`List`, `Set`, `Delete`) — satisfied by `*LocalProvider`, used by `ControlServer` and `pkg/webui` (`SecretsManager = secrets.Manager` type alias)
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
- **`pkg/mcp/client/`** — lightweight HTTP JSON-RPC 2.0 MCP client: `New(port int)`, `ListTools(ctx)`, `Call(ctx, tool, args)`. Used by the socket server to proxy `mcp.list_tools` / `mcp.call` requests from task scripts to daemon MCP tasks.

### `pkg/agent/` ✅

- `skill.go` — `//go:embed skill.md` + exported `Skill` string
- `skill.md` — complete agent skill document (workflow, rules, globals reference, test format, common mistakes)

### `pkg/relay/` ✅

WebSocket relay client and self-hosting server for receiving webhooks behind NAT. Restored and hardened after the initial PR #79, then refactored to act as a transparent HTTP proxy.

- `client.go` — `Client` struct: persistent WSS connection to relay server with exponential backoff reconnection (1 s → 60 s, ±20% jitter). Receives `request` messages, makes real HTTP requests to the local daemon at `http://localhost:<port>`, and sends `response` messages back. Security: path whitelist (`/hooks/*` and `/dicode.js` only), `X-Relay-Base` header injection (set from client's UUID, incoming values stripped), hop-by-hop + `Set-Cookie` header filtering, 5 MB body limit, 25 s local request timeout. `HookBaseURL()` exposes the relay URL for SDK injection.
- `server.go` — `Server` struct: simple relay server for self-hosting and integration tests. ECDSA P-256 challenge-response handshake, in-memory nonce store (60 s TTL), client registry, 30 s per-request forwarding timeout, 5 MB body limit. Implements `http.Handler` for embedding in tests.
- `protocol.go` — JSON message types: `challenge`, `hello`, `welcome`, `error`, `request`, `response`. All messages are text WebSocket frames.
- `keys.go` — `Identity` struct: P-256 keypair generation, `LoadOrGenerateIdentity(ctx, db)` persists private key in SQLite `kv` table (key: `relay.private_key`). UUID derived as `hex(sha256(uncompressed_pubkey))` — stable across restarts.
- Tests: `client_test.go` (16 tests), `keys_test.go`, `helpers_test.go`

**Wiring**: `cmd/dicoded/main.go` starts the relay client when `relay.enabled: true` and `relay.server_url` is set. The `webui.Server` receives the relay client via `SetRelayClient()` to expose the hook base URL to the frontend.

**Production relay server**: The production relay server is a separate TypeScript/Node.js service in the `dicode-relay` repository. It implements the same protocol with additional features: OAuth broker (Grant + Express), ECIES token encryption, status dashboard, and support for 14 OAuth providers. The Go `relay.Server` is kept for self-hosting and testing scenarios.

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
  - **`WaitRun(ctx, runID) (RunResult, error)`** — polls `registry.GetRun` every 500ms until terminal status; used by `EngineRunner` for `dicode.run_task` blocking calls from within task scripts
  - **`SetDefaultsOnFailureChain(id string)`** — sets the config-level global failure handler; called from `cmd/dicode/main.go` when `defaults.on_failure_chain` is set
  - **`on_failure_chain` logic** — after each run, if the run failed, the spec's `OnFailureChain` (or the global default) is invoked with `input: { taskID, runID, status, output }`; per-task `on_failure_chain: ""` disables the global default
  - Daemon: `startDaemon`, `onDaemonRunFinished` with restart policy (always/on-failure/never)
  - Shutdown: kills all active daemon runs via `shutdownCtx`
  - **Webhook HMAC**: `verifyWebhookSignature(spec, r, body)` — HMAC-SHA256, `X-Hub-Signature-256` header (GitHub-compatible), optional replay protection via `X-Dicode-Timestamp` (5-minute window). Body capped at 5 MB. Backwards-compatible: open when `webhook_secret` is absent. Raw body bytes read **before** `ParseForm` (replayed via `bytes.NewReader`) so HMAC always covers actual request bytes for form-encoded bodies.
  - **Webhook Task UIs**: `WebhookHandler()` detects tasks with an `index.html` file; on browser GET it serves the page with SDK injection; on POST it either runs the task (JSON/API) or redirects browser form submissions to `/runs/{id}/result`
  - `injectDicodeSDK(html, hookPath, taskID)` — injects `<base href>` + meta tags + `<script src="/dicode.js">` after `<head>` open tag; reads `X-Relay-Base` header to rewrite `<base href>` and SDK paths for relay-served tasks
  - **SPA fallback** — extensionless sub-paths under a webhook hook path (e.g. `/hooks/webui/tasks/foo`) serve `index.html` from the task directory, enabling client-side routing for any webhook task that ships an `index.html`. Path-traversal guard runs before extension check so `..` segments are rejected with 403 rather than silently served as the SPA shell.
  - `serveTaskAsset()` — sandboxed static asset serving with extension allowlist and path-traversal guard
  - `flatStringMap()` — converts POST body to `map[string]string` for `RunOptions.Params`
  - Audit logs: run started, run finished, kill requested, trigger types, daemon lifecycle
- Implements `denoserver.EngineRunner` interface: `FireManual()` + `WaitRun()` — allows Deno/Python task scripts to trigger and await other tasks via `dicode.run_task()`
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
- **Frontend (migrated)** — The dashboard SPA has been moved to `examples/webui/` and is served as a standalone webhook task at `/hooks/webui`. The Go binary no longer embeds the frontend assets. The server catch-all redirects `GET /*` to `/hooks/webui`. See `examples/webui/` below.
  - `static/dicode.js` still embedded — standalone IIFE SDK injected into any webhook task UI; `window.dicode` with `run()`, `stream()`, `execute()`, `result()`, `ansiToHtml()`
- `GET /runs/{runID}/result` — serves `OutputContent` with its MIME type, or `ReturnValue` as `application/json` when no structured output type is set
- 11 existing + 16 new auth/security tests (public path gate, 401 enforcement, session lifecycle, device cookie, rate limiting, **extended lockout**, CORS allowlist, **malformed origin skipping**, security headers, CSP, API key generate/validate/revoke, MCP key check, **device token rotation**, **XFF trust flag**)

### `pkg/tray/` ✅

- `tray.go` — fyne.io/systray, Open Dashboard / Quit menu items
- `icon.go` — generated 32×32 purple icon at init time (no binary asset)
- Controlled by `server.tray` in config

### `pkg/onboarding/` 🔧

- `onboarding.go` — `Required()`, `DefaultLocalConfig()` (with Docker examples), `WriteConfig()` ✅
- Browser wizard (HTTP server + HTML page) — **not yet implemented**

### `pkg/ipc/` ✅

Unified IPC protocol replacing the old per-runtime `pkg/runtime/deno/server/`. Two socket types:

**Task shim sockets** (per-run, temporary):

- **Wire format**: 4-byte little-endian length prefix + JSON payload
- **Handshake**: client sends `{"token":"<DICODE_TOKEN>"}`, server validates HMAC-signed token and replies `{"proto":1,"caps":[...]}`
- **Capability tokens**: HMAC-SHA256 signed, scoped to a specific run ID. Task shims get `log`, `params.read`, `input.read`, `kv.read`, `kv.write`, `output.write`, `return`, `tasks.list`, `runs.list`, `config.read`; additionally `tasks.trigger` / `mcp.call` based on security config; `http.register` for daemon tasks
- `server.go` — `Server` struct; `Start()` returns `(socketPath, token, error)`. Dispatcher enforces capability checks before every handler
- `gateway.go` — `Gateway` HTTP dispatch layer: priority-ordered pattern routing (longest-prefix wins). Two handler types: Go handlers (webhook tasks) and IPC handlers (daemon tasks via `http.register`). `ipcHandler` bridges HTTP requests to open IPC connections via `HTTPInboundRequest` push + `http.respond` reply
- `token.go` — `IssueToken` / `IssueTokenWithTTL` / `VerifyToken` (HMAC-SHA256); `NewSecret()`
- `conn.go` — `readMsg` / `writeMsg` (length-prefix framing) with outbound size guard (8 MiB)
- `capability.go` — capability constants; `defaultTaskCaps()`; **`CapCLI*` constants** + `cliCaps()` for control socket clients
- `message.go` — `Request`, `Response`, `OutputResult`, `EngineRunner`, `RunResult`, `HTTPInboundRequest`, **`TaskSummary`**, **`LogEntry`**, **`DaemonStatus`** types

**Control socket** (persistent, daemon-lifetime):

- `control.go` — **`ControlServer`**: listens at `~/.dicode/daemon.sock` (mode 0600). On startup writes a pre-issued CLI token to `~/.dicode/daemon.token` (mode 0600, atomic write). Handles `cli.ping`, `cli.list`, `cli.run`, `cli.logs`, `cli.status`, `cli.secrets.{list,set,delete}`. Context-aware: per-connection context cancels in-flight `cli.run` on client disconnect. CLI tokens use `tokenCLITTL` (~10 years) — daemon restart re-issues anyway
- `control_client.go` — **`ControlClient`**: `Dial(socketPath, tokenPath)` → `Send(req)` → `Close()`. Handshake decodes a union struct covering both success (`proto`) and error (`error`) envelopes

- `pkg/runtime/deno/server/` **deleted** — both Deno and Python runtimes now import `pkg/ipc`

### `pkg/runtime/deno/sdk/shim.js` ✅

Injected before every Deno task script. Updated for unified IPC protocol:
- Length-prefix framing (`readExact` + 4-byte LE header) replaces newline-delimited reads
- Handshake on connect: reads `DICODE_TOKEN` from env, sends to server, validates response
- All globals unchanged: `log`, `params`, `env`, `kv`, `input`, `output`, **`dicode`** (`run_task`, `list_tasks`, `get_runs`, `get_config`), **`mcp`** (`list_tools`, `call`)

### `pkg/runtime/python/sdk/dicode_sdk.py` ✅

Injected before every Python task script via `buildWrapper()`. Updated for unified IPC protocol:
- asyncio background IO loop (`asyncio.new_event_loop()` + daemon thread)
- `asyncio.open_unix_connection()` for socket; length-prefix framing via `struct.pack("<I", ...)`
- Handshake on connect: sends `DICODE_TOKEN`, validates server response
- `async def main()` detected via `asyncio.iscoroutinefunction` and run with `asyncio.run()`; return value captured as `result`; writer closed gracefully after `_set_return`
- `_call_async(req)` — bridges `_async_call` into the caller's event loop via `asyncio.wrap_future`; no thread pool
- Full `_async` variants on all globals: `log.*_async`, `params.get_async/all_async`, `kv.*_async`, `dicode.*_async` (including `get_config_async`), `mcp.*_async`
- Globals: `log`, `params`, `env`, `kv`, `input`, `output`, **`dicode`**, **`mcp`**

### `cmd/dicoded/main.go` ✅ (daemon binary)

The full daemon process — extracted from the old monolith. Starts all long-running components:

- Full component wiring: db → secrets → registry → Deno runtime → Docker/Podman/Python runtimes → trigger engine → reconciler → HTTP gateway → webui → control socket → tray
- `NewControlServer(socketPath, tokenPath, ...)` — creates the CLI control socket and writes `daemon.token`
- `buildSecretsChain(cfg, dataDir, database, log)` returns `(secrets.Chain, secrets.Manager)` — the `Manager` is passed to both `webui.New()` and `NewControlServer()`
- Startup sequence: `CleanupOrphanedContainers` → `CleanupStaleRuns` → build runtimes → build sources → build webui → build control socket → run errgroup (reconciler + engine + webui + control socket + tray)
- `make run` builds and starts this binary

### `cmd/dicode/main.go` ✅ (CLI binary)

Thin command dispatcher — no runtime or database initialisation:

- Subcommands: `run <task-id> [key=value ...]`, `list`, `logs <run-id>`, `status [task-id]`, `secrets {list,set,delete}`, `version`
- **Auto-start**: if `~/.dicode/daemon.sock` is not connectable, locates `dicoded` (next to `dicode` binary, then `$PATH`), starts it in the background, redirects stderr to `~/.dicode/daemon.log`, polls for the socket (8 second timeout)
- Reads `~/.dicode/daemon.token` and calls `ipc.Dial()` to connect
- `DICODE_DATA_DIR` env var overrides the default data directory

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
| `github-push-webhook/` | webhook + HMAC auth | deno |
| `webui/` | webhook (SPA shell) | deno |
| **`ai/`** | **webhook (auth)** | **deno** — generic OpenAI-compatible agent; uses `dicode.run_task()` as tools in a tool-use loop |
| **`failure-monitor/`** | **on_failure_chain** | **deno** — AI-powered failure diagnosis; receives `{ taskID, runID, status }` via `input` |
| **`task-creator/`** | **webhook (auth)** | **deno** — AI generates `task.yaml` + `task.ts` from a plain-language description |

`examples/webui/` is the full dicode dashboard SPA. It ships as a self-contained webhook task: `index.html` + Lit/LitElement components under `app/`. The engine injects `<base href="/hooks/webui/">` and the dicode SDK on every GET. Auth is enforced client-side by `dc-auth-overlay` (intercepts 401s from the REST API). Any unauthenticated REST call shows the login modal without a page redirect.

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
| OAuth token delivery handler in `pkg/relay/client.go` | ECIES decryption of tokens forwarded by dicode-relay broker (design in `docs/design/oauth-broker.md`) |
| `tasks/auth/_oauth-app/task.ts` broker mode | Open browser to dicode-relay OAuth broker when relay is connected (`config.relay.broker_url`) |

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

## Build

```bash
make build   # compiles both ./dicode (CLI) and ./dicoded (daemon)
make run     # builds and starts dicoded — web UI on :8080, control socket at ~/.dicode/daemon.sock
make test    # go test ./...
make lint    # go fmt + go vet
make clean   # removes both binaries
```

---

## Test coverage

94+ tests across: db, secrets, source/local, registry, runtime/js, trigger (including HMAC), taskset, ipc (including gateway + control socket), webui (including auth), and relay (client, keys) packages.

All packages compile with `go test -race ./...` as of 2026-04-09.
