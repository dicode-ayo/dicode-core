# Web UI & API

Dicode includes a built-in web interface and REST API. The UI is served by the dicode process itself — no separate web server needed.

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

### Task-contributed nav links

Any task can add itself to the header `<nav>` next to Tasks/Sources/Config/Secrets/Security/Metrics by setting a `webui.nav` block in its `task.yaml` (see [Task Format — `webui.nav`](task-format.md#webui-navigation-webuinav)). The `<dc-nav>` component reads `GET /api/tasks`, picks entries with `webui.nav.label` set, and renders one link per entry pointing at that task's own `trigger.webhook` path — a plain full-page navigation into the task's self-contained SPA (e.g. `buildin/auth-providers` adds an "Auth Providers" link this way). No source change to `pkg/webui` is needed: `GET /api/tasks` embeds the full task spec, so a new `webui` field on `task.Spec` is automatically surfaced in the JSON response.

---

## Frontend

The UI is a single-page application (SPA) built with [Lit](https://lit.dev) web components. No npm build step — all files are plain ESM modules loaded directly by the browser.

The SPA is itself a dicode task — `buildin/webui` (`tasks/buildin/webui/`, entry point `index.html` loading `app/app.js` as an ES module, components under `app/components/`, routing/websocket helpers under `app/lib/`) — served at the `/hooks/webui` webhook path like any other webhook task. Because it's a task, the reconciler hot-reloads it on change, same as any task in a watched source; no binary rebuild is needed. `pkg/webui`'s own embedded static assets are limited to `dicode.js` and `dicode-oauth-broadcast.js` (the client SDK injected into webhook-served pages), not the SPA itself.

For the component-level architecture (light-DOM app components vs. the Shadow-DOM `dc-card`/`dc-table`/etc. primitives, and the `DcElement` base class), see [WebUI frontend components](webui-components.md).

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
| `components/dc-nav.js` | `<dc-nav>` | Task-contributed header nav links (`webui.nav`) |

### Client-side routing

`lib/router.js` matches `location.pathname` against regex routes and mounts the appropriate Lit component into `<div id="app">`. Navigation uses `history.pushState` — no full page reloads.

### Real-time: WebSocket

All real-time data flows over a single persistent WebSocket at `/ws`:

- On connect, the client sends `{ type: "sub:logs" }` to subscribe to log lines
- The server pushes log lines, run status changes, and task registration events as JSON messages
- `lib/ws.js` handles connect, dispatch by message type, and auto-reconnect (3s backoff)

> **Development note:** the frontend is a hot-reloaded task, not embedded in the binary — changes to files under `tasks/buildin/webui/` take effect on the next reconciler pass, no rebuild required.

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
| `GET` | `/runs/{runID}/result` | Bare page: raw output content (or JSON return value) for a completed run, no chrome |
| `POST` | `/api/runs/{runID}/kill` | Kill a running task |
| `GET` | `/api/audit` | Security audit log — filter by `task_id`, `actor`, `event_type`; paginate with `limit` (≤1000, default 100) + `offset`. Newest first. |

Live log streaming while a run is in progress goes over the `/ws` WebSocket (see [Real-time: WebSocket](#real-time-websocket) above), not a per-run SSE endpoint.

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

**GET `/api/audit`** response (`{events, count}`; each event is sanitized — `params` never contains secret values):

```json
{
  "events": [
    {
      "id": "5f1c…",
      "ts": "2026-06-12T18:40:00Z",
      "event_type": "denied",
      "actor_kind": "ip",
      "actor_id": "203.0.113.9",
      "target_kind": "endpoint",
      "target_id": "GET /api/tasks",
      "allowed": false,
      "reason": "no valid session"
    }
  ],
  "count": 1
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

### AI authoring

Session-based AI authoring endpoints for creating and editing tasks via an interactive workflow.

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/api/task/create` | Scaffold a new task with boilerplate |
| `POST` | `/api/task/edit` | Create or resume an AI authoring session |
| `POST` | `/api/task/save` | Apply authored changes |
| `POST` | `/api/task/cancel` | Discard an authoring session |

**POST `/api/task/create`**:

```json
{ "name": "my-task", "source": "default" }
```

Response: `201`

```json
{ "task_id": "my-task", "source": "default", "files": ["task.yaml", "task.ts"] }
```

**POST `/api/task/edit`** — create a new session or resume an existing one:

```json
{ "task_id": "my-task", "prompt": "Add a cron trigger that runs every hour" }
```

Or resume:

```json
{ "session_id": "sess_abc123", "prompt": "Also add a slack notification" }
```

Response: `202`

```json
{ "session_id": "sess_abc123", "sandbox_path": "/tmp/dicode-edit-abc", "source": "default", "source_kind": "local" }
```

**POST `/api/task/save`**:

```json
{ "session_id": "sess_abc123" }
```

Response:

```json
{ "applied": true, "pr_url": "https://github.com/org/repo/pull/42" }
```

**POST `/api/task/cancel`**:

```json
{ "session_id": "sess_abc123" }
```

Response:

```json
{ "cancelled": true }
```

### MCP

| Path | Description |
| --- | --- |
| `POST /mcp` | MCP JSON-RPC 2.0 endpoint (see [MCP Server](./mcp-server.md)) |
| `GET /mcp` | MCP server info |

---

## API authentication

The REST API has no authentication by default (localhost only). Enable it with:

```yaml
server:
  auth: true
```

Two credential kinds cover the two kinds of callers:

- **Browser access** — the login session/device-token flow (Phase 2 in [security.md](security.md)): sign in once, get a session cookie plus a long-lived trusted-device cookie.
- **Programmatic access** (the `/mcp` endpoint, CI scripts, automation) — a `dck_` API key sent as `Authorization: Bearer dck_<key>` (Phase 4 in [security.md](security.md)).

See [security.md](security.md) for the full trust model.
