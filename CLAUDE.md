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

**Startup sequence** (`pkg/daemon/daemon.go`, invoked via `dicode daemon`):
1. Load `dicode.yaml` config (or run first-launch onboarding)
2. Init SQLite database (`pkg/db`)
3. Set up secrets chain (`pkg/secrets`) — encrypted SQLite → env vars
4. Init sources (`pkg/source`) — git repos or local folders
5. Create registry (`pkg/registry`) — in-memory task state + SQLite run log
6. Init runtimes (`pkg/runtime/deno`, `pkg/runtime/python`, `pkg/runtime/docker`, `pkg/runtime/podman`)
7. Start trigger engine (`pkg/trigger`) — cron, webhook, manual, daemon, chaining
8. Start web UI + REST API (`pkg/webui`) — serves a `/mcp` URL that forwards to the buildin/mcp task
9. Start reconciler loop (`pkg/registry/reconciler`) — syncs sources every 30s

**Reconciler loop**: polls sources, computes per-task content hash, diffs against registry. New task → register. Removed → deregister. Changed hash → reload. No restart needed after git push.

**Key packages:**

| Package | Role |
|---|---|
| `pkg/config` | Parse `dicode.yaml` into typed Config struct |
| `pkg/source` | Interface + git/local implementations; git uses go-git (no binary) |
| `pkg/registry` | In-memory task map; SQLite run/log persistence |
| `pkg/registry/reconciler` | Diff sources against registry, drive add/remove/update |
| `pkg/trigger` | Schedule/fire tasks; supports cron, webhook, manual, chain, daemon |
| `pkg/runtime/deno` | Execute JS/TS tasks via a Deno subprocess with restrictive `--allow-*` flags; SDK shim embedded from `pkg/runtime/deno/sdk/shim.ts` |
| `pkg/runtime/python` | Execute Python tasks via a `uv`-provisioned interpreter subprocess; SDK embedded from `pkg/runtime/python/sdk/dicode_sdk.py` |
| `pkg/runtime/docker` | Pull image, run container, stream logs, clean up |
| `pkg/runtime/podman` | Rootless container execution via podman CLI |
| `pkg/webui` | chi router, HTMX templates, REST API, WebSocket log streaming; `/mcp` forwards to the buildin/mcp dicode task |
| `pkg/mcp/client` | Generic JSON-RPC 2.0 MCP client used by the dicode SDK's `mcp.list_tools` / `mcp.call` to reach external MCP servers |
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

Task SDK surface (Deno `dicode` global / Python `dicode` module): `kv`, `log`, `params`, `env`, `output`, `mcp`. Deno tasks also get the platform's native `fetch`; Python tasks use stdlib (`urllib`, `requests`, etc.) — there is no HTTP helper on the `dicode` module itself. See `pkg/runtime/deno/sdk/shim.ts` and `pkg/runtime/python/sdk/dicode_sdk.py` for the authoritative shapes.

## Key Design Constraints

- **No external database** — SQLite embedded via `modernc.org/sqlite` (pure Go, no CGO for Linux builds)
- **No git binary** — `go-git/v5` for all git operations
- **JS/TS sandbox** — Deno subprocess per run; sandboxing via Deno's `--allow-net`/`--allow-env`/`--allow-read`/`--allow-write` permission flags derived from the task's declared permissions. The daemon communicates with the task over a unix-domain socket (`pkg/ipc`) so SDK calls like `dicode.kv.set()` go through the Go control plane, not raw `fetch`.
- **Graceful shutdown** — cancels running tasks, stops daemon loops, flushes logs before exit
