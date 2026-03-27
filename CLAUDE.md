# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make build        # compile ./dicode binary
make run          # build and run (Ctrl-C to stop)
make test         # go test ./... -timeout 60s
make test-verbose # with -v flag
make test-race    # with race detector
make lint         # go vet ./...
make tidy         # go mod tidy
make clean        # remove binary
```

Run a single test package: `go test ./pkg/registry/... -timeout 60s -run TestName`

## Architecture

**dicode** is a single Go binary: a GitOps-style task orchestrator that watches a git repo of JavaScript/Docker task scripts and executes them on cron schedules, webhooks, or manual triggers.

**Startup sequence** (`cmd/dicode/main.go`):
1. Load `dicode.yaml` config (or run first-launch onboarding)
2. Init SQLite database (`pkg/db`)
3. Set up secrets chain (`pkg/secrets`) — encrypted SQLite → env vars
4. Init sources (`pkg/source`) — git repos or local folders
5. Create registry (`pkg/registry`) — in-memory task state + SQLite run log
6. Init runtimes (`pkg/runtime/js`, `pkg/runtime/docker`)
7. Start trigger engine (`pkg/trigger`) — cron, webhook, manual, daemon, chaining
8. Start web UI + REST API (`pkg/webui`)
9. Start MCP server (`pkg/mcp`) — AI agent integration
10. Start reconciler loop (`pkg/registry/reconciler`) — syncs sources every 30s

**Reconciler loop**: polls sources, computes per-task content hash, diffs against registry. New task → register. Removed → deregister. Changed hash → reload. No restart needed after git push.

**Key packages:**

| Package | Role |
|---|---|
| `pkg/config` | Parse `dicode.yaml` into typed Config struct |
| `pkg/source` | Interface + git/local implementations; git uses go-git (no binary) |
| `pkg/registry` | In-memory task map; SQLite run/log persistence |
| `pkg/registry/reconciler` | Diff sources against registry, drive add/remove/update |
| `pkg/trigger` | Schedule/fire tasks; supports cron, webhook, manual, chain, daemon |
| `pkg/runtime/js` | Execute JS tasks via goja sandbox (one isolated runtime per run) |
| `pkg/runtime/js/globals` | Inject `http`, `kv`, `log`, `params`, `env`, `fs`, `output`, `notify` |
| `pkg/runtime/docker` | Pull image, run container, stream logs, clean up |
| `pkg/webui` | chi router, HTMX templates, REST API, WebSocket log streaming |
| `pkg/webui/ai` | Call OpenAI-compatible API to generate task code from prompts |
| `pkg/mcp` | MCP server exposing tools for AI agents to develop/test/deploy tasks |
| `pkg/secrets` | AES-encrypted SQLite store + env var fallback |
| `pkg/task` | Parse `task.yaml`, validate spec, compute content hash |
| `pkg/notify` | Push notifications on failure (ntfy.sh) |
| `pkg/relay` | WebSocket tunnel for public webhook URLs |

## Task Format

Tasks are folders in the watched source directory:

```
tasks/my-task/
├── task.yaml      # trigger config, params, env var names, timeout, runtime
├── task.js        # JavaScript logic (or docker config for docker runtime)
└── task.test.js   # optional unit tests with mocked globals
```

`task.js` globals: `http`, `kv`, `log`, `params`, `env`, `fs`, `output`, `notify`

## Key Design Constraints

- **No external database** — SQLite embedded via `modernc.org/sqlite` (pure Go, no CGO for Linux builds)
- **No git binary** — `go-git/v5` for all git operations
- **JS sandbox** — goja (ES5+, not Node.js); each task run gets a fresh isolated runtime
- **Graceful shutdown** — cancels running tasks, stops daemon loops, flushes logs before exit
