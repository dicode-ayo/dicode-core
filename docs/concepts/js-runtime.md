# JS Runtime

> **Deprecated**: This document describes the original goja-based JS runtime (`pkg/runtime/js/`), which was the MVP task execution engine. The primary runtimes are now **Deno** (TypeScript/JavaScript) and **Python** (uv). See [Deno Runtime](../deno-runtime.md) and [Python Runtime](../python-runtime.md) for the current SDKs. The goja runtime remains in the codebase but new tasks should use `runtime: deno` or `runtime: python`.

Tasks run as JavaScript files inside a sandboxed [goja](https://github.com/dop251/goja) runtime. goja is a pure Go ES2020 engine — no CGo, no system Node.js required, runs everywhere Go runs.

---

## Engine properties

- **Language**: ES2020. Async/await, destructuring, template literals, optional chaining, nullish coalescing all work.
- **Top-level await**: supported. Your script can use `await` at the top level.
- **No module system**: `import`/`require` are not available. All dependencies are injected as globals.
- **Isolated**: each task run gets a fresh runtime instance. No shared state between runs.
- **Sandboxed**: no filesystem access, no shell execution, no native modules.
- **Pure Go**: no CGo, no system Node.js. Runs anywhere the dicode binary runs.

---

## Globals reference

### Logging

Use standard `console` methods — no separate `log` global needed:

```javascript
console.log("message")
console.warn("something looks off")
console.error("it broke", err.message)
console.debug("verbose detail")
```

stdout is captured as `info` and stderr as `error` in the run log (visible in the WebUI and via `dicode task run --verbose`).

---

### `env` — resolved secrets and environment variables

```javascript
const token = env.get("SLACK_TOKEN")
```

`env.get(key)` resolves the key through the secrets provider chain (local encrypted store → host env vars → any configured external provider). Returns a string, or throws if the key is not found.

Tasks declare which env vars they need in `task.yaml`:
```yaml
env:
  - SLACK_TOKEN
  - GMAIL_TOKEN
```

At runtime, `env.get()` will only return values for keys declared in `task.yaml`. This is a security boundary — tasks cannot fish for arbitrary host environment variables.

---

### `params` — task parameters

```javascript
const channel = params.get("slack_channel")
const limit = parseInt(params.get("max_emails"), 10)
```

`params.get(name)` returns the value of a declared parameter. All parameter values are strings — parse them if you need a number or boolean.

Parameters are declared in `task.yaml`:
```yaml
params:
  - name: slack_channel
    default: "#general"
  - name: max_emails
    default: "20"
```

At run time, the default is used unless the trigger (webhook body, manual trigger API, or chain input) overrides it.

---

### `http` — outbound HTTP

```javascript
// GET request
const res = await http.get("https://api.example.com/data", {
  headers: { Authorization: `Bearer ${env.get("API_TOKEN")}` },
  timeout: "10s"
})

// POST with JSON body
const res = await http.post("https://api.example.com/items", {
  body: { name: "thing", value: 42 },   // auto-serialized as JSON
  headers: { "Content-Type": "application/json" }
})

// Other methods
await http.put(url, options)
await http.patch(url, options)
await http.delete(url, options)
```

**Response object:**
```javascript
res.status    // number: HTTP status code
res.headers   // object: response headers (lowercase keys)
res.body      // parsed JSON if Content-Type is application/json, else string
res.ok        // boolean: status < 400
```

**Options:**
| Field | Type | Default | Description |
|---|---|---|---|
| `headers` | object | `{}` | Request headers |
| `body` | object or string | | Request body. Objects are JSON-serialized. |
| `timeout` | string | `"30s"` | Request timeout. e.g. `"10s"`, `"1m"` |
| `follow_redirects` | bool | `true` | Follow HTTP redirects |

**Error handling**: throws a JavaScript Error if the request fails (network error, timeout). Does NOT throw on non-2xx status codes — check `res.ok` or `res.status` yourself.

---

### `kv` — persistent key-value store

Per-task namespaced storage, backed by sqlite. Survives across runs.

```javascript
// Store a value (any JSON-serializable value)
await kv.set("last_run_id", "abc123")
await kv.set("config", { threshold: 10, channel: "#alerts" })

// Retrieve a value
const id = await kv.get("last_run_id")      // "abc123" or null
const cfg = await kv.get("config")          // { threshold: 10, ... } or null

// Delete
await kv.delete("last_run_id")

// List keys with a prefix
const keys = await kv.list("session:")      // ["session:a", "session:b", ...]
```

Values are JSON-serialized on write and deserialized on read. `kv.get()` returns `null` for missing keys.

Keys are namespaced per task ID — two tasks cannot access each other's KV data.

---

### `fs` — filesystem access

Only available when `fs:` is declared in `task.yaml`. Zero filesystem access by default.

Every call is validated against the declared paths before execution. Requests outside declared paths, or with insufficient permission, throw immediately.

```javascript
// Read
const text = await fs.read("~/reports/data.csv")          // string (utf-8)
const obj  = await fs.readJSON("~/data/config.json")       // parsed object

// Write
await fs.write("~/reports/weekly.html", htmlContent)       // creates or overwrites
await fs.writeJSON("~/reports/summary.json", { count: 42 })
await fs.append("~/reports/log.txt", `${new Date().toISOString()} — done\n`)

// Directory listing
const entries = await fs.list("~/data")
// → [{ name, path, isDir, size, modified }]

// Glob
const csvFiles = await fs.glob("~/data/**/*.csv")          // string[] of matching paths

// File info
const info   = await fs.stat("~/reports/weekly.html")      // { size, modified, isDir }
const exists = await fs.exists("~/reports/weekly.html")    // boolean

// Mutations
await fs.mkdir("~/reports/2026/march")                     // creates full path (like mkdir -p)
await fs.copy("~/reports/weekly.html", "~/archive/weekly-2026-03-22.html")
await fs.move("~/reports/tmp.html", "~/reports/weekly.html")
await fs.delete("~/reports/old.txt")
```

**`fs.list()` entry shape:**
```javascript
{ name: "report.html", path: "/home/alice/reports/report.html", isDir: false, size: 4821, modified: 1742601600000 }
```

**Error cases:**
- Path outside any declared `fs:` entry → `PermissionError: path not declared`
- Insufficient permission (e.g. write to a `r` path) → `PermissionError: write not allowed`
- Symlink resolving outside a declared path → `PermissionError: symlink target not declared`
- File not found → `NotFoundError`

**Security enforcement (Go side):**
1. Resolve requested path to absolute path (`filepath.Abs`)
2. Resolve symlinks (`filepath.EvalSymlinks`)
3. Check resolved path has a declared `fs:` entry as a prefix
4. Check the operation matches the declared permission
5. Block — no silent fallback

---

### `notify` — send notifications

```javascript
await notify.send("Email digest sent", {
  priority: "low",
  tags: ["email"]
})

await notify.send("API is DOWN", {
  priority: "urgent",
  tags: ["alert", "warning"]
})
```

Routes through the notification provider configured in `dicode.yaml` (ntfy, gotify, etc.) and fires OS desktop notification if running locally.

**Options:**
| Field | Type | Default | Description |
|---|---|---|---|
| `priority` | string | `"default"` | `min`, `low`, `default`, `high`, `urgent` |
| `tags` | array | `[]` | Tag strings (provider-specific) |
| `actions` | array | | Action buttons (provider-specific) |

If no notification provider is configured, `notify.send()` is a no-op (no error thrown).

---

### `input` — chain input

Available when a task is triggered by a chain or a webhook. Contains the output value of the preceding task (chain) or the parsed POST body (webhook).

```javascript
// task-b/task.js — triggered by task-a completing
console.log(`Processing ${input.count} items`)
const results = await processItems(input.items)
return { processed: results.length }
```

`input` is `null` for cron and manual triggers.

---

### `dicode` — orchestrator API

Two-way channel between the running task and the dicode orchestrator.

```javascript
// Emit progress (streamed live to WebUI run log)
dicode.progress("Processed 42 of 200", { done: 42, total: 200 })

// Imperatively trigger another task
await dicode.trigger("send-alert", { reason: "spike", value: 99 })

// Check if another task is currently running
const busy = await dicode.isRunning("backup-task")
if (busy) {
  console.warn("backup already running, skipping")
  return
}

// Human approval gate (north star — suspends run)
const ok = await dicode.ask("Deploy to 500 users?", {
  timeout: "1h",
  options: ["approve", "reject"]
})
if (ok !== "approve") return

// Query orchestrator state (primary use: daemon tasks / WebUI task)
const tasks   = await dicode.listTasks()
const spec    = await dicode.getTask("morning-email-check")
const runs    = await dicode.listRuns("morning-email-check", 20)
const run     = await dicode.getRun("run_abc123")
const logs    = await dicode.getRunLogs("run_abc123")
const secrets = await dicode.listSecrets()   // names only, never values
```

See [Task → Orchestrator API](./orchestrator-api.md) for full documentation.

---

### `output` — rich return values

By default, returning a plain object shows a JSON viewer in the WebUI. The `output` global lets tasks return typed content that renders appropriately.

```javascript
// Rendered HTML (sandboxed iframe in WebUI)
return output.html(`
  <h1>Daily Report — ${new Date().toDateString()}</h1>
  <table>
    <tr><td>Emails processed</td><td>${count}</td></tr>
  </table>
`)

// Plain text (monospace block)
return output.text(`Done: ${count} items\n${errors} errors`)

// Image
return output.image("image/png", base64PngData)

// File download trigger in WebUI
return output.file("report.csv", csvContent, "text/csv")

// HTML + structured data: humans see the HTML, chain triggers receive { data }
return output.html(htmlContent, { data: { count, errors } })
```

**Chain compatibility:** when using `output.html(content, { data })`, the `data` field is what chained tasks receive as `input` — not the HTML. One task can produce a human-readable report AND pass structured data downstream.

**WebUI rendering:**

| Output type | Rendered as |
|---|---|
| plain return | Collapsible JSON tree |
| `output.html(...)` | Sandboxed `<iframe>` |
| `output.text(...)` | Monospace `<pre>` block |
| `output.image(...)` | `<img>` tag |
| `output.file(...)` | Download button |

---

### `server` — HTTP serving for daemon tasks (north star)

Available only in daemon tasks (`trigger: { daemon: true }`). Allows a task to serve HTTP — either mounted on the dicode server or on a standalone port.

```javascript
// Mount on dicode's main HTTP server (port 8080)
// Ideal for the WebUI task — replaces the built-in embedded UI
const app = server.mount("/")

app.get("/api/tasks", async (req, res) => {
  res.json(await dicode.listTasks())
})
app.get("/api/runs/:runId", async (req, res) => {
  res.json(await dicode.getRun(req.params.runId))
})
app.post("/api/tasks/:id/run", async (req, res) => {
  const runId = await dicode.trigger(req.params.id, req.body.params)
  res.json({ runId })
})
app.static("/", "./dist")   // serve compiled React/TypeScript bundle

await app.start()           // register routes and block (daemon never returns)

// Or: standalone port (for custom API servers, background services)
server.get("/api/v1/data", async (req, res) => { ... })
await server.listen(9090)
```

**Request:** `req.method`, `req.path`, `req.params`, `req.query`, `req.headers`, `req.body`

**Response:** `res.json(data)`, `res.html(str)`, `res.text(str)`, `res.status(404).json(...)`, `res.header(k, v)`

Not available in MVP — planned post-MVP. See [Web UI & API](./webui-api.md#webui-as-a-daemon-task).

---

## Globals summary

| Global | MVP | Description |
|---|---|---|
| `console` | ✅ | Logging (stdout → info, stderr → error in run log) |
| `env` | ✅ | Resolved secrets / env vars |
| `params` | ✅ | Task parameters |
| `http` | ✅ | Outbound HTTP |
| `kv` | ✅ | Persistent key-value store |
| `output` | ✅ | Typed return values (html, text, image, file) |
| `fs` | ✅ | Filesystem access (only when declared in task.yaml) |
| `input` | ✅ | Chain / webhook payload |
| `notify` | ✅ | Push notifications |
| `dicode` | ✅ | Orchestrator API (progress, trigger, isRunning, query methods) |
| `server` | 🔮 | HTTP serving for daemon tasks (post-MVP) |

---

## Error handling

Unhandled exceptions in `task.js` mark the run as `failed`. The error message and stack trace are captured in the run log.

```javascript
// Good: explicit error handling
try {
  const res = await http.get("https://api.example.com/data")
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return { data: res.body }
} catch (err) {
  console.error("request failed", err.message)
  throw err   // re-throw to mark run as failed
}
```

Use `console.error()` to record context before re-throwing. The final uncaught exception is always recorded regardless.

---

## Execution model

1. A fresh `goja.Runtime` is created for each run
2. All globals are injected
3. The task's `env` keys are resolved from the secrets chain and injected into `env`
4. `task.js` is compiled and executed
5. The return value (if any) is captured, JSON-serialized, and stored
6. Chained tasks receive this value as their `input` global

There is no shared state between runs. `kv` is the only cross-run persistence mechanism.

---

## Limitations

- No `import`/`require` — all libraries must be implemented via the injected globals or pure JS
- No `setTimeout`/`setInterval` — scheduling is done via `task.yaml` triggers
- No custom log levels beyond stdout/stderr — use `console.log`/`console.error` etc.
- Output cap for chain triggers: 1MB. Large data should be stored in `kv` and referenced by key.
- CPU timeout: configurable (default: 30s per run). Long-running tasks should use `dicode.progress()` to signal activity.
