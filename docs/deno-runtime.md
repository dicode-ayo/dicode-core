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

---

## Task structure

```text
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

log.info(`Processing up to ${limit} items`)

const prev = await kv.get("last_count")
if (prev) log.info(`Last run: ${prev}`)

await kv.set("last_count", limit)

return { processed: limit }
```

---

## SDK globals

The Deno runtime injects all globals via a Unix socket bridge. No imports needed — all globals are available at the top level.

### `log`

```typescript
log.info("message")
log.warn("something looks off")
log.error("it broke")
log.debug("verbose detail")
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
  log.info(`upstream returned: ${JSON.stringify(input)}`)
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

---

## Agent globals

### `dicode` — task orchestration

Allows a task to orchestrate other tasks. Requires `security.allowed_tasks` to be configured.

```typescript
// Run another task and await its result
const result = await dicode.run_task("send-report", { channel: "#ops" })
// result: { runID, status, returnValue }

// List all registered tasks
const tasks = await dicode.list_tasks()
// tasks: [{ id, name, description, params }]

// Get recent run history for a task
const runs = await dicode.get_runs("send-report", { limit: 5 })

// Get AI provider config (resolved server-side)
const ai = await dicode.get_config("ai")
// ai: { baseURL, model, apiKey }
```

**task.yaml security config:**

```yaml
security:
  allowed_tasks:
    - "send-report"   # specific task ID
    - "*"             # or allow all tasks
```

### `mcp` — MCP server tools

Allows a task to call tools exposed by daemon tasks that declare `mcp_port`. Requires `security.allowed_mcp`.

```typescript
// List available tools on an MCP server
const tools = await mcp.list_tools("github-mcp")

// Call an MCP tool
const result = await mcp.call("github-mcp", "search_repositories", { query: "dicode" })
```

**task.yaml security config:**

```yaml
security:
  allowed_mcp:
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
security:
  allowed_tasks: ["*"]
```

```typescript
// task.ts
import OpenAI from "npm:openai"

const ai = await dicode.get_config("ai")
const client = new OpenAI({
  baseURL: ai.baseURL || undefined,
  apiKey: ai.apiKey || "ollama",
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
on_failure_chain: failure-monitor   # override for this task
# on_failure_chain: ""              # disable global default for this task
```

A global default can be set in `dicode.yaml`:

```yaml
defaults:
  on_failure_chain: failure-monitor
```

The failure handler receives:

```typescript
// input to the failure handler task:
// { taskID, runID, status, output }
const { taskID, runID } = input
log.info(`Task ${taskID} failed — run ${runID}`)
```

---

## npm / jsr imports

Any npm or jsr package can be imported inline:

```typescript
import OpenAI from "npm:openai"
import { z } from "npm:zod"
import * as _ from "npm:lodash-es"
```

Deno caches packages on first run.

---

## Deno permissions

Permissions are derived from `task.yaml`:

| Permission | Source |
| --- | --- |
| `--allow-net` | Always granted |
| `--allow-env=DICODE_SOCKET,DICODE_TOKEN,VAR1,...` | `DICODE_SOCKET`, `DICODE_TOKEN` (IPC handshake) + all `env:` vars |
| `--allow-read=path1,path2` | `fs:` entries with `r` or `rw` |
| `--allow-write=path1` | `fs:` entries with `w` or `rw` |

---

## Warm process pool

By default, dicode spawns a fresh Deno process for every task execution. Deno's cold-start costs 100–300 ms per invocation on typical hardware. The **warm process pool** eliminates this overhead by keeping a set of Deno processes pre-initialized and ready to receive a task.

### How it works

1. On daemon startup, the pool pre-spawns N Deno processes. Each process runs a lightweight bootstrap shim that connects back to the daemon and waits for a dispatch message.
2. When a task is triggered, the daemon dispatches the script to a waiting process (< 1 ms), the process executes it, then exits.
3. A replacement process is spawned in the background to keep the pool full.

If no warm process is available within 500 ms (e.g. pool is still warming up or all slots are busy), the runtime falls back to the normal cold-spawn path transparently.

### Enabling the pool

Set the `DICODE_DENO_POOL_SIZE` environment variable before starting `dicoded`:

```bash
DICODE_DENO_POOL_SIZE=3 dicoded
```

- **0** (default) — pool disabled; all tasks cold-spawn (backwards-compatible)
- **1–N** — keep N warm processes ready at all times

A pool size of **2–4** is a good starting point for most workloads. Larger values consume more memory (each idle Deno process uses ~30–60 MB RSS) but reduce latency under high concurrency.

### When to use it

| Scenario | Recommendation |
| --- | --- |
| Infrequent manual tasks | Pool not needed (default size 0) |
| Webhook-driven tasks with < 1 s SLA | `DICODE_DENO_POOL_SIZE=2` |
| High-throughput cron / chain workloads | `DICODE_DENO_POOL_SIZE=4` |
| Memory-constrained environments | Keep at 0 or 1 |

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
