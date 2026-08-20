# Deno Runtime

dicode executes TypeScript/JavaScript tasks via [Deno](https://deno.com/) — the Deno binary is downloaded and cached automatically; no system installation is required.

---

## Setup

The Deno runtime is always available. To update to a specific version:

1. Open **Config → Runtimes** in the dicode web UI.
2. Find **Deno** in the table, change the version, and click **Install**.

Or pin a version in `dicode.yaml`:

```yaml
runtimes:
  deno:
    version: "2.3.3"
```

Pinning a version routes every Deno run through a dedicated per-version
executor instead of the default one. That executor gets the full SDK surface
— `dicode.run_task`, `dicode.http.register`, `dicode.secrets_set`/
`secrets_delete`, and correctly-signed per-run IPC capability tokens — the
same as the unpinned default (fixed in [#718](https://github.com/dicode-ayo/dicode-core/issues/718); versions before that fix silently
dropped these for a pinned executor).

---

## Task structure

```
tasks/
└── my-task/
    ├── task.yaml
    └── task.ts
```

### task.yaml

```yaml
name: My Task
runtime: deno
trigger:
  manual: true

params:
  - name: limit
    default: "10"
    description: Maximum items to process

env:
  - API_TOKEN

timeout: 60s
```

### task.ts

```typescript
// SDK globals are injected automatically — no imports needed.

const limit = parseInt(params.limit)
const token = env.API_TOKEN

console.log(`Processing up to ${limit} items`)

const prev = await kv.get("last_count")
if (prev) console.log(`Last run: ${prev}`)

await kv.set("last_count", limit)

return { processed: limit }
```

---

## SDK globals

The Deno runtime injects all globals via a Unix socket bridge. No imports needed — all globals are available at the top level.

### Logging

Use standard `console` methods — stdout is captured as `info` and stderr as `error` in the run log:

```typescript
console.log("processing started")
console.warn("something looks off")
console.error("it broke")
console.debug("verbose detail")
```

### `params`

```typescript
const value = params.my_param       // string, uses default if not overridden
```

### `env`

```typescript
const token = env.API_TOKEN         // reads from host environment
```

### `kv`

Persistent key-value store scoped to the task.

```typescript
await kv.set("counter", 42)
const value = await kv.get("counter")    // null if not set
const keys  = await kv.list()            // all keys
const keys  = await kv.list("prefix_")  // keys with prefix
await kv.delete("counter")
```

### `input`

The return value of the upstream task (chain triggers), or the parsed webhook POST body.

```typescript
if (input) {
  console.log(`upstream returned: ${JSON.stringify(input)}`)
}
```

### `output`

Rich output types rendered in the Web UI.

```typescript
output.html("<h1>Report</h1><table>...</table>")
output.text("plain text result")

// HTML with structured data for chain triggers
output.html(html, { data: { count: 5 } })  // chained tasks receive { count: 5 }
```

### Return value

```typescript
return { count: 42, status: "ok" }
```

### `suspend` — pause for human input, auto-dispatched

Pause a run to collect input from a human. `dicode.suspend({ schema })` never
returns — the process exits, the run becomes `suspended`, and on resume the
runner re-runs the file and **dispatches the right handler for you**: no
hand-rolled `if (resume_state)` switch. Each handler reads `ctx.state` (the blob
you carried) and `ctx.input` (the validated submission).

Export **`main`** (the first run) and optionally **`resume`** (the continuation):

```typescript
export default async function main({ dicode }) {
  await dicode.suspend({
    schema: {
      type: "object",
      properties: { ok: { type: "boolean", title: "OK?" } },
      required: ["ok"],
    },
  })  // unreachable — never returns
}

export async function resume({ input }) {
  return { confirmed: input.ok }
}
```

For a multi-step wizard, export a **`steps`** map and name the next handler with
`suspend({ to })`. A `to` that names no exported step fails the run loudly rather
than falling back to `main`/`resume`. `schema` is a
[JSON Schema](https://json-schema.org) (draft 2020-12); the daemon compiles it at
`suspend()` time (rejecting a malformed schema up front) and validates the
submission against it before resuming, so `ctx.input` conforms.

See [Suspendable Tasks](./concepts/suspendable-tasks.md) for the dispatch rules,
the `steps` wizard shape, lifecycle (`suspended` → `resumed`), and how to resume
via the Web UI or `dicode resume`.

---

## Agent globals

### `dicode` — task orchestration

Allows a task to orchestrate other tasks. Requires `permissions.dicode.tasks` to be configured.

```typescript
// Run another task and await its result
const result = await dicode.run_task("send-report", { channel: "#ops" })
// result: { runID, status, returnValue }

// List all registered tasks
const tasks = await dicode.list_tasks()
// tasks: [{ id, name, description, params }]

// Get recent run history for a task
const runs = await dicode.get_runs("send-report", { limit: 5 })
```

**task.yaml permissions config:**

```yaml
permissions:
  dicode:
    tasks:
      - "send-report"   # specific task ID
      - "*"             # or allow all tasks
```

### `mcp` — MCP server tools

Allows a task to call tools exposed by daemon tasks that declare `mcp_port`. Requires `permissions.dicode.mcp`.

```typescript
// List available tools on an MCP server
const tools = await mcp.list_tools("github-mcp")

// Call an MCP tool
const result = await mcp.call("github-mcp", "search_repositories", { query: "dicode" })
```

**task.yaml permissions config:**

```yaml
permissions:
  dicode:
    mcp:
      - "github-mcp"   # daemon task ID that declares mcp_port
      - "*"            # or allow all MCP servers
```

**MCP daemon task example:**

```yaml
# tasks/github-mcp/task.yaml
name: GitHub MCP Server
runtime: docker
trigger:
  daemon: true
mcp_port: 3000
docker:
  image: ghcr.io/github/github-mcp-server
  ports: ["3000:3000"]
env:
  - GITHUB_TOKEN
```

---

## Agent task pattern

A full AI agent task using OpenAI tool-use:

```yaml
# task.yaml
name: ai-agent
runtime: deno
trigger:
  webhook: /hooks/agent
  auth: true
params:
  - name: prompt
    type: string
    required: true
permissions:
  dicode:
    tasks: ["*"]
```

```typescript
// task.ts
import OpenAI from "npm:openai"

// Read provider config from task params (see the ai-agent task for the
// canonical pattern). Declare OPENAI_API_KEY in permissions.env.
const apiKey = await env.get("OPENAI_API_KEY") ?? "ollama"
const client = new OpenAI({
  baseURL: params.base_url || undefined,
  apiKey,
})

const allTasks = await dicode.list_tasks()
const tools = allTasks.map(t => ({
  type: "function" as const,
  function: {
    name: t.id.replace(/[^a-z0-9_]/gi, "_"),
    description: t.description,
    parameters: {
      type: "object",
      properties: Object.fromEntries(
        (t.params ?? []).map((p: any) => [p.name, { type: "string", description: p.description }])
      ),
    },
  },
}))

const messages: OpenAI.Chat.ChatCompletionMessageParam[] = [
  { role: "user", content: params.prompt },
]

while (true) {
  const response = await client.chat.completions.create({
    model: ai.model || "gpt-4o-mini",
    messages,
    tools,
    tool_choice: "auto",
  })
  const msg = response.choices[0].message
  messages.push(msg)

  if (!msg.tool_calls?.length) {
    return { answer: msg.content }
  }

  for (const call of msg.tool_calls) {
    const taskID = call.function.name.replace(/_/g, "-")
    const callParams = JSON.parse(call.function.arguments)
    const result = await dicode.run_task(taskID, callParams)
    messages.push({
      role: "tool",
      tool_call_id: call.id,
      content: JSON.stringify(result),
    })
  }
}
```

---

## on_failure_chain

A task can declare a failure handler that runs automatically when it fails:

```yaml
on_failure_chain: failure-monitor   # short form — override for this task
# on_failure_chain: ""              # disable global default for this task
```

The structured form accepts engine guardrails and chain params:

```yaml
on_failure_chain:
  task: buildin/auto-fix
  params:                           # forwarded into the chained run's input
    mode: review                    # auto-fix preset: "review" (open PR) | "autonomous" (push)
    branch_prefix: "fix/"
  cooldown: 10m                     # min interval between two chain fires for this task; default 10m
  max_concurrent: 1                 # in-flight chain runs per failing task; default 1
  max_depth: 2                      # chain hops before suppression; default 2
```

A global default lives in `dicode.yaml`:

```yaml
defaults:
  on_failure_chain:
    task: failure-monitor
    max_concurrent_global: 3        # ceiling across ALL tasks; defaults-only
    storm:                          # circuit-breaker — defaults-only
      rate: 10                      # if > rate fires
      window: 1m                    # within window
      suppress: 30m                 # suppress that source for this duration
```

`max_concurrent_global` and `storm` are **operator policy** — only honored at the defaults level. Per-task blocks that set them get a config-load WARN and the fields are zeroed.

Per-task `on_failure_chain` **fully replaces** the defaults block (no deep merge). To inherit + extend, restate the full structure.

The failure handler receives:

```typescript
// input to the failure handler task:
// { taskID, runID, status, output, _chain_depth, ...params }
export default async function main({ input }: any) {
  const { taskID, runID, status, _chain_depth } = input
  console.log(`Task ${taskID} failed (depth ${_chain_depth}) — run ${runID}`)
}
```

A replay-fired run that fails does NOT trigger `on_failure_chain` — replay is human/agent-initiated and has no auto-recovery semantics.

---

## Auto-fix loop (`buildin/auto-fix`)

The `buildin/auto-fix` taskset entry is a preset that overrides `ai-agent` with the `dicode-auto-fix` skill loaded into the system prompt. When a task with `on_failure_chain: buildin/auto-fix` fails, an AI agent diagnoses the failure, edits source on a fix branch in a per-fix clone of the source repo, validates via `dicode.tasks.test` + `dicode.runs.replay`, then either pushes (autonomous) or opens a PR via `buildin/git-pr` (review).

```yaml
# tasks/my-task/task.yaml
on_failure_chain:
  task: buildin/auto-fix
  params:
    mode: review                    # default — opens a PR
    # mode: autonomous              # alternative — pushes directly to tracked branch
```

Permissions the auto-fix task needs (set via the `buildin/auto-fix` taskset entry — users typically don't touch these):

```yaml
permissions:
  env:
    - GH_TOKEN_AUTOFIX              # fine-grained PAT for git-pr; contents+pull_requests scope
  fs:
    - path: "${DATADIR}/dev-clones"
      permission: rw
  dicode:
    runs_get_input: true            # read failed run's persisted input (subject to redaction)
    runs_replay: true
    runs_pin_input: true
    runs_unpin_input: true
    sources_set_dev_mode: true      # clone the source onto a fix branch
    tasks_test: true
    git_commit_push: true
    tasks: ["git-pr"]               # call buildin/git-pr to open PRs
```

`runs_get_input` is gated by a lineage check: the auto-fix run can only read inputs of runs whose ID matches its own `parent_run_id` (the failed run that fired it) OR runs of its own task. Users can grant `runs_get_input: true` to other tasks to build their own replayer / fixer / auditor — the redaction layer (deny-listed fields are never persisted) is what bounds the surface.

### LLM backend choice

The auto-fix preset uses `buildin/ai-agent` (OpenAI-compatible HTTPS endpoints, per-token API key billing) by default. To switch to your Claude.ai Pro/Max subscription instead, declare a sibling override that swaps the underlying agent task — see the [Claude CLI agent docs](./claude-cli-agent.md) for the recipe.

---

## npm / jsr imports

Any npm or jsr package can be imported inline:

```typescript
import OpenAI from "npm:openai"
import { z } from "npm:zod"
import * as _ from "npm:lodash-es"
```

Deno caches packages on first run.

### Dependency pinning

If a `deno.lock` file exists at or near the task directory (the runtime walks up to three parent directories), Deno is invoked with `--lock=<path> --frozen`. This prevents per-run resolution of newer versions within a caret range (`^`) — packages are pinned to the exact versions recorded in the lockfile. Buildin tasks ship a `tasks/deno.lock` that is automatically detected and enforced.

When a task's dependencies change, regenerate the lock with `dicode deno relock [dir]` (dir defaults to `tasks`). It provisions the pinned Deno and re-caches every `task.ts` under the tree, so the lock is deterministic regardless of any system Deno. `dicode deno relock --check` verifies the lock without modifying it (exit non-zero if stale) — run it in CI to catch drift before it reaches the runtime. The runtime-spanning `dicode relock [--check] [dir]` runs this Deno pass and the Python sidecar pass (see [python-runtime.md](python-runtime.md)) together — one command covering every task lock.

---

## Deno permissions

Permissions are derived from `task.yaml`:

| Permission | Source |
| --- | --- |
| `--allow-net` / `--allow-net=host1,...` | `net:` entries — omit or `[]` = denied (no flag), `["*"]` = unrestricted, host list = allowlist |
| `--allow-env=DICODE_SOCKET,DICODE_TOKEN,VAR1,...` | `DICODE_SOCKET`, `DICODE_TOKEN` (IPC handshake) + cache vars + all `env:` vars (a `PREFIX_*` pattern entry expands to its matching host var names, minus the daemon credential denylist). Bare `--allow-env` (read any var) when `env_read_exposed: true` — node-compat / npm escape hatch |
| `--allow-read=path1,path2` | `fs:` entries with `r` or `rw` |
| `--allow-write=path1` | `fs:` entries with `w` or `rw` |

---

## Configuration reference

```yaml
runtimes:
  deno:
    version: "2.3.3"   # Deno version; leave blank to use the dicode default

defaults:
  on_failure_chain: my-monitor-task   # global failure handler

ai:
  base_url: "https://api.openai.com/v1"
  model: "gpt-4o-mini"
  api_key_env: OPENAI_API_KEY         # resolved from env, never exposed to tasks directly
```

See [task.yaml reference](./concepts/task-format.md) for the full field list.
