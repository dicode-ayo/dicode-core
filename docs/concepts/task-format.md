# Task Format

Every dicode task is a folder containing up to three files:

```text
tasks/
└── morning-email-check/
    ├── task.yaml       ← required: trigger, params, env, metadata
    ├── task.ts         ← required: TypeScript/JS logic (Deno runtime)
    └── task.test.ts    ← optional: unit tests
```

When using a TaskSet source, the folder name is not the task ID — instead, the ID is built from the namespace path (e.g. `infra/morning-email-check`).

---

## `task.yaml`

All task files must declare `apiVersion` and `kind`:

### Minimal example

```yaml
apiVersion: dicode/v1
kind: Task
name: Morning Email Check
trigger:
  cron: "0 8 * * *"
```

### Full example

```yaml
apiVersion: dicode/v1
kind: Task
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
| `runtime` | string | | `deno` (default), `python`, `docker`, or `podman` |
| `trigger` | object | ✅ | Exactly one trigger must be set |
| `trigger.cron` | string | | Standard cron expression (5 fields) |
| `trigger.webhook` | string | | Webhook path, e.g. `/github-push` |
| `trigger.auth` | bool | | Require a valid dicode session for webhook GET (UI) and POST (run) |
| `trigger.manual` | bool | | Set `true` to enable manual-only |
| `trigger.chain` | object | | Chain trigger (see below) |
| `trigger.chain.from` | string | | Task ID to listen for |
| `trigger.chain.on` | string | | `success` (default), `failure`, `always` |
| `trigger.daemon` | bool | | Start on app start, restart on exit |
| `trigger.restart` | string | | daemon only: `always` (default), `on-failure`, `never` |
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

To require a valid dicode session before allowing access to the webhook UI or running the task, add `auth: true`:

```yaml
trigger:
  webhook: /hooks/my-internal-tool
  auth: true
```

- `GET /hooks/my-internal-tool` (serving `index.html`) → redirects to `/?auth=required` if no session
- `POST /hooks/my-internal-tool` (running the task) → returns `401` JSON if no session
- `dicode.js` handles 401 automatically: silent refresh via device token, then redirects to login
- Open webhooks (no `auth: true`) remain fully public — no behaviour change

**Manual** — only fires when explicitly triggered via API or UI:
```yaml
trigger:
  manual: true
```

**Daemon** — starts automatically when dicode starts and restarts when it exits.
```yaml
trigger:
  daemon: true
  restart: always   # always (default) | on-failure | never
```

- **`always`** (default) — restarts whenever the task exits (success, failure). Does not restart if explicitly killed.
- **`on-failure`** — only restarts on non-zero exit / script error. Stops if the task succeeds.
- **`never`** — starts once on app start, never restarts.

**Stale run detection:** if dicode is killed without a clean shutdown, any "running" runs from the previous session are automatically marked "cancelled" on the next startup, so the history stays accurate and daemon tasks start fresh.

**Graceful shutdown:** when dicode stops, all daemon tasks receive a kill signal (SIGTERM for Docker tasks, context cancellation for JS tasks) before the process exits.

A 2-second back-off is applied between restarts to prevent tight loops on immediately-failing tasks.

**Chain** — fires when another task completes:
```yaml
trigger:
  chain:
    from: fetch-emails
    on: success    # success | failure | always
```

The completing task's return value is available as the `input` global.

---

## Docker runtime

Set `runtime: docker` to run a container instead of a JS script. No `task.js` is needed. Uses the Docker daemon via the Go SDK.

```yaml
name: Nginx Dev Server
description: Serves /tmp on port 8888. Kill from the run page when done.
runtime: docker

trigger:
  manual: true

docker:
  image: nginx:alpine
  pull_policy: missing       # always | missing (default) | never
  ports:
    - "8888:80"              # host:container
  volumes:
    - "/tmp:/usr/share/nginx/html:ro"
```

A more complete example:

```yaml
name: Data Pipeline
runtime: docker

trigger:
  cron: "0 3 * * *"

docker:
  image: python:3.12-slim
  command: ["python", "/scripts/pipeline.py"]
  pull_policy: missing
  volumes:
    - "/data/input:/input:ro"
    - "/data/output:/output"
  working_dir: /scripts
  env_vars:
    BATCH_SIZE: "500"
```

---

## Podman runtime

Set `runtime: podman` to run a rootless container via the `podman` CLI. Uses the same `docker:` config section as the Docker runtime — no changes to task fields required.

Podman must be installed on the host via the system package manager. dicode does not download it automatically, but the **Config → Runtimes** card will show its status and link to installation instructions.

```yaml
name: Nginx Dev Server
runtime: podman

trigger:
  manual: true

docker:
  image: nginx:alpine
  ports:
    - "8888:80"
  volumes:
    - "/tmp:/usr/share/nginx/html:ro"
```

**Differences from Docker:**

| | Docker | Podman |
|---|---|---|
| Daemon required | Yes (`dockerd`) | No — daemonless, rootless by default |
| Go SDK | Yes | No — dicode uses the CLI |
| stdout/stderr | Multiplexed (Docker framing) | Plain line-by-line streams |
| Binary management | System / Docker Desktop | System package manager |

---

## Container fields (`docker:`)

Both the `docker` and `podman` runtimes share the same `docker:` config section.
Either `image` or `build` must be set — not neither.

### Pull a pre-built image

```yaml
docker:
  image: nginx:alpine
  pull_policy: missing   # always | missing (default) | never
```

### Build from a local Dockerfile

```yaml
docker:
  build:
    dockerfile: Dockerfile   # relative to task folder; default "Dockerfile"
    context: .               # relative to task folder; default task folder
  ports:
    - "8888:80"
```

The built image is tagged `dicode-<taskID>:<hash>` where `<hash>` is derived
from the Dockerfile content. If the Dockerfile hasn't changed, the existing image
is reused and the build step is skipped entirely. Build output is streamed to the
run log in real time.

Use **Edit code** on the task page to edit the Dockerfile directly in the web UI.

### All fields

| Field | Type | Description |
|---|---|---|
| `docker.image` | string | Container image (e.g. `nginx:alpine`). Required if `build` is not set. |
| `docker.build` | object | Build from local Dockerfile instead of pulling. |
| `docker.build.dockerfile` | string | Path to Dockerfile, relative to task folder. Default: `Dockerfile` |
| `docker.build.context` | string | Build context path, relative to task folder. Default: task folder |
| `docker.command` | list | Overrides image CMD |
| `docker.entrypoint` | list | Overrides image ENTRYPOINT |
| `docker.ports` | list | Port bindings — `"hostPort:containerPort"` |
| `docker.volumes` | list | Volume mounts — `"host:container[:ro]"` |
| `docker.working_dir` | string | Container working directory |
| `docker.env_vars` | map | Literal environment variables injected into container |
| `docker.pull_policy` | string | `missing` (default), `always`, `never`. Ignored when using `build`. |

**Live logs** — container stdout/stderr is streamed line-by-line to the run log as it runs.

**Kill** — Container tasks may run indefinitely. Use the **Kill** button on the run detail page (or `POST /api/runs/{runID}/kill`) to stop the container gracefully (SIGTERM + 10 s timeout).

**No default timeout** — unlike JS tasks (60 s default), container tasks have no timeout unless you set `timeout:` explicitly.

---

## `task.ts` / `task.js` (Deno runtime)

TypeScript or JavaScript. Runs via a managed Deno subprocess.

Globals available: `log`, `kv`, `params`, `env`, `input`, `output`.

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

- Filesystem access requires explicit `fs:` declarations in task.yaml
- Return value must be JSON-serializable (for chain triggers — capped at 1MB)
- Async/await and top-level await are supported

---

## `task.py` (Python runtime)

Python script executed via the managed [uv](https://github.com/astral-sh/uv) runner.
Install the Python runtime from **Config → Runtimes** before use.

```yaml
runtime: python
```

Params are available as `DICODE_PARAM_<NAME>` environment variables (name uppercased).
Inline dependencies via PEP 723 `# /// script` blocks are supported.

See [Python Runtime](../python-runtime.md) for full documentation.

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

- `task.yaml` is always required. A folder without it is ignored.
- The script file (`task.ts`, `task.js`, or `task.py`) is required for code runtimes; omit it only for `runtime: docker` or `runtime: podman`.
- Container tasks using `docker.build` need a `Dockerfile` in the task folder (or at the path set in `docker.build.dockerfile`).
- `task.test.js` / `task.test.ts` is optional. `dicode task test` skips tasks without it.
- Any other files in the folder are ignored (useful for README, schema files, etc.).
- Subdirectories are ignored — task folders are flat.

---

## Configuration reference

For the full `dicode.yaml` configuration, see [Deployment](./deployment.md#configuration-reference).
