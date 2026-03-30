# dicode

**GitOps-native task orchestrator with AI generation.**

Tell an AI what you want to automate. It writes the code, commits it to your git repo, and `dicode` picks it up automatically — no redeployment, no config files, no YAML pipelines.

> "Generate a task that checks my Gmail every morning and posts a digest to #devops on Slack."

That's it. The task appears in your dashboard within seconds.

---

## What it is

`dicode` is a single Go binary that:

- **Watches task sources** and reconciles them automatically (like ArgoCD, but for automation tasks) — git repos, local directories, or hierarchical **TaskSet** manifests
- **Executes tasks** on a schedule (cron), via HTTP webhook, or manually from the web UI
- **Lets AI write your tasks** from natural language — the generated code lives in your source so you can read, review, and modify it
- **Serves a web UI** for monitoring runs, viewing logs, triggering tasks, and managing sources
- **Exposes an MCP server** at `/mcp` so AI agents (Claude Code, Cursor) can list tasks, trigger runs, and control dev mode

Tasks are TypeScript/Python/Docker containers. You can write them yourself or have AI generate them. All approaches produce the same artifact: a folder with `task.yaml` + script.

---

## How it works

```
┌─────────────────────────────────────────────────────────────────┐
│                         tasks git repo                          │
│  tasks/                                                         │
│  ├── morning-email-check/                                       │
│  │   ├── task.yaml   ← trigger config, params, env vars        │
│  │   └── task.js     ← JavaScript logic                        │
│  └── weekly-report/                                             │
│      ├── task.yaml                                              │
│      └── task.js                                                │
└──────────────────────┬──────────────────────────────────────────┘
                       │  git pull (every 30s or on push webhook)
                       ▼
┌─────────────────────────────────────────────────────────────────┐
│                        dicode binary                            │
│                                                                 │
│  ┌─────────────┐   ┌──────────────┐   ┌──────────────────────┐ │
│  │ Reconciler  │──▶│   Registry   │──▶│   Trigger Engine     │ │
│  │             │   │              │   │  cron / webhook /    │ │
│  │ add/remove/ │   │  in-memory   │   │  manual              │ │
│  │ update tasks│   │  task state  │   └──────────┬───────────┘ │
│  └─────────────┘   └──────────────┘              │             │
│                                                  ▼             │
│  ┌─────────────┐   ┌──────────────┐   ┌──────────────────────┐ │
│  │  AI Gen     │   │   Web UI     │   │   JS Runtime (goja)  │ │
│  │             │   │   + REST API │   │   http / kv / log    │ │
│  │  prompt →   │   │              │   │   params / env       │ │
│  │  code →     │   │  dashboard   │   ├──────────────────────┤ │
│  │  git commit │   │  logs/runs   │   │   Docker Runtime     │ │
│  └─────────────┘   └──────────────┘   │   live logs / kill   │ │
│                                        └──────────┬───────────┘ │
│                                        ┌─────────────────────┐ │
│                                        │   SQLite run log    │ │
│                                        └─────────────────────┘ │
└─────────────────────────────────────────────────────────────────┘
```

### The reconciliation loop

Every 30 seconds (or immediately on a push webhook), `dicode`:

1. Re-resolves the full task tree from each source
2. Diffs the result against the current registry snapshot
3. Emits Added / Updated / Removed events:
   - New task → register and start scheduling
   - Removed task → deregister and cancel triggers
   - Changed task (different content hash) → reload with new config

This means you never need to restart `dicode`. Change a file and it's live within one poll interval (or ~100ms for local sources with fsnotify).

### TaskSet sources

Tasks are organized as **TaskSets** — hierarchical YAML manifests that compose task trees:

```yaml
# taskset.yaml
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  defaults:
    timeout: 30m
  entries:
    deploy-backend:
      ref:
        path: ./backend/task.yaml
    platform:
      ref:
        path: ./platform/taskset.yaml   # nested TaskSet
```

Tasks get namespace-scoped IDs: `infra/deploy-backend`, `infra/platform/nginx-start`. A 6-level override stack flows defaults from root TaskSet down to individual tasks.

---

## Quickstart

**No GitHub account? No problem.** Dicode works entirely locally — git is optional.

### Option A — Local only (no accounts needed)

```bash
# 1. Install
curl -Lo dicode https://github.com/dicode/dicode/releases/latest/download/dicode-linux-amd64
chmod +x dicode && sudo mv dicode /usr/local/bin/

# 2. Run — first launch opens the onboarding wizard in your browser
dicode
```

The onboarding wizard asks one question:

```
Welcome to dicode

How do you want to store your tasks?

  ● Local only   — no accounts needed, tasks stay on this machine
  ○ Git repo     — tasks versioned in GitHub/GitLab

[Get started →]
```

Choose **Local only** and dicode:
1. Creates `~/dicode-tasks/` on your machine
2. Writes a minimal `dicode.yaml` automatically
3. Opens the dashboard — ready to create your first task

That's it. No git, no GitHub token, no relay account.

### Option B — Git-backed (versioned, shareable)

```bash
# 1. Install (same as above)

# 2. Run — choose "Git repo" in the onboarding wizard
dicode

# Or skip the wizard by providing a config directly:
export GITHUB_TOKEN=ghp_...
export ANTHROPIC_API_KEY=sk-ant-...
dicode --config dicode.yaml
```

```yaml
# dicode.yaml
sources:
  - type: git
    url: https://github.com/you/my-tasks
    branch: main
    auth:
      type: token
      token_env: GITHUB_TOKEN

ai:
  api_key_env: ANTHROPIC_API_KEY
```

### Create your first task (both options)

Open `http://localhost:8080`, click **New Task**, and describe what you want:

> "Ping https://myapi.example.com/health every 5 minutes and log the status"

dicode generates the code, shows you a diff, and saves it. The task is live immediately.

### Migrating from local to git later

Start local, add git when you're ready — no rework required:

```bash
# 1. Add a git source to dicode.yaml
# 2. Push your local tasks to the repo
dicode task commit my-task --to https://github.com/you/my-tasks
# task moves to the git repo; reconciler takes over ownership automatically
```

---

## Task Format

Every task lives in its own directory inside the `tasks/` folder of your git repo:

```
tasks/
└── my-task-name/
    ├── task.yaml       ← metadata, trigger, params, required env vars
    ├── task.js         ← JavaScript logic
    └── task.test.js    ← optional unit tests
```

### task.yaml

```yaml
name: morning-email-check
description: Check Gmail and post a digest to Slack every morning

runtime: js   # js (default) or docker

trigger:
  cron: "0 9 * * *"   # 9am every day
  # webhook: /hooks/my-task   # or: HTTP POST trigger
  # manual: true              # or: only manual via UI/API
  # daemon: true              # or: start on app start, restart on exit

params:
  - name: slack_channel
    default: "#general"
  - name: max_emails
    default: "10"

env:
  - SLACK_TOKEN    # env vars resolved from secrets chain
  - GMAIL_TOKEN

timeout: 120s   # default: 60s for JS tasks; no timeout for Docker/daemon
```

**Trigger options** — exactly one must be set:

| Type | Example | Description |
|------|---------|-------------|
| `cron` | `"0 9 * * *"` | Standard 5-field cron expression |
| `webhook` | `"/hooks/my-task"` | HTTP POST to this path triggers a run |
| `manual` | `true` | Only triggerable from UI or API |
| `chain` | `from: other-task` | Triggered when another task completes |
| `daemon` | `true` | Starts on app start, restarted on exit |

**Docker runtime** — run containers instead of JS scripts. Live log streaming, kill support, daemon-compatible:

```yaml
name: Nginx Dev Server
runtime: docker
trigger:
  daemon: true
docker:
  image: nginx:alpine
  pull_policy: missing
  ports:
    - "8888:80"
  volumes:
    - "/tmp:/usr/share/nginx/html:ro"
```

### task.js

The task script. Has access to the following globals:

#### `http` — make outbound HTTP requests

```javascript
// GET
const res = await http.get("https://api.example.com/data")
console.log(res.status)    // 200
console.log(res.body)      // parsed JSON if Content-Type is application/json, else string

// POST
const res = await http.post("https://api.example.com/events", {
  headers: { "Authorization": `Bearer ${env.MY_TOKEN}` },
  body: { event: "deploy", status: "success" }
})

// Other methods
await http.put(url, options)
await http.patch(url, options)
await http.delete(url, options)
```

#### `kv` — persistent key-value store (per-task, survives restarts)

```javascript
kv.set("last_processed_id", "msg_12345")
const id = kv.get("last_processed_id")   // "msg_12345" or null
kv.delete("last_processed_id")
```

#### `log` — structured logging (appears in the run log in the UI)

```javascript
log.info("starting email check")
log.warn("rate limited, waiting...")
log.error("failed to send Slack message", { error: err.message })
```

#### `params` — values from task.yaml params (with user overrides applied)

```javascript
const channel = params.slack_channel    // "#devops" (from params or default)
const max = params.max_emails           // 10
```

#### `env` — environment variables declared in task.yaml `env:` list

```javascript
const token = env.SLACK_TOKEN   // only vars declared in task.yaml are accessible
```

#### `input` — output from the previous task (only in chain-triggered tasks)

```javascript
// task-b is triggered when task-a completes
log.info("emails received", { count: input.count })
await sendToSlack(input.emails)
```

The `input` global contains whatever the upstream task returned. Always JSON.

#### `webhook` — incoming webhook payload (only in webhook-triggered tasks)

```javascript
const payload = webhook.body      // parsed request body
const headers = webhook.headers   // request headers
```

#### `notify` — send push notifications

```javascript
await notify.send("Task completed successfully", { priority: "default" })
await notify.send("API is DOWN", {
  priority: "urgent",   // min | low | default | high | urgent
  tags: ["warning", "skull"]
})
```

Uses the notification provider configured in `dicode.yaml`. No configuration needed in the task itself.

#### `dicode` — communicate with the orchestrator

```javascript
// Report intermediate progress (streamed live to the WebUI run log)
dicode.progress("processing email 42 of 200", { done: 42, total: 200 })

// Send a notification through the configured provider
await dicode.notify("Unusual spike detected", { priority: "high" })

// Imperatively trigger another task (fire-and-forget)
// Different from chain: this task controls when and whether to fire
await dicode.trigger("send-alert", { reason: "threshold exceeded", value: 99 })

// Query orchestrator state
const running = await dicode.isRunning("backup-task")
if (running) {
  log.info("backup in progress, skipping duplicate run")
  return
}

// Human approval gate — suspends this run, sends actionable notification,
// resumes when user responds (north star feature)
const decision = await dicode.ask("Deploy to production?", {
  timeout: "30m",
  options: ["approve", "reject"]
})
if (decision !== "approve") return
```

### Full example

```javascript
// task: morning-email-check
// Fetches recent emails via Gmail API and posts a digest to Slack

const since = kv.get("last_check") || new Date(Date.now() - 86400000).toISOString()

log.info("checking emails since", { since })

const emails = await http.get("https://gmail.googleapis.com/gmail/v1/users/me/messages", {
  headers: { Authorization: `Bearer ${env.GMAIL_TOKEN}` },
  params: { q: `after:${since} is:unread`, maxResults: params.max_emails }
})

if (emails.body.messages?.length === 0) {
  log.info("no new emails")
  return
}

const lines = emails.body.messages.map(m => `• ${m.snippet}`)
const text = `*Morning email digest* (${lines.length} unread)\n${lines.join("\n")}`

await http.post("https://slack.com/api/chat.postMessage", {
  headers: { Authorization: `Bearer ${env.SLACK_TOKEN}` },
  body: { channel: params.slack_channel, text }
})

kv.set("last_check", new Date().toISOString())
log.info("digest sent", { count: lines.length, channel: params.slack_channel })
```

---

## Configuration Reference

```yaml
# sources: one or more task sources (git repos and/or local directories)
sources:
  # Remote git repo — pulled every poll_interval or on push webhook
  - type: git
    url: https://github.com/you/my-tasks   # required
    branch: main                            # default: main
    poll_interval: 30s                      # default: 30s
    auth:
      type: token                           # "token" or "ssh"
      token_env: GITHUB_TOKEN               # env var holding the token
      ssh_key: ~/.ssh/id_ed25519            # path to SSH key (if type: ssh)

  # Second git repo — multiple sources are merged into one registry
  - type: git
    url: https://github.com/team/shared-tasks
    branch: main
    auth:
      type: token
      token_env: GITHUB_TOKEN

  # Local directory — watched via fsnotify for instant reloads (dev workflow)
  - type: local
    path: ~/tasks-dev                       # required
    watch: true                             # default: true

secrets:
  providers:
    - type: local                         # encrypted SQLite (default, recommended first)
    - type: env                           # host env vars (fallback)
    # - type: vault
    #   address: https://vault.example.com
    #   token_env: VAULT_TOKEN

notifications:
  on_failure: true                        # push notification on any task failure
  on_success: false
  provider:
    type: ntfy                            # ntfy | gotify | pushover | telegram
    url: https://ntfy.sh                  # or self-hosted ntfy instance
    topic: my-dicode-alerts               # your private ntfy topic
    token_env: NTFY_TOKEN                 # optional auth token env var

server:
  port: 8080                              # web UI + API port (default: 8080)
  auth: false                             # set true to require passphrase for all endpoints
  secret: ""                              # optional YAML passphrase override; if omitted dicode
                                          # auto-generates one on first boot and stores it in SQLite
  allowed_origins: []                     # CORS allowlist — empty = same-origin only
  mcp: true                               # MCP server at /mcp (default: true)
  tray: true                              # system tray icon (default: true when interactive)

ai:
  model: gpt-4o                           # any OpenAI-compatible model name
  api_key_env: OPENAI_API_KEY             # env var holding the API key
  # base_url: https://api.openai.com/v1  # default; override for Anthropic/Ollama/etc.

  # Claude (via Anthropic API):
  # model: claude-sonnet-4-6
  # api_key_env: ANTHROPIC_API_KEY
  # base_url: https://api.anthropic.com/v1

  # Local Ollama:
  # model: qwen2.5-coder:7b
  # base_url: http://localhost:11434/v1
  # api_key_env: ""  # not needed

log_level: info                           # "debug", "info", "warn", "error"
data_dir: ~/.dicode                       # where to store repo clones, sqlite db, etc.
```

**Task ID uniqueness**: task IDs (directory names) must be unique across all sources. If two sources contain a task with the same ID, the second one is skipped and an error is logged.

---

## AI Task Generation

### New task from prompt

Open the web UI and click **New Task**. Type what you want in plain English:

> "Every Monday at 9am, fetch the top 10 posts from Hacker News and send them to my Slack channel #links"

dicode will:

1. Ask your configured AI model to generate `task.yaml` + `task.js`
2. Validate the generated code (syntax check, schema check)
3. Show you the generated files in the browser
4. Save to your local tasks directory immediately — the task goes live within ~100ms

The AI has access to the full JS API documentation and a few example tasks as context, so the generated code consistently uses the correct patterns.

### AI chat in the editor

When editing an existing task, the Monaco editor has a built-in AI panel. Ask questions or request changes:

> "Rewrite this to batch the API calls in groups of 10"
> "Add error handling for rate limit responses"
> "Write a task.test.js for this"

The AI can read the current file content and write files directly using a `write_file` tool — changes appear in the editor instantly. The conversation streams in real time via SSE.

Works with any OpenAI-compatible endpoint — OpenAI, Anthropic (Claude), Ollama, or any other provider that speaks the OpenAI API format.

---

## Task Store

Tasks can be shared as git repos. Install any public task with:

```bash
dicode task install github.com/dicode-community/tasks/morning-email-check
```

Override params at install time:

```bash
dicode task install github.com/dicode-community/tasks/morning-email-check \
  --param slack_channel="#devops" \
  --param max_emails=20
```

This clones the task directory into your tasks repo, commits, and pushes. The reconciler picks it up automatically.

### Publishing a task

Any git repo with a `tasks/` directory is a valid task source. To share your tasks:

1. Push your tasks repo to GitHub as a public repo
2. Tell others the install path: `github.com/you/my-tasks/<task-name>`

A community registry index is planned — tasks will be searchable by name and tag from the UI.

---

## Local-Only Mode

**Git is fully optional.** You do not need a GitHub account, a git repo, or any cloud service to use dicode. Everything runs on your machine.

### What works without git

| Feature | Local-only |
|---|---|
| Task execution (cron / webhook / manual / chain / daemon) | ✅ |
| JavaScript runtime + all globals | ✅ |
| Docker runtime (containers as tasks) | ✅ requires Docker daemon |
| AI task generation | ✅ needs any OpenAI-compatible API key |
| Local encrypted secrets | ✅ |
| MCP server + agent skill | ✅ |
| Testing, validation, dry-run | ✅ |
| Tray icon + desktop notifications | ✅ |
| Mobile push (ntfy) | ✅ optional |
| WebUI + REST API | ✅ |
| Community task store (install tasks) | ✅ public repos |
| Relay / public webhook URLs | ❌ no public URL without account |
| Task version history | ❌ no git = no history |

### Minimal config

The onboarding wizard generates this automatically when you choose "Local only":

```yaml
# ~/.dicode/dicode.yaml (auto-generated on first run)
sources:
  - type: local
    path: ~/dicode-tasks
    watch: true

database:
  type: sqlite

server:
  port: 8080
```

No tokens, no URLs, no external services. Tasks are files in `~/dicode-tasks/`. The reconciler watches that directory with fsnotify — save a file and the task is live within ~100ms.

### AI generation in local mode

AI generation still works — it writes generated task files directly to your local tasks directory instead of committing to git. Any OpenAI-compatible endpoint works:

```yaml
ai:
  model: gpt-4o
  api_key_env: OPENAI_API_KEY

  # Or use Claude via Anthropic API:
  # model: claude-sonnet-4-6
  # api_key_env: ANTHROPIC_API_KEY
  # base_url: https://api.anthropic.com/v1

  # Or a local Ollama model (no API key needed):
  # model: qwen2.5-coder:7b
  # base_url: http://localhost:11434/v1
```

Or skip AI entirely and write tasks by hand — the JS runtime, testing, and all other features work without it.

### First-run onboarding

When no `dicode.yaml` exists, dicode opens a one-time setup wizard in your browser:

```
┌─────────────────────────────────────────────────────────┐
│  Welcome to dicode                                      │
│                                                         │
│  How do you want to store your tasks?                   │
│                                                         │
│  ● Local only                                           │
│    No accounts needed. Tasks stay on this machine.      │
│    Great for personal automation.                       │
│                                                         │
│  ○ Git repo                                             │
│    Tasks versioned in GitHub or GitLab.                 │
│    Shareable, auditable, works with CI.                 │
│                                                         │
│                          [Get started →]                │
└─────────────────────────────────────────────────────────┘
```

Both options land on the same dashboard. You can always add a git source later.

### Migrating local tasks to git

When you're ready to version and share your tasks:

```bash
# Add a git source to dicode.yaml, then commit each local task
dicode task commit morning-email-check --to https://github.com/you/my-tasks
dicode task commit api-health-check    --to https://github.com/you/my-tasks
```

Each task is moved into the git repo, committed, and pushed. The git source takes over ownership. Your local tasks directory becomes a development sandbox.

---

## Secrets & API Keys

Tasks frequently need API keys, tokens, and passwords. dicode resolves these through a **provider chain** — a list of secret backends tried in order. Tasks declare which secrets they need; they never care which provider supplies them.

### Declaring secrets in a task

```yaml
# task.yaml
env:
  - SLACK_TOKEN
  - GMAIL_TOKEN
```

That's it. The `env.SLACK_TOKEN` global in your JS script will contain the resolved value at runtime.

### Provider chain

Configured in `dicode.yaml`:

```yaml
secrets:
  providers:
    - type: local   # encrypted SQLite — checked first
    - type: env     # host environment variables — fallback

    # Future providers (same interface, no task changes needed):
    # - type: vault
    #   address: https://vault.example.com
    #   token_env: VAULT_TOKEN
    # - type: aws-secrets-manager
    #   region: us-east-1
    # - type: gcp-secret-manager
    #   project: my-gcp-project
    # - type: doppler
    #   token_env: DOPPLER_TOKEN
```

Dicode walks the list in order. First provider that has the key wins. If no provider has it, the task run fails with a clear error before execution starts.

### Local encrypted store

The `local` provider stores secrets encrypted in SQLite using **ChaCha20-Poly1305**. The encryption key is derived from a master key via **Argon2id**.

**Master key resolution** (in order):
1. `DICODE_MASTER_KEY` env var — base64-encoded 32 bytes. Best for running dicode as a service.
2. `~/.dicode/master.key` — auto-generated on first run, `chmod 600`. Best for local use.

> **Back up `~/.dicode/master.key`**. If it's lost, encrypted secrets cannot be recovered.

#### Managing secrets

```bash
# Store a secret
dicode secrets set SLACK_TOKEN xoxb-your-token-here

# Retrieve a secret (for verification)
dicode secrets get SLACK_TOKEN

# List all stored secret names (values never printed)
dicode secrets list

# Delete a secret
dicode secrets delete SLACK_TOKEN
```

Secrets can also be managed from the Web UI under the **Secrets** tab.

### Environment variable fallback

The `env` provider reads host environment variables. Useful for:
- CI/CD environments where secrets are injected as env vars
- Gradual migration (set vars in the environment, move to local store over time)
- Quick testing without storing secrets

```bash
export SLACK_TOKEN=xoxb-...
dicode  # env.SLACK_TOKEN resolved from host env
```

### Security model

- Secret **values** are never written to git — only the key *names* appear in `task.yaml`
- Tasks can only access secrets they declare in `env:` — no ambient access to the full store
- Each task run receives a resolved snapshot of its declared secrets — the provider is not accessible from JS
- The local store file (`~/.dicode/secrets.db`) is readable only by the user who owns it (mode 0600)

### North star: explicit secret sources per task

Future `task.yaml` syntax to pin a secret to a specific provider:

```yaml
# task.yaml (future)
secrets:
  - name: SLACK_TOKEN
    from: { provider: vault, path: secret/slack, key: token }
  - name: GMAIL_TOKEN
    from: { provider: aws-secrets-manager, key: myapp/gmail/token }
```

Tasks without explicit `secrets:` entries continue to use the provider chain — so existing tasks never need to change.

---

## Notifications

dicode can push notifications to your phone or desktop when tasks fail, succeed, or need your attention.

### System-level notifications (automatic)

Configure in `dicode.yaml` — no task code required:

```yaml
notifications:
  on_failure: true    # notify when any task run fails (default: true)
  on_success: false   # notify on success (default: false)
  provider:
    type: ntfy              # ntfy | gotify | pushover | telegram
    url: https://ntfy.sh    # use ntfy.sh or point to your self-hosted instance
    topic: my-dicode-alerts # your private topic name
    token_env: NTFY_TOKEN   # optional: env var holding auth token
```

Install the [ntfy app](https://ntfy.sh) on your phone, subscribe to your topic, and you'll receive push notifications instantly when tasks fail.

**Why ntfy?** It's [Apache 2.0 licensed](https://github.com/binwiederheer/ntfy), self-hostable, has iOS and Android apps, and supports action buttons for approval gates. The API is a single HTTP POST.

### Task-level notifications (from code)

Use the `notify` global inside any task script:

```javascript
// Simple notification
await notify.send("Weekly report generated", { priority: "default" })

// Urgent alert
await notify.send("Payment API is returning 500s", {
  priority: "urgent",
  tags: ["warning", "rotating_light"]
})
```

Priority levels: `min` · `low` · `default` · `high` · `urgent`

Tags map to emoji in the ntfy app — see [ntfy emoji list](https://docs.ntfy.sh/emojis/).

### Supported providers

| Provider | License | Self-host | Action buttons |
|---|---|---|---|
| ntfy | Apache 2.0 | ✅ | ✅ |
| Gotify | MIT | ✅ | ❌ |
| Pushover | Commercial | ❌ | ✅ |
| Telegram | — | ❌ | ✅ |

All providers implement the same Go interface — switching provider requires only a config change, no task code changes.

### Desktop notifications & system tray

When running dicode locally, you also get native OS integration.

**Desktop notifications** fire automatically (alongside any configured mobile push) using the OS native notification system — no extra config needed.

**System tray icon** gives you ambient status and quick actions without opening a browser.

Enable in config or via flag:

```yaml
server:
  tray: true   # default: true when running interactively, false as a service
```

```bash
dicode --tray    # force tray icon on
dicode --no-tray # force tray icon off (e.g. when running as systemd service)
```

**Tray icon states:**

| Icon | Meaning |
|---|---|
| 🟢 Green | All tasks healthy, last run succeeded |
| 🟡 Yellow | A task is currently running |
| 🔴 Red | Last run failed — click for details |
| ⚪ Grey | Reconciler paused (dev mode or manual pause) |

**Right-click menu:**

```
┌─────────────────────────────┐
│ dicode  ●  3 tasks active   │
├─────────────────────────────┤
│ Open Web UI                 │
│ Run task ▶                  │
│   morning-email-check       │
│   api-health-check          │
│   weekly-report             │
├─────────────────────────────┤
│ Last run: api-health-check  │
│   ✓ 2 min ago               │
├─────────────────────────────┤
│ Pause reconciler            │
│ Quit                        │
└─────────────────────────────┘
```

Tray support: Linux (StatusNotifierItem / DBus — works with KDE, GNOME with AppIndicator extension, waybar, and most modern panels), macOS, Windows. Uses `fyne.io/systray` — no CGo or GTK required. Automatically disabled when running as a headless service (`server.tray: false` in config).

### Approval gates (north star)

Pause a task mid-execution and wait for a human decision on your phone:

```javascript
// Task pauses here, sends an actionable notification, resumes when you respond
const decision = await notify.ask("Send report to all 500 users?", {
  timeout: "1h",              // fail the run if no response within 1 hour
  options: ["approve", "reject"]
})

if (decision !== "approve") {
  log.info("rejected by user, aborting")
  return
}

await sendReport()
```

The run is stored as `suspended` in sqlite. When you tap Approve or Reject in the notification, dicode resumes the run with your answer. If the timeout expires, the run fails with a timeout error.

---

## Task → Orchestrator API

Tasks are not isolated black boxes — they can communicate back to the orchestrator while running. The `dicode` global provides this two-way channel.

### Intermediate progress

Stream progress updates to the WebUI run log in real time:

```javascript
const emails = await fetchAllEmails()
for (let i = 0; i < emails.length; i++) {
  dicode.progress(`processing ${i + 1} of ${emails.length}`, {
    done: i + 1,
    total: emails.length,
    percent: Math.round(((i + 1) / emails.length) * 100)
  })
  await processEmail(emails[i])
}
```

Progress events are streamed via SSE to the WebUI. Long-running tasks no longer look frozen.

### Querying orchestrator state

```javascript
// Check if another task is currently running
const backupRunning = await dicode.isRunning("nightly-backup")
if (backupRunning) {
  log.warn("backup in progress, skipping to avoid conflict")
  return
}
```

### Imperative task dispatch

Chain triggers are **declarative**: TaskB says "I follow TaskA" and TaskA doesn't know. Sometimes you need **imperative** dispatch — the running task explicitly decides to fire another:

```javascript
const result = await analyzeMetrics()

if (result.anomalies.length > 0) {
  // fire alert task with context — only when needed, not always
  await dicode.trigger("send-pagerduty-alert", {
    anomalies: result.anomalies,
    severity: result.maxSeverity
  })
}

return result
```

`dicode.trigger()` fires the target task asynchronously (fire-and-forget). The current run does not wait for it to complete.

### `dicode` global reference

| Method | Description |
|---|---|
| `dicode.progress(msg, data?)` | Stream intermediate progress to WebUI |
| `dicode.notify(msg, opts?)` | Send notification through configured provider |
| `dicode.trigger(taskId, input?)` | Imperatively fire another task |
| `dicode.isRunning(taskId)` | Returns `true` if task has an active run |
| `dicode.ask(question, opts)` | *(north star)* Suspend run, wait for human input |

### Chain vs `dicode.trigger()` — when to use each

| | Chain trigger | `dicode.trigger()` |
|---|---|---|
| Coupling | None — TaskB declares dependency | TaskA knows about TaskB |
| Condition | Always fires on task completion | Task code decides |
| Fan-out | Multiple tasks can chain from same source | Explicit, one at a time |
| Use when | Standard pipeline steps | Conditional dispatch, error escalation |

---

## Local Development

You don't need a git round-trip to write and test tasks. Add a `local` source pointing at a working directory and dicode will pick up changes instantly via filesystem watching (~100ms).

### Setup

```yaml
# dicode.yaml
sources:
  - type: git
    url: https://github.com/you/my-tasks   # stable tasks from git
    branch: main
    auth: { type: token, token_env: GITHUB_TOKEN }

  - type: local
    path: ~/tasks-dev                       # active development here
    watch: true
```

### Workflow

```bash
# 1. create a new task directory
mkdir ~/tasks-dev/my-new-task
```

```yaml
# ~/tasks-dev/my-new-task/task.yaml
name: my-new-task
trigger:
  manual: true
runtime: js
```

```javascript
// ~/tasks-dev/my-new-task/task.js
log.info("hello from my new task")
```

```bash
# 2. dicode detects the new directory within ~100ms and registers the task
# 3. trigger it manually from the UI or API to test
curl -X POST http://localhost:8080/api/tasks/my-new-task/run

# 4. edit task.js — dicode reloads immediately on save, no restart needed

# 5. when ready, promote to the git repo
dicode task commit my-new-task --to https://github.com/you/my-tasks
# moves the directory into the git repo clone, commits, pushes
# the git source takes over ownership on next sync
```

### Why separate directories

Keeping the local dev directory and the git repo clone separate means there's no conflict between your edits and git pulls. The git source never touches `~/tasks-dev`, and the local source never touches the git clone. Both feed the same reconciler independently.

### AI generation in dev mode

When you use the WebUI to generate a task via AI, by default the generated code is written to your local source directory first (if one is configured), so you can test it immediately. Confirming in the UI then commits it to the target git repo.

---

## Task Chaining

Tasks can be chained so the output of one becomes the input of the next. The downstream task declares the dependency — the upstream task doesn't need to know anything about it.

### Basic chain

```yaml
# tasks/fetch-emails/task.yaml
name: fetch-emails
trigger:
  cron: "0 9 * * *"
```

```javascript
// tasks/fetch-emails/task.js
const emails = await fetchInbox()
return { emails, count: emails.length }   // return value is passed downstream
```

```yaml
# tasks/send-slack-digest/task.yaml
name: send-slack-digest
trigger:
  chain:
    from: fetch-emails   # fires when fetch-emails succeeds
    on: success          # success (default) | failure | always
```

```javascript
// tasks/send-slack-digest/task.js
log.info("got emails", { count: input.count })   // `input` = fetch-emails return value
await http.post("https://slack.com/api/chat.postMessage", {
  headers: { Authorization: `Bearer ${env.SLACK_TOKEN}` },
  body: { channel: params.slack_channel, text: formatDigest(input.emails) }
})
```

### Longer chains

Chains can be as long as you need — each task just declares which task it follows:

```
fetch-emails → send-slack-digest → archive-processed-emails → cleanup-old-archives
```

### Failure handling

```yaml
trigger:
  chain:
    from: fetch-emails
    on: failure   # only fires if fetch-emails fails — useful for alerting
```

```yaml
trigger:
  chain:
    from: fetch-emails
    on: always    # fires regardless of outcome
```

### Constraints

- **Linear chains only** — one upstream task per chain trigger. Fan-out (one task triggering many) works naturally since multiple tasks can declare `from: same-task`. Fan-in (waiting for multiple tasks) is not supported yet.
- **Output must be JSON-serializable** — tasks are not a data pipeline; keep outputs small (under 1MB).
- **Cycle detection** — dicode detects cycles (A→B→A) when tasks are loaded and refuses to register them.
- **Chained runs are linked** — in the UI, a chain run shows as a child of the parent run, so you can trace the full execution path.

### North star: pipeline files

For complex DAGs (parallel steps, fan-in, conditionals), a future `pipeline.yaml` format will let you define multi-step workflows as a single unit. For now, linear chains handle the vast majority of real use cases.

---

## Testing & Validation

Every task can be validated statically and tested with mocked globals — no live credentials, no network, no side effects. This is the foundation for CI guardrails on your tasks repo.

### Four layers

| Layer | Command | What it catches |
|---|---|---|
| Static validation | `dicode task validate` | Schema errors, JS syntax, chain cycles |
| Unit tests | `dicode task test` | Logic bugs, wrong HTTP calls, bad return values |
| Dry run | `dicode task run --dry-run` | Secret resolution, correct endpoints |
| CI | auto on push | Regressions before merge |

---

### Layer 1 — Static validation

```bash
dicode task validate morning-email-check   # single task
dicode task validate --all                 # every loaded task
```

Checks (in order):
1. `task.yaml` parses and passes schema validation
2. JS compiles without syntax errors (goja compile-without-execute)
3. All `env:` vars have a matching secret registered (warning, not error)
4. Chain cycles — if this task is chained, no circular dependency exists

Exit code 1 on any failure. Structured error output with file and line number where possible.

---

### Layer 2 — Unit tests (`task.test.js`)

Place a `task.test.js` file in the task directory. It's picked up automatically:

```
tasks/morning-email-check/
├── task.yaml
├── task.js
└── task.test.js    ← optional, runs with `dicode task test`
```

The test file runs in a goja runtime with **mock globals** injected. Call `runTask()` to evaluate `task.js` inside the same runtime — the task uses the mocks transparently.

```javascript
// task.test.js

test("sends digest to Slack when emails exist", async () => {
  // mock outbound HTTP
  http.mock("GET", "https://gmail.googleapis.com/*", {
    status: 200,
    body: { messages: [{ snippet: "Meeting at 3pm" }, { snippet: "Hello" }] }
  })
  http.mock("POST", "https://slack.com/api/chat.postMessage", { ok: true })

  // mock secrets and params
  env.set("SLACK_TOKEN", "xoxb-test")
  env.set("GMAIL_TOKEN", "ya29-test")
  params.set("slack_channel", "#test")

  // run the task
  const result = await runTask()

  // assert on return value
  assert.equal(result.count, 2)

  // assert on HTTP calls made
  assert.httpCalled("POST", "https://slack.com/api/chat.postMessage")
  assert.httpCalledWith("POST", "https://slack.com/api/chat.postMessage", {
    body: { channel: "#test" }
  })
})

test("skips Slack when inbox is empty", async () => {
  http.mock("GET", "https://gmail.googleapis.com/*", {
    status: 200,
    body: { messages: [] }
  })

  await runTask()

  assert.httpNotCalled("POST", "https://slack.com/api/chat.postMessage")
})
```

#### Test globals reference

| Global | Description |
|---|---|
| `test(name, fn)` | Define a test case. `fn` can be async. |
| `runTask()` | Evaluate `task.js` in the current test runtime. Returns the task's return value. |
| `http.mock(method, urlPattern, response)` | Intercept matching HTTP calls. `urlPattern` supports `*` wildcards. |
| `http.mockOnce(method, urlPattern, response)` | Same but only matches the first call. |
| `env.set(key, value)` | Set a mock env var for this test. |
| `params.set(key, value)` | Set a mock param for this test. |
| `kv.set(key, value)` | Pre-populate the kv store. |
| `assert.equal(a, b)` | Deep equality assertion. |
| `assert.ok(val)` | Assert truthy. |
| `assert.throws(fn)` | Assert that `fn` throws. |
| `assert.httpCalled(method, urlPattern)` | Assert an HTTP call was made. |
| `assert.httpCalledWith(method, url, opts)` | Assert call was made with specific body/headers. |
| `assert.httpNotCalled(method, urlPattern)` | Assert no matching HTTP call was made. |

Each `test()` call gets a **fresh mock state** — mocks from one test don't leak into the next.

#### Running tests

```bash
dicode task test morning-email-check

# ✓ sends digest to Slack when emails exist  (38ms)
# ✓ skips Slack when inbox is empty          (11ms)
# 2 passed, 0 failed

dicode task test --all   # run tests for every task that has task.test.js
```

---

### Layer 3 — Dry run

Run the full task execution path with real secrets and real logic — but intercept all outbound HTTP calls and log them instead of sending:

```bash
dicode task run morning-email-check --dry-run
```

```
[dry-run] Secrets resolved: SLACK_TOKEN=✓  GMAIL_TOKEN=✓
[dry-run] GET https://gmail.googleapis.com/gmail/v1/users/me/messages?...
[dry-run]   → intercepted, not executed
[dry-run] POST https://slack.com/api/chat.postMessage
[dry-run]   body: { "channel": "#devops", "text": "Morning digest..." }
[dry-run]   → intercepted, not executed
[dry-run] Task returned: { count: 3 }
[dry-run] Duration: 120ms
```

Useful for verifying that secrets resolve correctly and the task targets the right endpoints before a live run.

---

### Layer 4 — CI integration

Generate a CI config for your tasks repo:

```bash
dicode ci init --github    # creates .github/workflows/dicode.yml
dicode ci init --gitlab    # creates .gitlab-ci.yml
```

Generated GitHub Actions workflow:

```yaml
# .github/workflows/dicode.yml
name: Validate tasks
on: [push, pull_request]

jobs:
  validate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: dicode/setup-action@v1   # downloads the dicode binary
      - name: Validate all tasks
        run: dicode task validate --all
      - name: Run task tests
        run: dicode task test --all
```

CI runs entirely **offline** — no secrets needed, no database, no live network. Validation and unit tests only use the files in the repo.

**What CI catches on every PR:**
- Broken `task.yaml` (typo in cron expression, missing name, invalid chain)
- JS syntax errors
- Chain cycles introduced by a new task
- Test regressions in any `task.test.js`

### AI-generated tests

When AI generates a task, it generates `task.test.js` alongside `task.js`. Both are shown in the diff before you confirm. If the test file has a syntax error, the AI retry loop fixes it automatically (up to 3 attempts) before showing you the result.

---

## MCP Server

dicode exposes an [MCP (Model Context Protocol)](https://modelcontextprotocol.io) server so any MCP-capable agent — Claude Code, Cursor, or a custom agent — can develop, test, and deploy tasks autonomously.

Enable in config (on by default):

```yaml
server:
  port: 8080
  mcp: true    # MCP endpoint at http://localhost:8080/mcp
```

### Connecting Claude Code

```json
{
  "mcpServers": {
    "dicode": {
      "url": "http://localhost:8080/mcp"
    }
  }
}
```

When `server.auth: true`, add an API key from the Security page:

```json
{
  "mcpServers": {
    "dicode": {
      "url": "http://localhost:8080/mcp",
      "headers": { "Authorization": "Bearer dck_your-key-here" }
    }
  }
}
```

### MCP tools reference

| Tool | Description |
|---|---|
| `list_tasks` | All registered tasks with id, trigger, status, last run time |
| `get_task(id)` | Full task content: task.yaml + task.js + task.test.js |
| `get_js_api` | Complete JS globals reference (http, kv, log, params, env, input) |
| `get_example_tasks` | 2-3 curated example tasks for few-shot context |
| `list_secrets` | Names of registered secrets (values never exposed) |
| `write_task_file(path, content)` | Write a file into the configured local dev source directory |
| `validate_task(id_or_path)` | Static validation — returns structured errors with line numbers |
| `test_task(id_or_path)` | Run task.test.js — returns pass/fail per test case with output |
| `dry_run_task(id)` | Full execution with intercepted HTTP — returns log + return value |
| `run_task(id)` | Trigger a live run — returns run ID for log streaming |
| `get_run_log(run_id)` | Fetch execution log for any run |
| `commit_task(id, source_id)` | Promote local task to git repo — returns commit SHA |

### Agent workflow example

When an agent receives "write a task that pings my API every 5 minutes and alerts Slack on failure":

```
1. list_tasks          → check no similar task exists
2. list_secrets        → confirm SLACK_TOKEN and API_URL are available
3. get_js_api          → understand available globals
4. get_example_tasks   → see real examples for code style reference
5. write_task_file("api-health-check/task.yaml", ...)
6. write_task_file("api-health-check/task.js", ...)
7. write_task_file("api-health-check/task.test.js", ...)
8. validate_task("api-health-check")   → fix any errors
9. test_task("api-health-check")       → fix failing tests
10. dry_run_task("api-health-check")   → verify endpoints and secrets
11. commit_task("api-health-check", "my-tasks")
```

The agent iterates on steps 8–10 until all checks pass, then commits. No human in the loop unless the agent needs clarification.

---

## Agent Skill

A skill file gives any AI agent the full context needed to develop dicode tasks correctly — task format, JS API, workflow rules, and common mistakes.

### Install

```bash
# Print skill to stdout (pipe into any agent or LLM)
dicode agent skill show

# Install for Claude Code
dicode agent skill install --claude-code
# writes to ~/.claude/skills/dicode-task-developer.md

# Install to a project's CLAUDE.md
dicode agent skill show >> CLAUDE.md
```

### What the skill teaches the agent

**Mandatory workflow** — the agent must follow this order every time:
1. `list_tasks` — avoid duplicating existing tasks
2. `list_secrets` — know what credentials are available before writing code
3. `get_js_api` — understand available globals and their signatures
4. `get_example_tasks` — use as few-shot style reference
5. Write `task.yaml` + `task.js` + `task.test.js` via `write_task_file`
6. `validate_task` — fix all errors before proceeding
7. `test_task` — all tests must pass (min: one happy path + one edge case)
8. `dry_run_task` — verify HTTP calls and secret resolution
9. `commit_task` — only when steps 6–8 are clean

**Hard rules the skill enforces:**
- Never commit if `validate_task` or `test_task` return errors
- Always write `task.test.js` — no exceptions
- `task.js` must return a JSON-serializable value (required for chain triggers)
- Never hardcode secrets — use `env.VARIABLE_NAME`; declare in `task.yaml env:`
- Keep tasks single-purpose — one task, one responsibility
- Output size under 1MB — tasks are not a data pipeline
- Use `http.get/post/...` — `fetch()` is not available

**Common mistakes the skill prevents:**
- Forgetting to declare env vars in `task.yaml env:` (they resolve as `undefined`)
- Returning non-serializable values (Date objects, functions, circular references)
- Writing tests that don't call `runTask()` and therefore test nothing
- Setting `trigger.chain.on` to anything other than `success`, `failure`, or `always`

---

## Web UI & API

### Dashboard

- Task list with status, last run time, next scheduled run
- Click a task to see its run history and logs
- Trigger any task manually with one click
- Create new tasks via AI prompt

### REST API

All UI actions are backed by a REST API you can call directly:

```bash
# List all tasks
curl http://localhost:8080/api/tasks

# Trigger a task manually
curl -X POST http://localhost:8080/api/tasks/morning-email-check/run

# Get run details and logs
curl http://localhost:8080/api/runs/<run-id>

# Stream live logs for a running task
curl http://localhost:8080/api/runs/<run-id>/stream   # Server-Sent Events

# Generate a task from a prompt
curl -X POST http://localhost:8080/api/ai/generate \
  -H "Content-Type: application/json" \
  -d '{"prompt": "ping my API every 5 minutes"}'

# Force an immediate git sync
curl -X POST http://localhost:8080/api/sync
```

### GitHub push webhook

To get near-instant task updates instead of waiting for the poll interval, add a webhook in your GitHub repo settings:

- **Payload URL**: `http://your-dicode-host:8080/hooks/git`
- **Content type**: `application/json`
- **Events**: `push`
- **Secret**: value of `server.secret` in your config

---

## Security

### Authentication

Enable the auth wall to gate all endpoints behind a passphrase:

```yaml
server:
  auth: true
  secret: "your-strong-passphrase"   # ≥ 16 chars recommended
  allowed_origins: []                 # empty = same-origin only
  # allowed_origins: ["https://your-domain.com"]   # if serving from a separate origin
```

When `auth: true`, every API call and page load requires a valid session. The SPA shows a login modal — no page reload. Wrong passwords are rate-limited (429 after 5 attempts per IP).

### Trusted browser (30-day device tokens)

Check **"Trust this browser for 30 days"** at login to issue a long-lived device cookie. On return visits the SPA silently renews your session — no login prompt unless the device is revoked.

Manage trusted devices from the **Security** page (`/security`): see all devices, revoke individually, or use the emergency **Logout all devices** button.

### Webhook HMAC authentication

Secure any webhook endpoint with a per-task shared secret:

```yaml
# task.yaml
trigger:
  webhook: /hooks/my-task
  webhook_secret: "${MY_WEBHOOK_SECRET}"   # resolved from secrets chain
```

dicode verifies `X-Hub-Signature-256: sha256=<hmac>` before the task script runs. The format is GitHub-compatible — point a GitHub webhook at a dicode endpoint with the same secret and it works out of the box. Requests older than 5 minutes are rejected (replay protection via `X-Dicode-Timestamp`).

See `examples/github-push-webhook/` for a full working example.

### MCP API key authentication

When `server.auth: true`, the MCP endpoint requires a bearer token:

```
Authorization: Bearer dck_<your-key>
```

Generate keys from the **Security** page. Keys are stored as SHA-256 hashes; the raw value is shown only once at creation. Revoke any key individually from the same page.

**Connecting Claude Code with a key:**

```json
{
  "mcpServers": {
    "dicode": {
      "url": "http://localhost:8080/mcp",
      "headers": { "Authorization": "Bearer dck_your-key-here" }
    }
  }
}
```

### Task isolation

Each task run gets a fresh JS runtime instance. Tasks share no memory. The JS environment is sandboxed — there is no `exec` or direct network binding available. Tasks can only make outbound HTTP calls and read env vars explicitly listed in `task.yaml`.

**Environment variables**: Tasks can only access env vars they declare in their `task.yaml` `env:` list. Undeclared vars are not visible even if set on the host.

**Git access**: dicode needs read access to your tasks repo (to pull changes) and write access (to commit AI-generated tasks). Use a fine-grained GitHub token scoped to the tasks repo only.

**AI-generated code**: Generated code is always shown as a diff before committing. Review it before confirming. The code runs with the same permissions as any other task — no special privileges.

### Deployment checklist

Before exposing dicode outside localhost:

- `server.auth: true` with a strong passphrase (`server.secret`)
- TLS terminated at a reverse proxy (nginx / Caddy) — dicode listens on localhost only
- `server.allowed_origins` set to your exact WebUI origin (if separate from the API host)
- Webhook tasks using `webhook_secret:` for any public-facing endpoint
- MCP API key generated from the Security page and set in your agent config
- `dicode.yaml` not world-readable (`chmod 600`)

---

## Deployment

### Desktop app

The default mode. Runs on your laptop with a tray icon, OS notifications, and automatic startup on login.

```bash
# Install the binary
brew install dicode            # macOS
scoop install dicode           # Windows
# or download from releases page

# Start dicode
dicode

# Install to run automatically on login
dicode service install
```

The tray icon right-click menu has a **"Start on login"** toggle. Under the hood this writes a LaunchAgent (macOS), XDG autostart entry (Linux), or Registry key (Windows).

### Headless server

For VPS, homelab, or any machine without a desktop session. Auto-detected when `$DISPLAY` is absent.

```bash
# Run directly
dicode --headless --config /etc/dicode/dicode.yaml

# Install as a system service (systemd on Linux, Windows Service on Windows)
dicode service install --headless
dicode service start
dicode service status
dicode service logs
```

### Docker

```bash
docker run -d \
  --name dicode \
  -e GITHUB_TOKEN=ghp_... \
  -e ANTHROPIC_API_KEY=sk-ant-... \
  -p 8080:8080 \
  -v ~/.dicode:/data \
  dicode/dicode
```

```yaml
# docker-compose.yml
services:
  dicode:
    image: dicode/dicode
    ports:
      - "8080:8080"
    volumes:
      - ./data:/data
      - ./dicode.yaml:/config/dicode.yaml
    environment:
      - GITHUB_TOKEN
      - ANTHROPIC_API_KEY
    restart: unless-stopped
```

The official Docker image sets `DICODE_HEADLESS=true` automatically — no tray, no desktop notifications. Configure mobile push notifications via `ntfy` to get alerts from headless deployments.

---

## Webhook Relay

Webhooks need a publicly reachable URL. If dicode is running on your laptop behind NAT, there's no public URL — until now.

Connect your dicode account and get a stable public URL instantly:

```
https://dicode.app/u/{your-uid}/hooks/my-task
```

dicode maintains a persistent WebSocket tunnel to `dicode.app`. When a webhook hits your URL, it's forwarded to your local instance in real time. Works behind any NAT, VPN, or firewall. No port forwarding, no ngrok.

### Setup

```bash
# Authenticate with dicode.app
dicode relay login

# Check tunnel status and your webhook URL
dicode relay status
# ● Connected  →  https://dicode.app/u/abc123/hooks/
```

Your webhook URL for a task is:
```
https://dicode.app/u/{uid}/hooks/{webhook-path-from-task.yaml}
```

### Relay limits

| | Free | Pro |
|---|---|---|
| Deliveries/month | 500 | Unlimited |
| URL format | `dicode.app/u/{uid}/...` | `{name}.dicode.app/...` |
| Payload replay | No | 7 days |

Self-hosted **server** deployments don't need the relay — expose port 8080 directly and use your own domain.

---

## Service Management

```bash
dicode service install            # install as OS service / startup item
dicode service install --headless # headless mode (no tray, for servers)
dicode service start
dicode service stop
dicode service restart
dicode service status
dicode service logs
dicode service uninstall
```

---

## Pricing

Dicode is **open source and free to self-host** — no feature limits on the core engine.

| | Self-hosted | Cloud Free | Cloud Pro | Team | Enterprise |
|---|---|---|---|---|---|
| Price | Free | Free | ~$12/mo | ~$20/seat/mo | Custom |
| Git repos | Public only | Public only | Public + Private | Public + Private | Public + Private |
| Database | SQLite | SQLite | Managed | Managed | BYO (Postgres/MySQL) |
| Tasks | Unlimited | 3 | Unlimited | Unlimited | Unlimited |
| Runs/month | Unlimited | 100 | Unlimited | Unlimited | Unlimited |
| AI generations | BYO API key | 10 | Unlimited | Unlimited | Custom model |
| Webhook relay | 500/mo | 500/mo | Unlimited + custom domain | Unlimited | Self-managed |
| Users | 1 | 1 | 1 | Unlimited | Unlimited |
| RBAC + audit log | — | — | — | ✅ | ✅ |
| SSO / SAML | — | — | — | — | ✅ |
| SLA | — | — | — | — | ✅ |

**Self-hosted users**: bring your own Anthropic API key for unlimited AI generations. Private git repos require a paid plan only on `dicode.cloud` — self-hosted has no such restriction.

See [BUSINESSPLAN.md](./BUSINESSPLAN.md) for full business model documentation.

---

## Project Structure

```
dicode/
├── cmd/dicode/         # binary entrypoint, CLI subcommands
├── pkg/
│   ├── config/         # config loading
│   ├── task/           # task spec (task.yaml) + content hashing
│   ├── runtime/js/     # goja JS runtime + injected globals
│   ├── source/         # Source interface, git + local implementations
│   ├── trigger/        # cron, webhook, manual, chain trigger engine
│   ├── registry/       # in-memory task registry + sqlite run log
│   ├── db/             # database abstraction (sqlite / postgres / mysql)
│   ├── secrets/        # provider chain, local encrypted store, env fallback
│   ├── notify/         # Notifier interface, ntfy, gotify, desktop, noop
│   ├── tray/           # system tray icon (systray)
│   ├── onboarding/     # first-run wizard (config generation, local vs git choice)
│   ├── relay/          # WebSocket tunnel client (dicode.app relay)
│   ├── service/        # OS service management (systemd, LaunchAgent, WinSvc)
│   ├── testing/        # task test harness (mock globals, assert, runTask)
│   ├── mcp/            # MCP server (wraps registry, runner, secrets)
│   ├── agent/          # agent skill file (embedded markdown)
│   ├── store/          # task store installer (dicode task install)
│   ├── ai/             # Claude API integration + prompt builder
│   └── webui/          # HTTP server, REST API, HTMX frontend
├── dicode.yaml         # example config
└── go.mod
```

---

## Comparison

| | dicode | Windmill | n8n | Airflow |
|---|---|---|---|---|
| Single binary | ✅ | ❌ (requires Postgres) | ❌ | ❌ |
| Git as source of truth | ✅ | partial | ❌ | ❌ |
| Auto-sync from git | ✅ | manual deploy | ❌ | ❌ |
| AI task generation | ✅ | ✅ | ❌ | ❌ |
| Code-first tasks | ✅ | ✅ | partial | ✅ |
| Task sharing / store | ✅ | ❌ | ❌ | ❌ |
| Self-contained (no infra) | ✅ | ❌ | ❌ | ❌ |

---

## License

MIT
