# Web UI & API

Dicode includes a built-in web interface and REST API. The UI is served by the dicode process itself — no separate web server needed. All frontend assets are embedded in the binary.

---

## Accessing the UI

```text
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
- Status badges: success / failed / running / never run
- Tasks grouped by namespace when TaskSet sources are configured (e.g. `infra/deploy-backend` → "infra" section)
- Click a task to open its detail page

### Task detail (`/tasks/{id}`)

- Task metadata (name, trigger, source)
- `task.yaml` and `task.js` viewer
- Manual trigger button (with parameter override inputs)
- Run history table (last 50 runs)

### Run detail (`/runs/{runID}`)

- Run metadata (task, trigger type, start time, duration, status)
- Live log viewer — logs stream via WebSocket while the run is in progress
- Return value (for completed runs)
- Parent/child run links (for chained runs)

### Secrets (`/secrets`)

- List of registered secret names
- Add / delete secrets (values never shown in the UI)

### Sources (`/sources`)

- All configured sources with type badge (local/git), path/URL, branch
- Dev mode toggle per TaskSet source — enter a local path and enable to swap the source root immediately
- Branch picker for git sources (lazy-loaded from `/api/sources/:name/branches`)
- Status messages auto-clear after 3s

### Config (`/config`)

- Edit `dicode.yaml` directly in Monaco editor

---

## Frontend

The UI is a single-page application (SPA) built with [Lit](https://lit.dev) web components. No npm build step — all files are plain ESM modules loaded directly by the browser.

All frontend assets are embedded in the binary via `//go:embed static` and served under `/app/*`. The entry point is `static/app/index.html`, which loads `app.js` as an ES module. A catch-all route (`/*`) serves `index.html` for all unmatched paths so client-side navigation works on reload.

### Components

| File | Component | Description |
| --- | --- | --- |
| `components/dc-task-list.js` | `<dc-task-list>` | Task list page (namespace-grouped) |
| `components/dc-task-detail.js` | `<dc-task-detail>` | Task detail, file viewer, run history |
| `components/dc-run-detail.js` | `<dc-run-detail>` | Run detail, live log viewer |
| `components/dc-config.js` | `<dc-config>` | Config editor (Monaco) |
| `components/dc-secrets.js` | `<dc-secrets>` | Secrets manager |
| `components/dc-sources.js` | `<dc-sources>` | Sources manager with dev mode toggle |
| `components/dc-log-bar.js` | `<dc-log-bar>` | Global log bar (bottom of every page) |
| `components/dc-notif-panel.js` | `<dc-notif-panel>` | Notification panel |

### Client-side routing

`lib/router.js` matches `location.pathname` against regex routes and mounts the appropriate Lit component into `<div id="app">`. Navigation uses `history.pushState` — no full page reloads.

### Real-time: WebSocket

All real-time data flows over a single persistent WebSocket at `/ws`:

- On connect, the client sends `{ type: "sub:logs" }` to subscribe to log lines
- The server pushes log lines, run status changes, and task registration events as JSON messages
- `lib/ws.js` handles connect, dispatch by message type, and auto-reconnect (3s backoff)

> **Development note:** static files are embedded in the binary via `//go:embed static`. Changes to frontend files require a binary rebuild to take effect.

---

## REST API

All API responses are JSON.

### Tasks

| Method | Path | Description |
| --- | --- | --- |
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
| --- | --- | --- |
| `GET` | `/api/runs/{runID}` | Run detail |
| `GET` | `/api/runs/{runID}/logs` | Run logs (completed) |
| `GET` | `/api/runs/{runID}/logs/stream` | Run logs (SSE, live) |
| `POST` | `/api/runs/{runID}/kill` | Kill a running task |

**GET `/api/runs/{runID}`** response:

```json
{
  "id": "run_abc123",
  "task_id": "infra/deploy-backend",
  "status": "success",
  "trigger": "manual",
  "started_at": "2026-03-29T10:00:01Z",
  "finished_at": "2026-03-29T10:00:04Z",
  "duration_ms": 3241,
  "return_value": { "count": 5 },
  "parent_run_id": null
}
```

### Webhooks

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/hooks/{path}` | Trigger a webhook task |

The path matches `trigger.webhook` in `task.yaml`. The POST body is available as `input` in the task.

### Secrets

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/api/secrets` | List secret names (never values) |
| `POST` | `/api/secrets` | Set a secret |
| `DELETE` | `/api/secrets/{key}` | Delete a secret |

**POST `/api/secrets`**:

```json
{ "key": "SLACK_TOKEN", "value": "xoxb-..." }
```

### Sources

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/api/sources` | List all sources with dev mode state |
| `PATCH` | `/api/sources/{name}/dev` | Enable/disable dev mode for a TaskSet source |
| `GET` | `/api/sources/{name}/branches` | List available git branches |

**PATCH `/api/sources/{name}/dev`**:

```json
{ "enabled": true, "local_path": "/tmp/dev-tasks/taskset.yaml" }
```

Response:

```json
{
  "name": "infra",
  "type": "local",
  "dev_mode": true,
  "dev_path": "/tmp/dev-tasks/taskset.yaml"
}
```

### AI generation

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/api/generate` | Generate task from prompt |
| `POST` | `/api/generate/confirm` | Write generated files to local source |

**POST `/api/generate`**:

```json
{ "prompt": "Check the Stripe status page every 15 minutes and alert if degraded" }
```

Response (streaming JSON lines):

```jsonl
{ "type": "progress", "message": "Generating task..." }
{ "type": "result", "files": { "task.yaml": "...", "task.js": "...", "task.test.js": "..." } }
```

### MCP

| Path | Description |
| --- | --- |
| `POST /mcp` | MCP JSON-RPC 2.0 endpoint (see [MCP Server](./mcp-server.md)) |
| `GET /mcp` | MCP server info |

---

## API authentication

The REST API has no authentication by default (localhost only). For remote access, set a shared secret:

```yaml
server:
  api_secret_env: DICODE_API_SECRET
```

Include it as a header: `Authorization: Bearer <secret>`.
