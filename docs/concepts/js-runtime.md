# JS Runtime

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

### `log` — structured logging

```javascript
log.debug("message")
log.info("message", { key: value })
log.warn("message")
log.error("message", { err: error.message })
```

All log calls are captured in the run log (visible in the WebUI and via `dicode task run --verbose`). They are also forwarded to the dicode process logger at the corresponding level.

The second argument (optional) is a plain object — its fields are added as structured context fields.

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
log.info(`Processing ${input.count} items`)
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
  log.warn("backup already running, skipping")
  return
}

// Human approval gate (north star — suspends run)
const ok = await dicode.ask("Deploy to 500 users?", {
  timeout: "1h",
  options: ["approve", "reject"]
})
if (ok !== "approve") return
```

See [Task → Orchestrator API](./orchestrator-api.md) for full documentation.

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
  log.error("request failed", { err: err.message })
  throw err   // re-throw to mark run as failed
}
```

Use `log.error()` to record context before re-throwing. The final uncaught exception is always recorded regardless.

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
- No `console.log` — use `log.info()` instead
- Output cap for chain triggers: 1MB. Large data should be stored in `kv` and referenced by key.
- CPU timeout: configurable (default: 30s per run). Long-running tasks should use `dicode.progress()` to signal activity.
