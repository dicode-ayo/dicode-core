# Web UI & API

Dicode includes a built-in web interface and REST API. The UI is served by the dicode process itself — no separate web server needed. All frontend assets are embedded in the binary.

---

## Accessing the UI

```
http://localhost:8080
```

Configure the port in `dicode.yaml`:
```yaml
server:
  port: 8080
```

---

## UI pages

### Task list (`/`)

- All registered tasks with their source, trigger type, and last run status
- Status badges: ✅ success / 🔴 failed / 🟡 running / ⚪ never run
- Click a task to open its detail page

### Task detail (`/tasks/{id}`)

- Task metadata (name, trigger, source)
- `task.yaml` and `task.js` viewer
- Manual trigger button (with parameter override inputs)
- Run history table (last 50 runs)

### Run detail (`/runs/{runID}`)

- Run metadata (task, trigger type, start time, duration, status)
- Live log viewer — logs stream via Server-Sent Events while the run is in progress
- Return value (for completed runs)
- Parent/child run links (for chained runs)

### AI Generate (`/generate`)

- Plain-language prompt input
- Generated diff view (task.yaml / task.js / task.test.js)
- Edit-and-re-validate inline
- Confirm to write to local source

### Secrets (`/secrets`)

- List of registered secret names
- Add / delete secrets (values never shown in the UI)

---

## Frontend

The UI is a single-page application (SPA) built with [Lit](https://lit.dev) web components. No npm build step — all files are plain ESM modules loaded directly by the browser.

All frontend assets are embedded in the binary via `//go:embed static` and served under `/app/*`. The entry point is `static/app/index.html`, which loads `app.js` as an ES module. A catch-all route (`/*`) serves `index.html` for all unmatched paths so client-side navigation works on reload.

### Components

| File | Component | Description |
|---|---|---|
| `components/dc-task-list.js` | `<dc-task-list>` | Task list page |
| `components/dc-task-detail.js` | `<dc-task-detail>` | Task detail, file viewer, run history |
| `components/dc-run-detail.js` | `<dc-run-detail>` | Run detail, live log viewer |
| `components/dc-config.js` | `<dc-config>` | Config editor |
| `components/dc-secrets.js` | `<dc-secrets>` | Secrets manager |
| `components/dc-log-bar.js` | `<dc-log-bar>` | Global log bar (bottom of every page) |
| `components/dc-notif-panel.js` | `<dc-notif-panel>` | Notification panel |

### Client-side routing

`lib/router.js` matches `location.pathname` against regex routes and mounts the appropriate Lit component into `<div id="app">`. Navigation uses `history.pushState` — no full page reloads.

### Real-time: WebSocket

All real-time data flows over a single persistent WebSocket at `/ws`:

- On connect, the client sends `{ type: "sub:logs" }` to subscribe to log lines
- The server pushes log lines, run status changes, and task registration events as JSON messages
- `lib/ws.js` handles connect, dispatch by message type, and auto-reconnect (3 s backoff)

The old SSE endpoint (`/api/runs/{runID}/logs/stream`) is still available for direct log streaming.

> **Development note:** static files are embedded in the binary via `//go:embed static`. Changes to frontend files require a binary rebuild (`make run`) to take effect.

---

## REST API

All API responses are JSON.

### Tasks

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/tasks` | List all tasks |
| `GET` | `/api/tasks/{id}` | Get task detail (spec + last run) |
| `POST` | `/api/tasks/{id}/run` | Manual trigger |
| `GET` | `/api/tasks/{id}/runs` | Run history (last 50) |

**POST `/api/tasks/{id}/run`** — trigger with optional param overrides:
```json
{
  "params": {
    "slack_channel": "#ops",
    "max_emails": "5"
  }
}
```

Response:
```json
{ "run_id": "run_abc123" }
```

### Runs

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/runs/{runID}` | Run detail |
| `GET` | `/api/runs/{runID}/logs` | Run logs (completed) |
| `GET` | `/api/runs/{runID}/logs/stream` | Run logs (SSE, live) |

**GET `/api/runs/{runID}`** response:
```json
{
  "id": "run_abc123",
  "task_id": "morning-email-check",
  "status": "success",
  "trigger": "cron",
  "started_at": "2026-03-22T08:00:01Z",
  "finished_at": "2026-03-22T08:00:04Z",
  "duration_ms": 3241,
  "return_value": { "count": 5 },
  "parent_run_id": null
}
```

### Webhooks

| Method | Path | Description |
|---|---|---|
| `POST` | `/hooks/{path}` | Trigger a webhook task |

The path matches `trigger.webhook` in `task.yaml`. The POST body is available as `input` in the task.

### Secrets

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/secrets` | List secret names (never values) |
| `POST` | `/api/secrets` | Set a secret |
| `DELETE` | `/api/secrets/{key}` | Delete a secret |

**POST `/api/secrets`**:
```json
{ "key": "SLACK_TOKEN", "value": "xoxb-..." }
```

### AI generation

| Method | Path | Description |
|---|---|---|
| `POST` | `/api/generate` | Generate task from prompt |

**POST `/api/generate`**:
```json
{ "prompt": "Check the Stripe status page every 15 minutes and alert if degraded" }
```

Response (streaming JSON lines):
```jsonl
{ "type": "progress", "message": "Generating task..." }
{ "type": "result", "files": { "task.yaml": "...", "task.js": "...", "task.test.js": "..." } }
```

**POST `/api/generate/confirm`** — write generated files to local source:
```json
{
  "task_id": "stripe-status-monitor",
  "files": { "task.yaml": "...", "task.js": "...", "task.test.js": "..." }
}
```

### MCP

| Path | Description |
|---|---|
| `/mcp` | MCP server endpoint (see [MCP Server](./mcp-server.md)) |

---

---

## WebUI as a daemon task (north star)

The embedded Go WebUI is the MVP. The north star is a **WebUI daemon task** — a long-running task that serves a TypeScript/React frontend using the `server` global and `dicode` query methods.

This decouples the UI entirely from the binary:
- The UI can be developed in a separate repo (TypeScript, React, any framework)
- It is versioned and updated independently of the dicode binary
- Community-built alternative UIs are possible
- The dicode binary becomes purely an orchestrator — no UI opinion baked in

### Architecture

```
github.com/dicode/webui/        ← separate TypeScript/React repo
  src/                           ← source (not committed to task folder)
  dist/                          ← compiled bundle (released as GitHub artifact)

your-tasks/webui/                ← the dicode task
  task.yaml                      ← trigger: daemon: true
  task.js                        ← server setup, API routes, dicode queries
  dist/                          ← compiled bundle (copied from webui release)
```

### task.yaml

```yaml
name: Dicode WebUI
trigger:
  daemon: true
  restart: always
params:
  - name: port
    default: "8080"
```

### task.js

```javascript
const app = server.mount("/")

// API routes backed by dicode query methods
app.get("/api/tasks", async (req, res) => {
  res.json(await dicode.listTasks())
})
app.get("/api/tasks/:id", async (req, res) => {
  res.json(await dicode.getTask(req.params.id))
})
app.post("/api/tasks/:id/run", async (req, res) => {
  const runId = await dicode.trigger(req.params.id, req.body?.params)
  res.json({ runId })
})
app.get("/api/runs/:runId", async (req, res) => {
  res.json(await dicode.getRun(req.params.runId))
})
app.get("/api/runs/:runId/logs", async (req, res) => {
  res.json(await dicode.getRunLogs(req.params.runId))
})

// Serve the compiled React bundle for everything else
app.static("/", "./dist")

await app.start()
```

### Bootstrap

If no daemon task has mounted `/`, the dicode binary serves a minimal bootstrap page:

```
Dicode is running.

No WebUI task is installed. To install the default WebUI:
  dicode task install github.com/dicode/webui

The REST API is available at /api/
```

The REST API is always served by the binary regardless of whether a WebUI task is running.

### Separate repo development

The TypeScript/React source lives in a separate repo. To update the UI:
1. Make changes in the React repo, build (`npm run build` → `dist/`)
2. Copy `dist/` to your tasks `webui/dist/`
3. The local source detects the change, reloads the daemon task (~100ms)
4. The new UI is live immediately — no dicode restart

Or: the task fetches the latest release bundle from GitHub on startup and caches it in `kv`. On restart it checks for a newer release.

This is a post-MVP feature requiring the `server` global and daemon task type. See [Implementation Plan](../implementation-plan.md) for the milestone order.

---

## API authentication

The REST API has no authentication by default (localhost only). For remote access, set a shared secret:

```yaml
server:
  api_secret_env: DICODE_API_SECRET
```

Include it as a header: `Authorization: Bearer <secret>`.

---

## API reference

Full OpenAPI spec available at `http://localhost:8080/api/openapi.json` when dicode is running.
