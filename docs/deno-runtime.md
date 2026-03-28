# Deno Runtime Integration Plan

Replace the embedded goja JS engine with a Deno subprocess runtime, giving tasks
full npm support, native TypeScript, and a real V8 engine — while preserving the
existing globals API (`log`, `kv`, `params`, `env`, `input`, `output`) and keeping
all other runtimes (`docker`) and infrastructure untouched.

---

## Architecture Overview

```
dicode-core (Go)
  │
  ├── pkg/deno/                   ← binary manager (download, cache, verify)
  │
  ├── pkg/runtime/deno/
  │   ├── runtime.go              ← Run() — mirrors js/runtime.go signature
  │   ├── runtime_test.go
  │   └── sdk/
  │       └── shim.js             ← injected before every task script
  │
  └── pkg/runtime/deno/server/
      └── server.go               ← per-run unix socket server
```

### Execution flow

```
Run(ctx, spec, opts)
  1. Create run record in registry
  2. Resolve secrets from spec.Env
  3. Start unix socket server  →  /tmp/dicode-<runID>.sock
  4. Write wrapped script to temp file  (shim + task content)
  5. Spawn: deno run --allow-net --allow-env=... --allow-read=... <tempfile>
             └── DICODE_SOCKET env var points to socket
  6. Stream deno stderr → log.warn in registry (real-time)
  7. Block until: POST /return received | process exit | ctx timeout
  8. Shutdown socket server, delete temp files
  9. Return RunResult
```

### Globals bridge

| Global | Bridge method |
|--------|--------------|
| `env`  | Process env vars (resolved secrets passed directly) |
| `params` | `GET /params` on socket |
| `log` | `POST /log` on socket → registry in real-time |
| `kv` | `GET /kv/:key`, `PUT /kv/:key`, `DELETE /kv/:key`, `GET /kv` on socket |
| `input` | `GET /input` on socket (chain-triggered tasks) |
| `output` | `POST /output` on socket |
| `http` | Native Deno `fetch` — no bridge needed |
| return value | `POST /return` on socket |

### Deno permissions (derived from task.yaml)

```
--allow-net                          always (tasks make HTTP calls)
--allow-env=DICODE_SOCKET,VAR1,...   DICODE_SOCKET + declared env: vars
--allow-read=/path1,/path2           from fs: entries with r or rw permission
--allow-write=/path1                 from fs: entries with w or rw permission
```

---

## Checklist

### Phase 1 — Deno binary manager (`pkg/deno/`)

- [ ] `EnsureDeno(version string) (path string, err error)`
  - [ ] Resolve cache path: `~/.cache/dicode/deno/<version>/deno`
  - [ ] Return cached path if binary already exists
  - [ ] Detect platform: `GOOS`/`GOARCH` → deno release filename
  - [ ] Download zip from `https://github.com/denoland/deno/releases/download/v<version>/deno-<platform>.zip`
  - [ ] Verify sha256 against `deno-<platform>.zip.sha256sum` from same release
  - [ ] Extract `deno` binary from zip, write to cache path, `chmod 0755`
- [ ] Platform matrix: `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`
- [ ] Configurable version (default pinned in `pkg/deno/version.go`)
- [ ] Tests: mock HTTP download, verify extraction and checksum logic

### Phase 2 — Per-run socket server (`pkg/runtime/deno/server/`)

- [ ] `Server` struct: holds run context, registry, db, resolved env/params/input
- [ ] `Start(runID string) (socketPath string, err error)` — creates unix socket
- [ ] `Stop()` — closes listener, removes socket file
- [ ] Endpoints:
  - [ ] `GET  /params`      → JSON of all merged params
  - [ ] `GET  /input`       → JSON of chained input (null if not a chain run)
  - [ ] `POST /log`         → `{ level, message, fields }` → `registry.AppendLog`
  - [ ] `GET  /kv/:key`     → sqlite kv read, namespaced by taskID
  - [ ] `PUT  /kv/:key`     → sqlite kv write
  - [ ] `DELETE /kv/:key`   → sqlite kv delete
  - [ ] `GET  /kv`          → list all keys for task
  - [ ] `POST /return`      → capture task return value, signal run complete
  - [ ] `POST /output`      → capture structured output (html/text/image/file)
- [ ] Real-time log streaming (each `/log` call writes to registry immediately)
- [ ] Tests: unit test each endpoint, verify kv namespacing

### Phase 3 — SDK shim (`pkg/runtime/deno/sdk/shim.js`)

- [ ] Unix socket HTTP client helper (`_call(method, path, body)`)
- [ ] `log` global: `info`, `warn`, `error`, `debug` → `POST /log`
- [ ] `params` global: `get(key)`, `all()` → `GET /params`
- [ ] `kv` global: `get`, `set`, `delete`, `list` → socket endpoints
- [ ] `env` global: `get(key)` → `Deno.env.get(key)` (vars already in process env)
- [ ] `input` global: fetched once at startup from `GET /input`
- [ ] `output` global: `html`, `text`, `image`, `file` → `POST /output`
- [ ] Script wrapper: Go wraps task content so `return value` still works:
  ```js
  // prepended by runtime — not visible to task author
  import { log, kv, params, env, input, output } from "/__dicode__/sdk.js";
  const __result__ = await (async () => {
    // ---- task script content ----
  })();
  await __setReturn__(__result__);
  ```
- [ ] Embed shim.js into Go binary via `//go:embed sdk/shim.js`
- [ ] Tests: shim logic unit tests (mock socket server)

### Phase 4 — Deno runtime (`pkg/runtime/deno/runtime.go`)

- [ ] `Runtime` struct: `registry`, `secrets`, `db`, `log`, `denoPath`
- [ ] `New(r, sc, db, log) *Runtime` — calls `deno.EnsureDeno()` on startup
- [ ] `Run(ctx, spec, opts) (*RunResult, error)`:
  - [ ] Create/load run record in registry
  - [ ] Resolve secrets from `spec.Env`
  - [ ] Start socket server, defer `Stop()`
  - [ ] Write wrapped script to `os.CreateTemp`
  - [ ] Build `deno run` command with derived permission flags
  - [ ] Set `DICODE_SOCKET` + resolved env vars on subprocess
  - [ ] Pipe stderr to registry logs in real-time (goroutine)
  - [ ] Wait: socket `/return` received OR process exits OR `ctx` cancelled
  - [ ] Map exit states to `StatusSuccess`, `StatusFailure`, `StatusCancelled`
  - [ ] Clean up temp file
  - [ ] Return `RunResult`
- [ ] Timeout: respect `spec.Timeout`, send `SIGTERM` then `SIGKILL` after grace period
- [ ] Tests: mirror `pkg/runtime/js/runtime_test.go` test cases

### Phase 5 — Wiring

- [ ] `pkg/task/spec.go`: add `RuntimeDeno = "deno"` constant
- [ ] `pkg/trigger/engine.go`:
  - [ ] Add `denoRT *denoruntime.Runtime` field to `Engine`
  - [ ] Add `SetDenoRuntime(rt *denoruntime.Runtime)` method
  - [ ] Add `case task.RuntimeDeno` in `dispatch()`
- [ ] `cmd/dicode/main.go`:
  - [ ] Instantiate `denoruntime.New(...)`
  - [ ] Call `eng.SetDenoRuntime(denoRT)`
- [ ] Update task spec validation to accept `"deno"` as a valid runtime value

### Phase 6 — Goja removal

- [ ] Confirm all existing `runtime: js` tasks migrated or relabelled to `runtime: deno`
- [ ] Delete `pkg/runtime/js/` directory
- [ ] Delete `pkg/runtime/js/globals/` directory
- [ ] Remove `jsRT *jsruntime.Runtime` field from `trigger.Engine`
- [ ] Remove `jsruntime` import and wiring from `cmd/dicode/main.go`
- [ ] Remove `case task.RuntimeJS` (or `default`) from `engine.dispatch()`
- [ ] Remove `RuntimeJS` constant from `pkg/task/spec.go`
- [ ] Drop goja dependencies from `go.mod`:
  - [ ] `github.com/dop251/goja`
  - [ ] `github.com/dop251/goja_nodejs`
- [ ] Run `go mod tidy`
- [ ] Confirm no remaining references: `grep -r "goja" .`

### Phase 7 — Docs & task migration

- [ ] Update `README.md`: replace goja/JS runtime section with Deno
- [ ] Update `docs/concepts/` and `docs/introduction.md` if they reference goja
- [ ] Update `dicode.yaml` example if it references `runtime: js`
- [ ] Migrate built-in example tasks (`hello-cron`, `gmail-to-slack`) to `runtime: deno`
- [ ] Add `runtime: deno` to task spec documentation

---

## What stays unchanged

- `task.yaml` format — only `runtime: js` → `runtime: deno`
- All globals names and API (`log`, `kv`, `params`, `env`, `input`, `output`)
- Trigger engine, cron scheduling, chain logic, webhook dispatch
- `runtime: docker` — fully unaffected
- Registry, secrets, sqlite persistence layer

---

## Files changed summary

| File | Change |
|------|--------|
| `pkg/deno/` | New package |
| `pkg/runtime/deno/` | New package |
| `pkg/runtime/js/` | Deleted (Phase 6) |
| `pkg/task/spec.go` | Add `RuntimeDeno`, remove `RuntimeJS` |
| `pkg/trigger/engine.go` | Add deno field, dispatch case; remove js |
| `cmd/dicode/main.go` | Wire deno runtime; remove js runtime |
| `go.mod` | Remove goja deps, `go mod tidy` |
