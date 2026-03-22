# Task Format

Every dicode task is a folder containing up to three files:

```
tasks/
└── morning-email-check/
    ├── task.yaml       ← required: trigger, params, env, metadata
    ├── task.js         ← required: JavaScript logic
    └── task.test.js    ← optional: unit tests
```

The folder name is the task ID. It must be unique across all sources.

---

## `task.yaml`

### Minimal example

```yaml
name: Morning Email Check
trigger:
  cron: "0 8 * * *"
```

### Full example

```yaml
name: Morning Email Digest
description: Fetches unread emails and posts a summary to Slack
runtime: js

trigger:
  cron: "0 8 * * 1-5"   # weekdays at 8am

params:
  - name: slack_channel
    description: Slack channel to post digest
    default: "#general"
  - name: max_emails
    description: Maximum emails to include
    default: "20"

env:
  - GMAIL_TOKEN
  - SLACK_TOKEN
```

### All fields

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | ✅ | Human-readable task name |
| `description` | string | | One-line description |
| `runtime` | string | | `js` (default, only option currently) |
| `trigger` | object | ✅ | Exactly one trigger must be set |
| `trigger.cron` | string | | Standard cron expression (5 fields) |
| `trigger.webhook` | string | | Webhook path, e.g. `/github-push` |
| `trigger.manual` | bool | | Set `true` to enable manual-only |
| `trigger.chain` | object | | Chain trigger (see below) |
| `trigger.chain.from` | string | | Task ID to listen for |
| `trigger.chain.on` | string | | `success` (default), `failure`, `always` |
| `trigger.daemon` | bool | | `true` for long-running daemon tasks |
| `trigger.daemon.restart` | string | | `always` (default), `never`, `on-failure` |
| `fs` | list | | Filesystem access declarations |
| `fs[].path` | string | | Absolute or `~`-prefixed path |
| `fs[].permission` | string | | `r`, `w`, or `rw` |
| `params` | list | | Input parameters with defaults |
| `params[].name` | string | | Parameter name |
| `params[].description` | string | | Human-readable description |
| `params[].default` | string | | Default value (all params are strings) |
| `env` | list of strings | | Secret/env var names required by this task |
| `tags` | list of strings | | Tags for filtering (future: source selectors) |

### Trigger types

**Cron** — runs on a schedule:
```yaml
trigger:
  cron: "*/15 * * * *"   # every 15 minutes
```

Uses standard 5-field cron syntax. Evaluated against the machine's local timezone.

**Webhook** — fires on HTTP POST:
```yaml
trigger:
  webhook: /github-push
```

Endpoint: `POST /hooks/github-push`. Request body available as `input` global in `task.js`.

**Manual** — only fires when explicitly triggered via API or UI:
```yaml
trigger:
  manual: true
```

**Daemon** — starts with dicode and runs indefinitely. Restarts automatically if the script exits.
```yaml
trigger:
  daemon: true
  restart: always   # always (default) | never | on-failure
```

Daemon tasks are long-lived processes — the script never returns. Use them to run persistent HTTP servers, background workers, or anything that should always be running. See [Web UI & API](./webui-api.md#webui-as-a-daemon-task) for the WebUI-as-task pattern.

**Chain** — fires when another task completes:
```yaml
trigger:
  chain:
    from: fetch-emails
    on: success    # success | failure | always
```

The completing task's return value is available as the `input` global.

---

## `task.js`

Plain JavaScript (ES2020, no `import`/`require`). Runs in goja — a pure Go JS engine.

Globals available: `http`, `kv`, `log`, `params`, `env`, `input`, `notify`, `dicode`.

### Example

```javascript
// Read params and env
const channel = params.get("slack_channel")
const token = env.get("SLACK_TOKEN")

// Fetch data
const res = await http.get("https://gmail.googleapis.com/gmail/v1/users/me/messages", {
  headers: { Authorization: `Bearer ${env.get("GMAIL_TOKEN")}` }
})

const messages = res.body.messages || []
log.info(`Found ${messages.length} messages`)

// Post to Slack
await http.post("https://slack.com/api/chat.postMessage", {
  headers: { Authorization: `Bearer ${token}` },
  body: {
    channel,
    text: `You have ${messages.length} unread emails`
  }
})

// Return value available to chained tasks
return { count: messages.length }
```

### Constraints

- No filesystem access (`fs`, `require`, `import` are not available)
- No shell execution (`child_process` is not available)
- No network access except via `http` global
- Return value must be JSON-serializable (for chain triggers — capped at 1MB)
- Async/await supported. Top-level await works.

---

## `task.test.js`

Unit test file. Uses a mock-aware test harness injected by the runtime.

See [Testing & Validation](./testing.md) for full documentation.

### Example

```javascript
test("sends digest to slack on new emails", async () => {
  http.mock("GET", "https://gmail.googleapis.com/*", {
    status: 200,
    body: { messages: [{ id: "1", snippet: "Hello from Alice" }] }
  })
  http.mock("POST", "https://slack.com/api/chat.postMessage", {
    status: 200,
    body: { ok: true }
  })
  env.set("GMAIL_TOKEN", "test-gmail-token")
  env.set("SLACK_TOKEN", "test-slack-token")
  params.set("slack_channel", "#test")

  const result = await runTask()

  assert.equal(result.count, 1)
  assert.httpCalled("POST", "https://slack.com/api/chat.postMessage")
})

test("handles empty inbox gracefully", async () => {
  http.mock("GET", "https://gmail.googleapis.com/*", {
    status: 200,
    body: { messages: [] }
  })
  env.set("GMAIL_TOKEN", "test-token")
  env.set("SLACK_TOKEN", "test-token")

  const result = await runTask()

  assert.equal(result.count, 0)
})
```

---

## Filesystem access

By default tasks have **zero filesystem access**. To grant access, declare the paths and permissions explicitly in `task.yaml`:

```yaml
fs:
  - path: ~/data
    permission: r       # read-only
  - path: ~/reports
    permission: rw      # read + write + delete
  - path: /tmp/dicode
    permission: rw
```

| Permission | Read | Write | Delete | mkdir |
|---|---|---|---|---|
| `r` | ✅ | ❌ | ❌ | ❌ |
| `w` | ❌ | ✅ | ✅ | ✅ |
| `rw` | ✅ | ✅ | ✅ | ✅ |

**Path resolution:**
- `~` is expanded to the user's home directory
- Relative paths are resolved relative to the task folder (useful for bundling data files alongside the script)
- Symlinks are resolved before permission checking — a symlink pointing outside a declared path is rejected
- `../` traversal that escapes a declared base path is rejected

The `fs` global is only injected into the runtime when `fs:` is declared. Tasks without `fs:` cannot access the filesystem at all.

See [JS Runtime — fs global](./js-runtime.md#fs--filesystem-access) for the full API.

## Rich output types

Tasks can return typed output that renders nicely in the WebUI. Use the `output` global:

```javascript
// Default: JSON viewer
return { count: 5 }

// Rendered HTML (sandboxed iframe in WebUI)
return output.html(`<h1>Daily Report</h1><table>...</table>`)

// Plain text (monospace block)
return output.text("Done: processed 42 items\n3 errors")

// Image
return output.image("image/png", base64PngData)

// File download
return output.file("report.csv", csvContent, "text/csv")

// HTML with structured data for chain triggers
// chained tasks receive { count: 5 }, not the HTML
return output.html(htmlContent, { data: { count: 5 } })
```

See [JS Runtime — output global](./js-runtime.md#output--rich-return-values) for the full API.

## Task ID

The task ID is derived from the folder name. It must be:
- Lowercase letters, digits, and hyphens only
- Unique across all configured sources
- Stable — changing the folder name changes the ID (breaks chain references and run history links)

Examples: `morning-email-check`, `github-release-notifier`, `backup-database`

---

## File layout rules

- `task.yaml` and `task.js` are both required. A folder missing either is ignored.
- `task.test.js` is optional. `dicode task test` skips tasks without it.
- Any other files in the folder are ignored (useful for README, schema files, etc.).
- Subdirectories are ignored — task folders are flat.

---

## Configuration reference

For the full `dicode.yaml` configuration, see [Deployment](./deployment.md#configuration-reference).
