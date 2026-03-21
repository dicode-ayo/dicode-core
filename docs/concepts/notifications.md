# Notifications

Dicode delivers notifications through three channels: **mobile push**, **OS desktop notifications**, and the **system tray icon**. All three are optional and work together.

---

## Mobile push

Sends push notifications to your phone or any HTTP-capable endpoint.

```yaml
notifications:
  on_failure: true
  on_success: false
  provider:
    type: ntfy
    url: https://ntfy.sh
    topic: my-dicode-alerts
    token_env: NTFY_TOKEN   # optional, for authenticated topics
```

**Supported providers:**

| Type | Description | License |
|---|---|---|
| `ntfy` | ntfy.sh or self-hosted ntfy. Simple HTTP API, free tier available. | Apache 2.0 |
| `gotify` | Self-hosted Gotify server. | MIT |
| `pushover` | Pushover mobile app ($5 one-time). | Commercial |
| `telegram` | Telegram bot message. | — |

### ntfy

```yaml
provider:
  type: ntfy
  url: https://ntfy.sh      # or your self-hosted URL
  topic: my-dicode-alerts
  token_env: NTFY_TOKEN     # optional: for access-controlled topics
```

Subscribe on your phone by opening `https://ntfy.sh/my-dicode-alerts` in the ntfy app or browser.

### gotify

```yaml
provider:
  type: gotify
  url: https://gotify.example.com
  token_env: GOTIFY_APP_TOKEN
```

### Automatic notifications

Set `on_failure: true` / `on_success: true` at the top level to get automatic notifications for all tasks. You can override per-task (future feature).

---

## Task-level notifications

From inside `task.js`:

```javascript
// Low-priority informational notification
await notify.send("Email digest sent", {
  priority: "low",
  tags: ["email", "digest"]
})

// Urgent alert
await notify.send("API is DOWN — 5 failures in 10 minutes", {
  priority: "urgent",
  tags: ["alert", "warning"]
})
```

**Options:**

| Field | Type | Default | Description |
|---|---|---|---|
| `priority` | string | `"default"` | `min`, `low`, `default`, `high`, `urgent` |
| `tags` | array | `[]` | Tag strings. ntfy maps these to emoji. |
| `actions` | array | `[]` | Action buttons (see below) |

**Action buttons** (provider-dependent):
```javascript
await notify.send("Deploy ready", {
  priority: "high",
  actions: [
    { label: "Deploy", url: "https://deploy.example.com/run" },
    { label: "Skip", url: "https://deploy.example.com/skip" }
  ]
})
```

If no provider is configured, `notify.send()` is a no-op — no error is thrown.

---

## OS desktop notifications

When dicode is running on a desktop machine, it automatically fires OS-native notifications alongside mobile push:

- **Linux**: D-Bus libnotify (`notify-send` equivalent)
- **macOS**: `NSUserNotification` / `UNUserNotificationCenter`
- **Windows**: Toast notifications

No configuration needed — desktop notifications are automatic when a display is detected. Disable with `notifications.desktop: false`.

---

## System tray icon

The tray icon provides ambient status and quick access to common actions.

**Enable:**
```yaml
server:
  tray: true
```
Or: `dicode --tray`

**Status indicator:**
- 🟢 Green — all recent runs successful (or no runs yet)
- 🟡 Yellow — a task is currently running
- 🔴 Red — the most recent run failed

The icon updates in real time as tasks run and complete.

**Right-click menu:**
```
Open Web UI
────────────
Run task ▶
  ├── morning-email-check
  ├── daily-backup
  └── ...
────────────
Last run: morning-email-check ✅ 2 min ago
────────────
Pause reconciler
────────────
Quit dicode
```

**Left-click:** opens `http://localhost:{port}` in the default browser.

**Platform support:** Linux (X11/Wayland via `systray`), macOS, Windows. Tray icon is compiled conditionally — headless builds (no display) skip it entirely.

---

## Approval gates (north star)

Future feature: a task can suspend itself and send an actionable notification requesting human approval before continuing.

```javascript
const decision = await dicode.ask("Deploy to 500 production users?", {
  timeout: "30m",
  options: ["approve", "reject"]
})
if (decision !== "approve") {
  log.info("Deployment rejected or timed out")
  return
}
// proceed with deployment
```

The run is stored as `suspended` in sqlite. When the user taps "approve" in the notification (or via the WebUI), the run resumes from where it paused. If the timeout expires, the run resumes with `null`.

This enables human-in-the-loop AI workflows: an agent task proposes an action, waits for human approval, then executes it.

See [Task → Orchestrator API](./orchestrator-api.md) for the full `dicode.ask()` specification.
