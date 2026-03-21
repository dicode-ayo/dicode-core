# Current State

> Last updated: 2026-03-22

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
- `spec.go` — `Spec`, `TriggerConfig`, `ChainTrigger`, `Param` structs
- `LoadDir(dir)` — reads and validates `task.yaml` from a directory
- `Script()` — reads task script source
- `validate()` — schema validation (name, trigger, runtime, chain.on)
- `hash.go` — `Hash(dir)` SHA256 over task.yaml + task.js
- `ScanDir(tasksDir)` — scans tasks directory, returns map[taskID]hash

### `pkg/source/` 🟡
- `source.go` — `Source` interface (`ID()`, `Start()`, `Sync()`), `Event` type, `EventKind` constants
- `source/git/` — **not yet created** (go-git implementation pending)
- `source/local/` — **not yet created** (fsnotify implementation pending)

### `pkg/secrets/` 🔧
- `provider.go` — `Provider` interface, `Chain`, `ResolveAll()`, `NotFoundError` ✅
- `env.go` — `EnvProvider` (reads host env vars) ✅
- `local.go` — `LocalProvider` struct, encryption/decryption (ChaCha20-Poly1305 + Argon2id), master key management ✅
  - Missing: `localDB` interface implementation (sqlite backend not yet written)
  - `Set()`, `Get()`, `Delete()`, `List()` — implemented but blocked on sqlite backend

### `pkg/notify/` 🔧
- `notify.go` — `Notifier` interface, `Message`, `Priority`, `Action`, `NoopNotifier` ✅
- `ntfy.go` — `NtfyNotifier` full HTTP implementation ✅
- `gotify.go` — **not yet created**
- `desktop.go` — **not yet created** (OS desktop notifications)

### `pkg/mcp/` 🟡
- `server.go` — `Server` struct, `Handler()` returning a stub HTTP mux, `Shutdown()`
- All MCP tools — **not yet implemented** (TODO comments in place)

### `pkg/agent/` ✅
- `skill.go` — `//go:embed skill.md` + exported `Skill` string
- `skill.md` — complete agent skill document (workflow, rules, globals reference, test format, common mistakes)

### `pkg/relay/` 🟡
- `relay.go` — `Client` struct, `Start()`, `WebhookURL()`, `WebhookHandler` type
- WebSocket tunnel logic — **not yet implemented**

### `pkg/service/` 🟡
- `service.go` — `Manager` interface (Install, Uninstall, Start, Stop, Restart, Status, Logs)
- Platform-specific implementations — **not yet created**

### `pkg/db/` 🟡
- `db.go` — `DB` interface, `Scanner`, `Config`, `Open()` dispatcher, `UnsupportedBackendError`
- `sqlite.go` — **not yet created**
- `postgres.go` — **not yet created**
- `mysql.go` — **not yet created**

### `pkg/onboarding/` 🔧
- `onboarding.go` — `Required()`, `DefaultLocalConfig()`, `WriteConfig()`, `Mode`, `Result` ✅
- Browser wizard (HTTP server + HTML page) — **not yet implemented**

### `cmd/dicode/main.go` 🔧
- Binary entry point ✅
- Flag parsing (`--config`) ✅
- Onboarding check (calls `onboarding.Required()`, writes default config) ✅
- `task` subcommand skeleton ✅
- `version` subcommand ✅
- Component wiring (`run()` function) — **TODO stub only**

---

## What is not yet created

These packages are designed (documented in plan + README) but have zero code:

| Package | What it will contain |
|---|---|
| `pkg/source/git/` | GitSource — go-git poll + push webhook handler |
| `pkg/source/local/` | LocalSource — fsnotify watcher |
| `pkg/trigger/` | Cron, webhook, manual, chain trigger engine |
| `pkg/registry/` | In-memory task registry + sqlite-backed run log |
| `pkg/runtime/js/` | goja runtime + all injected globals (http, kv, log, etc.) |
| `pkg/runtime/js/dicode.go` | `dicode` global (progress, trigger, isRunning, ask) |
| `pkg/testing/` | Task test harness (mock globals, assert, runTask) |
| `pkg/store/` | Task store installer (`dicode task install`) |
| `pkg/ai/` | Claude API integration + prompt builder |
| `pkg/webui/` | HTTP server, REST API handlers, HTMX templates |
| `pkg/tray/` | System tray icon (systray) |
| `pkg/notify/desktop.go` | OS desktop notifications |
| `pkg/db/sqlite.go` | SQLite implementation of DB interface |
| `pkg/db/postgres.go` | PostgreSQL implementation |
| `pkg/db/mysql.go` | MySQL implementation |

---

## Configuration files

| File | Status |
|---|---|
| `go.mod` | ✅ All dependencies declared (not yet `go mod tidy`'d — Go not installed) |
| `dicode.yaml` | ✅ Example config with all sections and comments |
| `BUSINESSPLAN.md` | ✅ Full business model documentation |
| `README.md` | ✅ Comprehensive user documentation |
| `docs/` | ✅ This documentation tree |
| `pkg/agent/skill.md` | ✅ Agent skill document |

---

## What needs to happen before dicode can run

In order of dependency:

1. **Go installation** — `mise use go@1.23` then `go mod tidy`
2. **`pkg/db/sqlite.go`** — everything needs storage
3. **`pkg/source/local/`** — enables local-only mode (lowest friction first)
4. **`pkg/registry/`** — task registry backed by sqlite
5. **`pkg/runtime/js/`** — goja + globals (http, kv, log, params, env)
6. **`pkg/trigger/`** — cron + manual triggers first
7. **`pkg/webui/`** — REST API + basic UI (needed to see anything)
8. Wire `run()` in `main.go`

At that point, dicode can run in local-only mode with cron and manual triggers, and basic WebUI. Everything else builds on top of this foundation.

See [Implementation Plan](./implementation-plan.md) for the full ordered roadmap.
