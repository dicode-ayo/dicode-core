# Notifications & Interactive Tasks — Design

## Overview

Tasks need to communicate results and request user input. This document covers
the full notification stack: passive result notifications, a persistent
notification inbox, and interactive task pausing for confirmations and OAuth.

**Guiding principle:** the browser is the notification surface. No OS
integration, no relay service. A lightweight Service Worker keeps notifications
alive even when the tab is backgrounded.

---

## Phase 1 — Run detail page

Every notification needs a destination. Before anything else, build
`/ui/runs/{runID}`.

### What it shows

| Runtime | Content |
|---|---|
| Deno / Python | Logs + return value (JSON viewer, or rendered rich output) |
| Docker / Podman | Logs only |
| Any | Status badge, task name, trigger type, start time, duration |

### Rich output rendering

The `output` global lets tasks return typed content. The run detail page
renders each type:

| Type | Render |
|---|---|
| `return { … }` / `result = …` | Collapsible JSON viewer |
| `output.html(…)` | Sandboxed `<iframe>` |
| `output.text(…)` | Monospace `<pre>` block |
| `output.image(mime, data)` | `<img>` tag |
| `output.file(name, data, mime)` | Download button |

### Example task — rich output

```javascript
// examples/daily-summary/task.ts
const rows = await fetchRows()

return output.html(`
  <h2>Daily Summary</h2>
  <p>${rows.length} rows processed.</p>
  <table>
    ${rows.map(r => `<tr><td>${r.id}</td><td>${r.status}</td></tr>`).join('')}
  </table>
`, { data: { count: rows.length } })
```

The run detail page renders the HTML in a sandboxed iframe. A chained task
receives `{ count: rows.length }` via `input`.

### Progress checklist

- [ ] `GET /ui/runs/{runID}` handler + template
- [ ] Log display (poll or SSE stream for live runs)
- [ ] JSON viewer for return value
- [ ] Rich output rendering (html / text / image / file)
- [ ] Link from task page run history rows → run detail

---

## Phase 2 — Browser notifications

### Permission flow

On first visit to the web UI, show a one-time banner:

> **dicode** would like to send notifications when tasks complete.
> [Allow] [Not now]

Clicking Allow calls `Notification.requestPermission()`. Permission state is
stored in `localStorage` and re-checked on each page load.

### Service Worker

A minimal Service Worker (`/sw.js`) enables notifications when the tab is
backgrounded or closed.

```
GET /sw.js          — served by dicode web server
GET /sw-client.js   — page-side registration helper
```

The SW listens for `push` events (or `message` events from the page) and calls
`self.registration.showNotification(…)`.

For local use (no push server), the page itself posts a message to the SW when
it receives a run-complete SSE event. For future headless/server deployments,
a Web Push endpoint can be added without changing the task or SW contract.

### Notification content

```
[dicode] morning-email-check ✓
Processed 12 emails — 00:03
```

Click → opens `/ui/runs/{runID}` (SW handles `notificationclick`).

For failures:

```
[dicode] backup-database ✗
exited with code 1 — 00:12
```

### SSE run-complete event

The web server already streams logs via SSE. Extend the existing event stream
with a `run:complete` event:

```
event: run:complete
data: {"runID":"…","taskID":"…","status":"success","durationMs":3210}
```

The page JS receives this, posts to the SW, which fires the notification.

### Progress checklist

- [ ] `GET /sw.js` route + minimal Service Worker
- [ ] `GET /sw-client.js` registration helper included in base template
- [ ] Permission banner in web UI
- [ ] `run:complete` SSE event from server
- [ ] Page JS → SW message → `showNotification`
- [ ] SW `notificationclick` → open/focus run detail page

---

## Phase 3 — Notification inbox

A persistent bell icon in the UI header shows recent completions and **pending
interactions** (Phase 4). Passive for now; interactive later.

```
🔔 3   ← badge count
```

Clicking opens a slide-in panel:

```
┌─────────────────────────────────┐
│ Notifications              Clear│
├─────────────────────────────────┤
│ ✓ morning-email-check   3m ago  │
│   12 emails processed    [View] │
├─────────────────────────────────┤
│ ✗ backup-database       12m ago │
│   exited with code 1     [View] │
└─────────────────────────────────┘
```

Entries are kept in `localStorage` (last 50). The inbox is populated from the
same `run:complete` SSE stream as the browser notifications.

### Progress checklist

- [ ] Bell icon + badge in base template header
- [ ] Slide-in inbox panel (HTMX or vanilla JS)
- [ ] Populate from `run:complete` SSE
- [ ] Persist to `localStorage`
- [ ] [View] links to run detail page
- [ ] Clear button

---

## Phase 4 — Interactive task pausing

Tasks can pause mid-execution to request user input. The mechanism reuses the
existing Unix socket bridge (same protocol as `log`, `kv`, etc.).

### New socket methods

| Method | Direction | Description |
|---|---|---|
| `interact.confirm` | task → server | Ask user to confirm or cancel |
| `interact.prompt` | task → server | Ask user for a text value |
| `interact.notify` | task → server | Fire-and-forget notification (no response needed) |

All three are **request/response** — the task blocks until the user responds
or the interaction times out.

### SDK globals

**Deno / TypeScript:**

```typescript
// Pause until user confirms or cancels. Returns true/false.
const ok = await interact.confirm("Delete 500 emails?")
if (!ok) return { cancelled: true }

// Pause until user submits a value.
const label = await interact.prompt("Enter report label:", "Weekly")

// Fire a notification without pausing.
interact.notify("Backup started — estimated 5 minutes")
```

**Python:**

```python
ok = interact.confirm("Delete 500 emails?")
if not ok:
    result = {"cancelled": True}
else:
    label = interact.prompt("Enter report label:", "Weekly")
```

### Server-side flow

1. Task sends `interact.confirm` request (with ID) over socket — blocks.
2. Server stores a `PendingInteraction{id, taskID, runID, type, message}` in memory.
3. Server broadcasts an SSE event `interact:pending` to all open UI connections.
4. Browser notification fires: **"morning-email-check needs your input"** → click opens run detail.
5. Run detail page shows an interaction card:

```
┌──────────────────────────────────┐
│ ⏸  Task is waiting               │
│                                  │
│  Delete 500 emails?              │
│                                  │
│  [Confirm]          [Cancel]     │
└──────────────────────────────────┘
```

6. User clicks Confirm → `POST /api/interact/{id}/respond` `{value: true}`.
7. Server sends response back over the socket → task unblocks and continues.

### Timeout

Interactions expire after a configurable timeout (default 10 minutes). On
expiry the task receives `false` / `null` and an `interact.timedOut` error is
set so the task can handle it gracefully.

### Example task — confirm before destructive action

```typescript
// examples/cleanup-old-runs/task.ts
const runs = await getOldRuns(30) // older than 30 days

log.info(`Found ${runs.length} runs to delete`)

const ok = await interact.confirm(
  `Delete ${runs.length} runs older than 30 days?`
)
if (!ok) {
  log.info("Cancelled by user")
  return { deleted: 0 }
}

for (const r of runs) {
  await deleteRun(r.id)
}

log.info(`Deleted ${runs.length} runs`)
return { deleted: runs.length }
```

### Progress checklist

- [ ] `PendingInteraction` store in server (in-memory map, keyed by interaction ID)
- [ ] `interact.*` socket methods in deno server
- [ ] `interact` global in Deno SDK shim (`sdk/shim.js`)
- [ ] `interact` global in Python SDK shim (`sdk/sdk.py`)
- [ ] `interact:pending` SSE event
- [ ] Interaction card on run detail page
- [ ] `POST /api/interact/{id}/respond` endpoint
- [ ] Interaction timeout + task-side error handling
- [ ] Inbox badge increment for pending interactions
- [ ] Example task: `examples/cleanup-old-runs/`

---

## Phase 5 — OAuth helper

A higher-level wrapper over Phase 4 for the common OAuth 2.0 PKCE flow.

```typescript
// examples/connect-gmail/task.ts
// First run: pauses and asks user to authorize. Token cached in kv.
const token = await oauth.authorize("google", [
  "https://www.googleapis.com/auth/gmail.readonly"
])

const res = await http.get("https://gmail.googleapis.com/gmail/v1/users/me/profile", {
  headers: { Authorization: `Bearer ${token.access_token}` }
})
log.info(`Logged in as ${res.body.emailAddress}`)
```

### Flow

1. Task calls `oauth.authorize(provider, scopes)` — checks `kv` for a cached
   non-expired token first.
2. If no cached token: server generates PKCE code verifier + challenge, builds
   the authorization URL, pauses the task via `interact`.
3. Notification fires: **"connect-gmail needs authorization"** — click opens a
   modal with an [Open Authorization Page] button.
4. User authorizes in new tab → OAuth redirect hits `GET /oauth/callback`.
5. Server exchanges code for tokens, stores in task `kv`, unblocks the task.
6. `oauth.authorize` returns the token object. Subsequent calls return the
   cached token (refreshing automatically if expired).

### Built-in providers

| Key | Provider |
|---|---|
| `google` | Google OAuth 2.0 |
| `github` | GitHub OAuth App |
| `slack` | Slack OAuth 2.0 |
| `custom` | Any provider (pass `authUrl`, `tokenUrl`, `clientId`) |

### Progress checklist

- [ ] `oauth.authorize(provider, scopes)` in Deno SDK
- [ ] PKCE flow in Go server
- [ ] `GET /oauth/callback` handler
- [ ] Provider config in `dicode.yaml` (`oauth.providers.*`)
- [ ] Token refresh logic
- [ ] `oauth` global in Python SDK
- [ ] Example task: `examples/connect-gmail/`
- [ ] Docs: `docs/oauth.md`

---

## Cross-cutting concerns

### Headless mode

Browser notifications require a browser. In headless/server deployments:
- Phase 1 (run detail page) — always available via the web UI URL
- Phase 2 (browser notifications) — not applicable; skip gracefully
- Phase 3 (inbox) — available in the web UI
- Phase 4 (interact) — interactions time out; tasks should handle `timedOut`
  and fall back to a default action or fail with a clear message

A future `notify:` config section could route notifications to ntfy, Slack, or
email in headless mode.

### Security

- Interaction responses require the same session auth as the rest of the web UI
- OAuth client secrets live in `dicode.yaml` under `oauth.providers` (not in task files)
- Interaction IDs are UUIDs — not guessable

---

## Implementation order

```
Phase 1  Run detail page                    ← start here
Phase 2  Browser notifications + SW
Phase 3  Notification inbox
Phase 4  Interactive task pausing
Phase 5  OAuth helper
```

Each phase is independently shippable and useful without the next.
