---
name: dicode-task-dev
description: Author, modify, or debug dicode tasks — the JS/TS/Python/Docker automation scripts under tasks/ (task.yaml + task.ts + task.test.ts). Use when writing a new task, editing an existing one, adding triggers/permissions/params, wiring MCP or task chaining, or fixing a failing task test. Covers the task.yaml schema, the SDK globals (kv/log/params/env/input/output/dicode/mcp), the test harness, and the validate → test → run dev loop.
---

# dicode Task Development

dicode is a single Go binary that watches a source of automation scripts ("tasks") and runs them on cron / webhook / manual / chain / daemon triggers. A **task is a folder** holding a `task.yaml` (metadata, trigger, permissions, params) and a runtime file (`task.ts` for Deno, `task.py` for Python, image config for Docker/Podman), plus an optional `task.test.ts`. The reconciler hot-reloads tasks — no daemon restart after an edit.

Tasks live under `tasks/` in this repo: builtins in `tasks/buildin/`, examples in `tasks/examples/`. IDs are namespaced by source, e.g. `buildin/ai-agent`, `examples/github-stars`.

## The authoritative references (read these for depth)

The long-form, canonical developer skill and concept docs already exist in this repo. Read them rather than guessing:

- `tasks/skills/dicode-task-dev.md` — full schema, every SDK global, the complete test-harness API, taskset precedence. **This is the source of truth; this SKILL.md is the quick index.**
- `tasks/skills/dicode-basics.md` — mental model of tasks/triggers/KV/permissions.
- `tasks/skills/dicode-auto-fix.md` — the on-failure auto-fix loop workflow.
- `docs/concepts/task-format.md`, `docs/deno-runtime.md`, `docs/python-runtime.md`, `docs/podman-runtime.md`, `docs/webhooks.md` — runtime + trigger detail.
- `tasks/sdk.ts` — the actual TypeScript SDK surface; `pkg/runtime/deno/sdk/shim.ts` and `pkg/runtime/python/sdk/dicode_sdk.py` are the runtime-authoritative shapes.
- `tasks/examples/*` — working tasks to copy patterns from.

## Dev loop (follow in order)

1. **Look before writing.** `dicode list` (or MCP `list_tasks`) to avoid duplicating an existing task; skim a sibling in `tasks/examples/` for style.
2. **Know the secrets.** `dicode secrets list` — never invent credentials; declare what exists under `permissions.env`.
3. **Write the three files** in `tasks/<source>/<task-id>/`:
   - `task.yaml` — trigger, permissions, params (see schema below)
   - `task.ts` (Deno) or `task.py` (Python) — logic; **must `return` a JSON-serializable value** so chain triggers can consume it
   - `task.test.ts` — required; a task without tests should not ship
4. **Test.** `dicode task test <task-id>` for one task, or `make test-tasks` to run every buildin task's `task.test.ts` through Deno. Fix all failures before proceeding. Add `--format=junit|gh-summary` for CI output.
5. **Exercise it.** `dicode run <task-id> key=value ...` triggers a real run and waits for the result; `dicode logs <run-id>` and `dicode status <task-id>` inspect it.
6. Commit only when tests pass. If the approval gate is on, a changed task lands **pending** — `dicode task approve <task-id>` to release it.

The daemon can also drive an AI author: `dicode task create <name> [--ai "PROMPT"]`, iterate with `dicode task edit <task-id> "<prompt>"`, persist with `dicode task save <session-id>`. `dicode ai "<prompt>"` runs the configured agent task directly.

## task.yaml schema (essentials)

```yaml
apiVersion: dicode/v1
kind: Task
name: <kebab-case-id>            # matches the directory name
description: <what it does>
runtime: deno                    # deno (default) | python | docker | podman
trigger:                         # exactly ONE of:
  cron: "0 9 * * *"              #   5-field cron
  webhook: /hooks/<path>         #   HTTP POST; open unless guarded (below)
  manual: true                   #   UI/API/CLI only
  chain: { from: <task-id>, on: success }   # success | failure | always
  daemon: true                   #   starts on boot, restarts on exit
permissions:                     # deny-by-default allowlist — nothing implicit
  env:                           # env the script may read:
    - SLACK_TOKEN                #   bare: pass through host env, same name
    - { name: API_KEY, from: GH_TOKEN }        # rename from host env
    - { name: DB_PASS, secret: db_password }   # resolve from secrets store
    - { name: LOG_LEVEL, value: "info" }       # literal
  net: ["api.github.com"]        # Deno only; omit = unrestricted; [] = deny all
  fs:  [{ path: ~/data, permission: rw }]      # r | w | rw
  run: ["curl"]                  # spawnable binaries; ["*"] = all
  dicode:                        # dicode runtime API — all denied by default
    tasks: ["*"]                 #   task IDs callable via dicode.run_task()
    mcp: ["github-mcp"]          #   daemon MCP task IDs for mcp.call()
    list_tasks: true
    get_runs: true
    secrets_write: true          #   write-only; tasks can never read secrets back
params:
  slack_channel: { type: string, default: "#general" }
timeout: 60s
mcp_exposed: false               # true → visible/invokable via the MCP server
on_failure_chain: <task-id>      # run on failure; "" disables global default
```

**Guarded webhooks:** add `webhook_secret: "${WEBHOOK_SECRET}"` (HMAC `X-Hub-Signature-256`, GitHub-compatible, 403 on bad sig) or `auth: true` (require a dicode session). Always reference a secret via `"${VAR}"`, never a raw value.

## SDK globals (Deno; Python module `dicode` mirrors most)

- `fetch` — native, for HTTP (Deno). Python uses stdlib (`urllib`/`requests`).
- `kv.set/get/delete` — persistent, per-task namespace, JSON-serializable values.
- `log.info/warn/error(msg, ctx?)` — structured, streamed to the WebUI run log.
- `params.<name>` — typed inputs from `task.yaml`.
- `await env.get("VAR")` — only vars declared in `permissions.env`; else `null`.
- `input` — chain: upstream return value; webhook: parsed POST body.
- `output.html()/text()` — render UI instead of returning JSON.
- `await dicode.run_task(id, params?)` / `list_tasks()` / `get_runs(id, opts)` / `secrets_set()/secrets_delete()` — gated by `permissions.dicode`.
- `await mcp.list_tools(server)` / `mcp.call(server, tool, args)` — gated by `permissions.dicode.mcp`.
- `return <json>` — value handed to downstream chain tasks; keep it under ~1MB.

## Test harness (`task.test.ts`)

Each `test()` gets fresh mock state. Arrange mocks → `await runTask()` → assert.

```typescript
test("happy path", async () => {
  http.mock("GET", "https://api.example.com/*", { status: 200, body: { items: [1, 2] } })
  env.set("SLACK_TOKEN", "xoxb-test")
  params.set("slack_channel", "#test")
  const result = await runTask()
  assert.equal(result.count, 2)
  assert.httpCalledWith("POST", "https://slack.com/api/chat.postMessage", { body: { channel: "#test" } })
})
```

Globals: `test`, `runTask`, `http.mock/mockOnce`, `env.set`, `params.set`, `kv.set`, `assert.equal/ok/throws/httpCalled/httpCalledWith/httpNotCalled`.

## Hard rules

- **Never commit** with a failing `task.yaml` parse or failing tests.
- **Always write `task.test.ts`.**
- **`return` must be JSON-serializable** (required for chaining).
- **Never hardcode secrets** — inject via `permissions.env`, read with `env.get()`.
- **Never declare `DICODE_SOCKET` or `DICODE_TOKEN`** in `permissions.env` — internal IPC vars injected automatically; declaring them leaks the token to task code.
- **One task, one responsibility.** Keep output small — tasks are not a data pipeline.

## MCP access (optional, when the daemon is running)

`dicode mcp install` mints an API key and wires the server into Claude Code (`http://localhost:8080/mcp`). Implemented tools: `list_tasks`, `get_task(id)`, `run_task(id, params?)`, `list_sources`, `switch_dev_mode(source, enabled, local_path?)`. Only tasks with `mcp_exposed: true` are listed/invokable. When the daemon isn't up, drive the loop with the `dicode` CLI + file edits instead.
