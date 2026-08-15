# Current State

> Last updated: 2026-07-10 — suspendable tasks v2: JSON-Schema forms + server-side validation (#514), auto-dispatch main/resume/steps (#515), robustness hardening (#519); relay client/server, tray, and the MCP server now live as built-in tasks rather than Go packages

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
- `ServerConfig` — port, secret, **auth** (global auth wall), **allowed_origins** (CORS allowlist), **trust_proxy** (XFF trust flag), MCP, TLS cert/key, bcrypt cost, device binding
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
- `source/git/` — go-git poll, `ListBranches()`, HTTP token auth, deterministic clone path. SSRF guard: `internal/gitops.ValidateRemoteHost()` rejects loopback/private/internal literal hosts (or blocked hostname suffixes) before any dial, for every scheme go-git's endpoint parser understands — `http`, `https`, `ssh`, and the `git@host:path` SCP shorthand. It's invoked from both `ListBranches` **and** `internal/gitops.CloneOrPull` itself (#489), so the actual poll/clone/pull path is covered, not just the branch-preview helper. `internal/gitops.InstallSSRFGuardedTransport()` additionally installs a process-wide go-git HTTP(S) transport whose dialer re-checks every *resolved* connection IP, closing the DNS-rebind gap — but only for `http`/`https` (#481); `ssh://`/SCP-shorthand remotes rely solely on the literal-host check, so a hostname that resolves differently at connect time than at validation time is a known, smaller residual gap for that scheme. `git://` remains rejected outright at config-load time (#486), so it never reaches either layer with a caller-supplied host. **Operator escape hatch (#537):** `source_security.allow_internal_hosts` in `dicode.yaml` names specific internal hosts/CIDRs to exempt — a single `internal/gitops.Allowlist` both layers consult (installed once at daemon start via `gitops.SetInternalHostAllowlist`). The two layers key on different values, so an entry's kind determines its reach: a **hostname** entry authorises the literal-host layer only (enough for `ssh`/SCP-shorthand, which never reach the dial layer); `http`/`https` additionally need the target's **IP/CIDR** listed, because the dial-time layer sees only the *resolved* IP and a hostname entry deliberately never authorises the address it resolves to (keeping DNS-rebind protection intact for an allowlisted name). Empty/absent = today's fully fail-closed behaviour; the rejection message names the config key.

### `pkg/secrets/` ✅

- `provider.go` — `Provider` interface, `Chain`, `ResolveAll()`, `NotFoundError`; **`Manager` interface** (`List`, `Set`, `Delete`) — satisfied by `*LocalProvider`, used by `ControlServer` and `pkg/webui` (`SecretsManager = secrets.Manager` type alias)
- `env.go` — `EnvProvider` (reads host env vars)
- `local.go` — `LocalProvider` — ChaCha20-Poly1305 + Argon2id, master key management
- `localdb.go` — `SQLiteSecretDB` — sqlite-backed Set/Get/Delete/List

### `pkg/taskset/` ✅

Full TaskSet architecture — hierarchical task composition inspired by ArgoCD App-of-Apps.

- `spec.go` — `TaskSetSpec`, `TaskSetEntry`, `Ref`, `Defaults`, `TaskOverrides` structs. `kind` field required on all yaml files (Task, TaskSet, Config). `Ref` encodes local vs git: `url` present = git ref, `path` only = local ref.
- `loader.go` — `LoadTaskSet(path)`, `LoadConfig(path)` — loads and validates `kind:TaskSet` and `kind:Config` yaml files. `ValidateRefURL` (and its bool wrapper `IsAllowedRefScheme`, also used by `pkg/webui`'s add-source form) gates `ref.url` to `http`, `https`, `ssh` schemes (plus the `user@host:path` SSH shorthand); the `git://` scheme is rejected outright (#486) — go-git dials it through a native transport with a hardcoded, unguarded `net.Dial`, giving it zero SSRF host validation at any layer, unlike the allowed schemes (`internal/gitops.ValidateRemoteHost` inspects the literal host and is now invoked from `CloneOrPull` itself, not just `ListBranches` — see #489, closing #486's follow-up for the schemes that are still allowed).
- `resolver.go` — `Resolver` struct (per `(url, branch)` repo dedup), `Resolve(ctx, namespace, rootRef, configDefaults, parentOverrides) []*ResolvedTask`. Implements 6-level precedence stack: task.yaml base → kind:Config defaults → spec.defaults → parent overrides.defaults → parent overrides.entries[key] → entry overrides (leaf wins). `SetDevMode(bool)` / `DevMode() bool`.
- `source.go` — `Source` implementing `source.Source`: polls by re-resolving the full task tree and diffing against snapshot. `SetDevMode(ctx, enabled, localPath)` — swaps the root ref to a local path and triggers immediate re-sync. `DevMode() bool`, `DevRootPath() string`.
- 11 tests passing (resolver override ordering, nested overrides, source event emission).

**Namespace-scoped task IDs**: tasks from a TaskSet source use `/`-separated IDs: `infra/backend/deploy`. Namespaces map to parent TaskSet names.

### `pkg/mcp/` ✅

- The MCP **server** is no longer a Go package — it ships as the `buildin/mcp` task (JSON-RPC 2.0 over HTTP POST). `pkg/webui` serves a session-or-API-key-gated `/mcp` URL that forwards to it.
- **Implemented tools** (`tasks/buildin/mcp/task.ts`): `list_tasks`, `get_task`, `run_task`, `list_sources`, `switch_dev_mode`, `test_task`.
- **Auth**: the `/mcp` forwarder accepts either a session cookie or a Bearer API key (`requireSessionOrAPIKey` in `pkg/webui/server.go`), not API-key-only. Bearer token format: `dck_<32 random bytes hex>`.
- **`pkg/mcp/client/`** — lightweight HTTP JSON-RPC 2.0 MCP client: `New(port int)`, `ListTools(ctx)`, `Call(ctx, tool, args)`. Used by the socket server to proxy `mcp.list_tools` / `mcp.call` requests from task scripts to daemon MCP tasks.

### Relay (built-in tasks, no Go package)

`pkg/relay` was removed — the relay client and server migrated to TypeScript built-in tasks (PR #254):

- `buildin/relay-client` — maintains the WebSocket tunnel to the dicode relay broker so the daemon can receive webhooks without a public port.
- `buildin/relay-server` / `buildin/relay-server-body` — run dicode-relay in-process under the daemon's Deno runtime for self-hosting.

**Production relay server**: the production broker is a separate TypeScript/Node.js service in the `dicode-relay` repository: OAuth broker (Grant + Express), ECIES token encryption, status dashboard, 14+ OAuth providers.

**OAuth broker plumbing**: built-in tasks handle the OAuth dance via the relay broker. `buildin/auth-start` generates a signed `/auth/<provider>` URL; `buildin/auth-relay` receives ECIES-encrypted tokens at `/hooks/oauth-complete`, verifies the broker signature, decrypts, and stores them in the secrets chain. See `docs/oauth.md`.

### `pkg/metrics/` ✅

- `proc.go` (+ `proc_linux.go` / `proc_other.go`) — process metrics for `GET /api/metrics`: `DaemonMetrics` (heap alloc/sys MB, goroutine count, cumulative CPU ms on Linux) and `ChildMetrics` (active task count, aggregate child RSS/CPU on Linux, semaphore slot usage).
- The HTTP handler is `apiMetrics` in `pkg/webui/metrics.go`; active-task concurrency counters come from the trigger engine's semaphore.

### `pkg/service/` 📄

- **Documented only — no code.** The `Manager` interface stub (Install, Uninstall, Start, Stop, Restart, Status, Logs) was removed as dead code (#388): it had no platform implementations and no CLI wiring (`dicode service ...` is not a real subcommand yet; see README.md's OS-service-management note). The design still lives in `docs/implementation-plan.md`.

### `pkg/db/` ✅

- `db.go` — `DB` interface, `Scanner`, `Config`, `Open()` dispatcher
- `sqlite.go` — WAL mode, full schema migration, Tx with rollback
- **New tables**: `sessions` (device tokens + scs session data), `api_keys` (MCP/programmatic keys, hashed)

### `pkg/registry/` ✅

- `registry.go` — Register/Unregister/Get/All (sorted by ID), StartRun/StartRunWithID/FinishRun/AppendLog/GetRun/ListRuns/GetRunLogs
- `CleanupStaleRuns(ctx)` — marks orphaned `running` rows as `cancelled` on startup, returns affected task IDs
- `reconciler.go` — fan-in multi-source, OnRegister/OnUnregister callbacks, AddSource/RemoveSource for live hot-add. 13 tests passing.

### `pkg/runtime/deno/` ✅

JS/TS execution (the former goja-based `pkg/runtime/js` is gone):

- `runtime.go` / `manager.go` — one Deno subprocess per run; sandboxing via `--allow-net`/`--allow-env`/`--allow-read`/`--allow-write` flags derived from the task's declared permissions
- SDK shim embedded from `pkg/runtime/deno/sdk/shim.ts`; SDK calls travel over the `pkg/ipc` unix socket, not raw `fetch`
- `lock.go` — per-task Deno lockfile handling with recovery

### `pkg/runtime/python/` ✅

- `runtime.go` — Python tasks via a `uv`-provisioned interpreter subprocess (`pkg/uv`)
- `guard.go` — in-interpreter enforcement of declared fs/net/run permissions
- SDK embedded from `pkg/runtime/python/sdk/dicode_sdk.py`
- `Execute` populates `RunResult.ReturnValue`, `OutputContentType` and `OutputContent` from the run (#680, matching `pkg/runtime/deno`) — `dicode.run_task` and `output.html(...)`/`output.text(...)`/etc. now work for Python tasks the way `docs/python-runtime.md`'s parity table always claimed.

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
  - `fireAsync(ctx, spec, opts, source)` — pre-generates runID, starts goroutine, returns immediately; **max concurrent tasks** enforced via semaphore (configurable, default 10)
  - `dispatch(ctx, spec, opts) string` — routes to JS or Docker runtime, returns final status string
  - `KillRun(runID)` — cancels run via `runCancels sync.Map`
  - **`WaitRun(ctx, runID) (RunResult, error)`** — channel-based notification (replaced polling loop); used by `EngineRunner` for `dicode.run_task` blocking calls from within task scripts
  - **`SetDefaultsOnFailureChain(id string)`** — sets the config-level global failure handler; called from `cmd/dicode/main.go` when `defaults.on_failure_chain` is set
  - **`on_failure_chain` logic** — after each run, if the run failed, the spec's `OnFailureChain` (or the global default) is invoked with `input: { taskID, runID, status, output }`; per-task `on_failure_chain: ""` disables the global default
  - Daemon: `startDaemon`, `onDaemonRunFinished` with restart policy (always/on-failure/never)
  - Shutdown: kills all active daemon runs via `shutdownCtx`; `runWG` (reserved by `trackRun()`) is drained before `Start` returns, bounded by `drainGrace` (10s), so `fireAsync`/pipeline/daemon runs finish their `FinishRun`/status writes before the daemon closes the DB (#520/#525). `Engine.DrainSlot()` is the exported form of that `trackRun`/`runWG.Done` pair: it reserves a slot, hands back an idempotent release func, and refuses (`ok == false`) once shutdown has latched — the single guard every top-level synchronous fire routes through. `fireWebhookTask` (`webhook.go`) uses it around its `fireSync` call, so a `wait=true` (default) webhook run outlasting `http.Server.Shutdown`'s ~5s cap is drained too, instead of racing DB close (#529). `apiReplayRun` (`pkg/webui/server.go`, `POST /api/runs/{runID}/replay`) uses it around `Replayer.Replay`, whose `InputStore.Fetch` → `inputStoreTaskRunner.RunTaskSync` → `fireSync` runs a storage-task fetch from an HTTP handler with no enclosing tracked slot — the same #529 class, now drained and refused by the same guard (#533). Nested `fireSync` calls made *from within an already-tracked run* (if_missing prereqs, and input-store delegation reached via a running task's own `dicode.runs.replay` IPC call) are deliberately not separately tracked — they run synchronously inside an already-reserved slot, so tracking them again would be redundant and, for a call made after shutdown latches, would wrongly abort a sub-run whose parent is already committed to finishing.
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
- **`scsstore.go`** — SQLite-backed session store adapter for `alexedwards/scs/v2`. Short-lived sessions (8h) are managed by scs with automatic expiry and cleanup. Replaces the former in-memory `sessionStore`.
- **`sessions_db.go`** — SQLite-backed `dbSessionStore` for long-lived device tokens: `issueDeviceToken`, `renewFromDevice` (wrapped in `db.Tx()`; implements atomic device token rotation after 24h — deletes old row, inserts new, returns new raw token to caller), `listDevices`, `revokeDevice`, `revokeAllDevices`. Device tokens: 30-day expiry, stored as SHA-256 hash, cookie is HttpOnly + SameSite=Strict. HTTP handlers: `apiAuthRefresh`, `apiListDevices`, `apiRevokeDevice`, `apiLogout`, `apiLogoutAll`.
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
- **Frontend (migrated)** — The dashboard SPA lives in `tasks/buildin/webui/` and is served as a standalone webhook task at `/hooks/webui`. The Go binary no longer embeds the frontend assets. The server catch-all redirects `GET /*` to `/hooks/webui`. See the built-in tasks table below.
  - `static/dicode.js` still embedded — standalone IIFE SDK injected into any webhook task UI; `window.dicode` with `run()`, `stream()`, `execute()`, `result()`, `ansiToHtml()`
- `GET /runs/{runID}/result` — serves `OutputContent` with its MIME type, or `ReturnValue` as `application/json` when no structured output type is set
- 11 existing + 16 new auth/security tests (public path gate, 401 enforcement, session lifecycle, device cookie, rate limiting, **extended lockout**, CORS allowlist, **malformed origin skipping**, security headers, CSP, API key generate/validate/revoke, MCP key check, **device token rotation**, **XFF trust flag**)

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

- `control.go` — **`ControlServer`**: listens at `~/.dicode/daemon.sock` (mode 0600). On startup writes a pre-issued CLI token to `~/.dicode/daemon.token` (mode 0600, atomic write). Handles `cli.ping`, `cli.list`, `cli.run`, `cli.logs`, `cli.status`, `cli.secrets.{list,set,delete}`, `cli.task.{approve,pending}`. `cli.task.pending` lists tasks the approval gate is holding (each with a short content hash); `cli.list` also flags held tasks via `TaskSummary.Pending`. Both read the gate through `SetPendingApprovals`; a nil gate yields an empty list, not an error. Context-aware: per-connection context cancels in-flight `cli.run` on client disconnect. CLI tokens use `tokenCLITTL` (~10 years) — daemon restart re-issues anyway
- `control_client.go` — **`ControlClient`**: `Dial(socketPath, tokenPath)` → `Send(req)` → `Close()`. Handshake decodes a union struct covering both success (`proto`) and error (`error`) envelopes

- `pkg/runtime/deno/server/` **deleted** — both Deno and Python runtimes now import `pkg/ipc`

### `pkg/runtime/deno/sdk/shim.ts` ✅

Injected before every Deno task script. Updated for unified IPC protocol:
- Length-prefix framing (`readExact` + 4-byte LE header) replaces newline-delimited reads
- Handshake on connect: reads `DICODE_TOKEN` from env, sends to server, validates response
- All globals unchanged: `log`, `params`, `env`, `kv`, `input`, `output`, **`dicode`** (`run_task`, `list_tasks`, `get_runs`), **`mcp`** (`list_tools`, `call`)

### `pkg/runtime/python/sdk/dicode_sdk.py` ✅

Injected before every Python task script via `buildWrapper()`. Updated for unified IPC protocol:
- asyncio background IO loop (`asyncio.new_event_loop()` + daemon thread)
- `asyncio.open_unix_connection()` for socket; length-prefix framing via `struct.pack("<I", ...)`
- Handshake on connect: sends `DICODE_TOKEN`, validates server response
- `async def main()` detected via `asyncio.iscoroutinefunction` and run with `asyncio.run()`; return value captured as `result`; writer closed gracefully after `_set_return`
- `_call_async(req)` — bridges `_async_call` into the caller's event loop via `asyncio.wrap_future`; no thread pool
- Full `_async` variants on all globals: `log.*_async`, `params.get_async/all_async`, `kv.*_async`, `dicode.*_async`, `mcp.*_async`
- Globals: `log`, `params`, `env`, `kv`, `input`, `output`, **`dicode`**, **`mcp`**

### `pkg/daemon/` ✅

The daemon process logic, invoked via `dicode daemon`. Exported entry point: `daemon.Run(configPath)`.

- Full component wiring: db → secrets → registry → Deno runtime → Docker/Podman/Python runtimes → trigger engine → reconciler → HTTP gateway → webui → control socket (the tray is the `buildin/tray` daemon task, not a wired Go component)
- `NewControlServer(socketPath, tokenPath, ...)` — creates the CLI control socket and writes `daemon.token`
- `buildSecretsChain(cfg, dataDir, database, log)` returns `(secrets.Chain, secrets.Manager)` — the `Manager` is passed to both `webui.New()` and `NewControlServer()`
- Startup sequence: `CleanupOrphanedContainers` → `CleanupStaleRuns` → build runtimes → build sources → build webui → build control socket → run errgroup (reconciler + engine + webui + control socket)
- `make run` builds and runs `dicode daemon`

### `cmd/dicode/main.go` ✅ (single binary)

CLI dispatcher + daemon mode in one binary:

- Subcommands: `daemon [-config dicode.yaml]`, `run <task-id> [key=value ...]`, `list`, `logs <run-id>`, `status [task-id]`, `task {test,create,edit,save,cancel,delete,approve,pending}`, `secrets {list,set,delete}`, `version` — `task pending` lists gate-held tasks with their content hash; `list` marks them in an APPROVAL column
- `daemon` subcommand calls `pkg/daemon.Run()` — starts the full engine in-process
- **Auto-start**: if `~/.dicode/daemon.sock` is not connectable, re-execs itself (`dicode daemon`) in the background, redirects stderr to `~/.dicode/daemon.log`, polls for the socket (8 second timeout)
- Reads `~/.dicode/daemon.token` and calls `ipc.Dial()` to connect
- `DICODE_DATA_DIR` env var overrides the default data directory

### `tasks/examples/` ✅

| Example | Trigger | Runtime |
| --- | --- | --- |
| `hello-cron/` | cron | js (deno) |
| `hello-webhook/` | webhook | deno |
| `hello-docker/` | manual | docker |
| `hello-podman/` | manual | podman |
| `hello-python/` | manual | python |
| `nginx-start/` | daemon | docker |
| `github-stars/` | manual | deno |
| `github-push-webhook/` | webhook + HMAC auth | deno |
| `suspend-wizard/` | manual | deno — multi-step suspend/resume wizard with JSON-Schema forms |
| `deploy-wizard/` | manual | deno — multi-step deploy flow using suspend/resume auto-dispatch |
| `doppler-secret-demo/` | manual | deno — resolves secrets through the `buildin/secret-providers/doppler` provider |
| `repo-prune/` | webhook (auth) | deno |
| `webhook-dashboard/` | webhook | deno |
| `webhook-form/` | webhook | deno |

**Built-in tasks** (`tasks/buildin/`):

| Task | Description |
| --- | --- |
| `webui` | Full dashboard SPA served as a webhook task at `/hooks/webui` |
| `tray` | System tray icon daemon (replaced the CGO `pkg/tray/` Go package) |
| `notify` | Native OS desktop notification via deno_notify |
| `alert` | Chain-friendly wrapper that calls `buildin/notify` via `dicode.run_task` |
| `ai-agent` | Chat agent for any OpenAI-compatible endpoint — discovers registered tasks as tools, KV-backed conversation history |
| `ai-agent-claude-cli` | Wraps the official `claude` CLI as a dicode task |
| `mcp` | MCP server (JSON-RPC 2.0 over HTTP POST); fronted by the API-key-gated `/mcp` URL |
| `auth-start` | OAuth broker: generates a signed `/auth/:provider` relay URL |
| `auth-relay` | Receives ECIES-encrypted OAuth tokens at `/hooks/oauth-complete`, verifies broker signature, decrypts, stores in secrets |
| `auth-providers` | Dashboard listing every OAuth provider known to this instance |
| `relay-client` | WebSocket tunnel to the dicode relay broker (webhooks without a public port) |
| `relay-server` / `relay-server-body` | Run dicode-relay in-process under the daemon's Deno runtime |
| `blob-storage` | Filesystem-backed blob store for user tasks |
| `local-storage` | Filesystem-backed blob store (base64 payloads) |
| `write-local` | Writes a string to a file at a given path and mode |
| `secret-providers/doppler` | Resolves secrets from a Doppler workspace via its REST API |
| `git-pr` | Reference git-pr implementation: opens a pull request via `gh pr create` |
| `template` | `${VAR}` placeholder substitution in a template body |
| `audit-export-loki` | Ships the security audit trail to Grafana Loki |
| `temp-cleanup` | Deletes orphaned task scratch files |
| `run-inputs-cleanup` | Hourly cleanup of expired run-input blobs |
| `dev-clones-cleanup` | Removes dev-mode clone directories whose run ID is gone |

`tasks/buildin/webui/` is the full dicode dashboard SPA. It ships as a self-contained webhook task: `index.html` + Lit/LitElement components under `app/`. The engine injects `<base href="/hooks/webui/">` and the dicode SDK on every GET. Auth is enforced client-side by `dc-auth-overlay` (intercepts 401s from the REST API). Any unauthenticated REST call shows the login modal without a page redirect.

---

## What is not yet created

| Package | What it will contain |
|---|---|
| `pkg/store/` | Task store installer (`dicode task install`) |
| `pkg/db/postgres.go` | PostgreSQL implementation |
| `pkg/db/mysql.go` | MySQL implementation |
| MCP tools: `validate_task`, `dry_run_task`, `commit_task` | Advanced agent workflow tools (`test_task` is implemented) |
| Multi-user RBAC | `users` table, argon2id passwords, role-based access (north star) |

---

## Configuration files

| File | Status |
|---|---|
| `go.mod` | ✅ All dependencies declared and resolved |
| `dicode.yaml` | ✅ Example config with all sections and comments |
| `LICENSE` | ✅ AGPL-3.0 license |
| `README.md` | ✅ Comprehensive user documentation |
| `docs/` | ✅ This documentation tree |
| `docs/security-plan.md` | ✅ Security design document (phases 1–4 implemented + hardened) |
| `docs/concepts/security.md` | ✅ Security developer reference (implementation details, DB schema, config reference) |
| `tasks/skills/dicode-task-dev.md` | ✅ Agent skill document (consumed by `buildin/ai-agent` and the `dicodai` preset via the `skills` param) |

---

## Build

```bash
make build   # compiles ./dicode binary
make run     # builds and runs dicode daemon — web UI on :8080, control socket at ~/.dicode/daemon.sock
make test    # go test ./...
make lint    # go fmt + go vet
make clean   # removes the binary
```

---

## Test coverage

100+ tests across: db, secrets, source/local, registry, runtime/deno, runtime/python, trigger (including HMAC), taskset, ipc (including gateway + control socket), webui (including auth), config, metrics, and task packages.

All packages compile with `go test -race ./...` as of 2026-07-10.

E2E test suites (Playwright) cover core UI flows, file changes, webhooks, config, and auth (PRs #18–#20).
