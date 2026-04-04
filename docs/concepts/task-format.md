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

permissions:
  env:
    - GMAIL_TOKEN
    - SLACK_TOKEN
```

### All fields

| Field | Type | Required | Description |
| --- | --- | --- | --- |
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
| `permissions` | object | | Explicit access grants — nothing is implicit |
| `permissions.env` | list | | Env vars the script may read (see below) |
| `permissions.fs` | list | | Filesystem access declarations (Deno only) |
| `permissions.fs[].path` | string | | Absolute or `~`-prefixed path |
| `permissions.fs[].permission` | string | | `r`, `w`, or `rw` |
| `permissions.run` | list of strings | | Executables the script may spawn (Deno only); use `["*"]` for all |
| `params` | list | | Input parameters with defaults |
| `params[].name` | string | | Parameter name |
| `params[].description` | string | | Human-readable description |
| `params[].default` | string | | Default value (all params are strings) |
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
| --- | --- | --- |
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

### Container fields reference

| Field | Type | Description |
| --- | --- | --- |
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

### Test example

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

## Permissions

All access is **deny by default**. Tasks can only read env vars, touch the filesystem, or spawn subprocesses that are explicitly listed under `permissions:`.

```yaml
permissions:
  env:
    - SLACK_TOKEN               # bare: allowlist $SLACK_TOKEN from host env (same name)
    - name: API_KEY             # from: read $GH_TOKEN from host OS env, inject as API_KEY
      from: GH_TOKEN
    - name: DB_PASS             # secret: resolve "db_password" from secrets store
      secret: db_password
  net:
    - "api.github.com"          # restrict outbound to these hosts (omit = unrestricted)
  fs:
    - path: ~/data
      permission: r             # read-only
    - path: ~/reports
      permission: rw            # read + write + delete
  run:
    - curl                      # allow spawning curl (Deno only)
    # - "*"                     # allow all executables
  sys:
    - hostname                  # Deno system-info APIs (omit = deny all)
```

### `permissions.env` — environment variables

Four forms, with clear source distinction:

| Form | Key | Source | Effect |
| --- | --- | --- | --- |
| Bare name | — | Host OS env | Script reads `$VAR` at runtime via `env.get()`; no injection |
| `from:` | host OS var name | Host OS env | Read `$GH_TOKEN` from OS, inject subprocess env as `API_KEY` |
| `secret:` | secrets store key | Secrets store | Resolve encrypted secret, inject as the given name; **fails if not found** |
| `value:` | — | Literal | Inject a fixed string (used by taskset override layers) |

**`from:` vs `secret:` — the key distinction:**

- `from:` reads **only** from the host OS environment (`os.Getenv`). Use it to rename a host env var or make the mapping explicit.
- `secret:` reads **only** from the dicode secrets store (set via `dicode secrets set`). Run fails immediately if the key is not in the store.
- A bare name does **neither** — it only allowlists the var so the script can read it from the host env via `env.get()`. No injection, no secrets lookup.

#### Example 1 — bare passthrough (name stays the same)

```yaml
# task.yaml
permissions:
  env:
    - GITHUB_TOKEN
```

```typescript
// task.ts
export default async function main({ env }) {
  const token = await env.get("GITHUB_TOKEN")  // reads $GITHUB_TOKEN from host env at runtime
}
```

#### Example 2 — rename from host OS env

The host OS has `GH_TOKEN`. The script needs it as `GITHUB_TOKEN`.

```yaml
# task.yaml
permissions:
  env:
    - name: GITHUB_TOKEN   # name the script sees
      from: GH_TOKEN       # name in the host OS environment
```

```typescript
// task.ts
export default async function main({ env }) {
  const token = await env.get("GITHUB_TOKEN")  // injected from $GH_TOKEN
}
```

#### Example 3 — inject from secrets store

Store first: `dicode secrets set slack_bot_token xoxb-…`

```yaml
# task.yaml
permissions:
  env:
    - name: SLACK_TOKEN        # name the script sees
      secret: slack_bot_token  # key in the dicode secrets store
```

```typescript
// task.ts
export default async function main({ env }) {
  const token = await env.get("SLACK_TOKEN")  // resolved from secrets store
}
```

#### Example 4 — all forms together

```yaml
# task.yaml
permissions:
  env:
    - PORT                          # bare: script reads $PORT from host env directly
    - name: GITHUB_TOKEN            # from: rename $GH_TOKEN → GITHUB_TOKEN
      from: GH_TOKEN
    - name: SLACK_TOKEN             # secret: from encrypted secrets store
      secret: slack_bot_token
    - name: LOG_LEVEL               # value: literal (set by taskset override)
      value: "info"
  net:
    - "api.github.com"
    - "hooks.slack.com"
```

```typescript
// task.ts
export default async function main({ env }) {
  const port    = await env.get("PORT")          // from host env (bare)
  const ghToken = await env.get("GITHUB_TOKEN")  // injected, renamed from $GH_TOKEN
  const slToken = await env.get("SLACK_TOKEN")   // injected from secrets store
  const level   = await env.get("LOG_LEVEL")     // literal "info"
}
```

### `permissions.fs` — filesystem access (Deno only)

| Permission | Read | Write | Delete | mkdir |
| --- | --- | --- | --- | --- |
| `r` | ✅ | ❌ | ❌ | ❌ |
| `w` | ❌ | ✅ | ✅ | ✅ |
| `rw` | ✅ | ✅ | ✅ | ✅ |

`~` is expanded to the user's home directory at runtime.

### `permissions.run` — subprocess execution (Deno only)

Lists executables the script may spawn via `Deno.Command`. Use `["*"]` to allow all. Omitting this field blocks all subprocess execution.

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
