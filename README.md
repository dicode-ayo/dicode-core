# dicode

**GitOps-native task orchestrator with AI generation.**

Tell an AI what you want to automate. It writes the code, commits it to your git repo, and `dicode` picks it up automatically — no redeployment, no config files, no YAML pipelines.

> "Generate a task that checks my Gmail every morning and posts a digest to #devops on Slack."

That's it. The task appears in your dashboard within seconds.

---

> [!WARNING]
> **Alpha software — not yet human security-reviewed.**
>
> dicode (and the companion [dicode-relay](https://github.com/dicode-ayo/dicode-relay)) was written by AI. The crypto-adjacent code paths — ECDSA P-256 handshake, ECIES (ECDH + AES-256-GCM + HKDF-SHA256) token delivery, TOFU broker pubkey pinning, ECDSA-signed delivery envelopes, OAuth broker flow — have **not** undergone human security review yet. Current tests pass and the design looks sensible, but "sensible on paper, reviewed only by the AI co-author" is not the bar to assume for software handling OAuth tokens and tunneling inbound HTTP on your behalf.
>
> What this means for v0.1.0 alpha users:
> - Use it with **throwaway OAuth apps** in development only.
> - **Do not** connect a production daemon to `relay.dicode.app` with long-lived secrets.
> - **Expect breaking protocol changes** as the crypto surface is audited.
>
> Tracked in epic [#189](https://github.com/dicode-ayo/dicode-core/issues/189). Pre-GA hardening migrations in [#192](https://github.com/dicode-ayo/dicode-core/issues/192).

---

## What it is

`dicode` is a single Go binary that:

- **Watches task sources** and reconciles them automatically (like ArgoCD, but for automation tasks) — git repos, local directories, or hierarchical **TaskSet** manifests
- **Executes tasks** on a schedule (cron), via HTTP webhook, manually, on chain completion, or as always-running daemons
- **Lets AI write your tasks** from natural language — the generated code lives in your source so you can read, review, and modify it
- **Serves a web UI** for monitoring runs, viewing logs, triggering tasks, and managing sources — the dashboard itself is a webhook task
- **Exposes an MCP server** at `/mcp` so AI agents (Claude Code, Cursor) can list tasks, trigger runs, and control dev mode
- **Receives webhooks behind NAT** via a built-in WebSocket relay tunnel — stable public URLs without port forwarding or ngrok
- **Serves custom webhook UIs** — webhook tasks can include `index.html` for browser-based interfaces with the auto-injected `dicode.js` SDK

Tasks are TypeScript (Deno), Python (uv), Docker containers, or Podman containers. You can write them yourself or have AI generate them. All approaches produce the same artifact: a folder with `task.yaml` + script.

dicode is a single binary. Run `dicode daemon` to start the engine, or use any CLI subcommand — the daemon is auto-started in the background if it isn't already running.

---

## How it works

```
┌─────────────────────────────────────────────────────────────────┐
│                         tasks git repo                          │
│  tasks/                                                         │
│  ├── morning-email-check/                                       │
│  │   ├── task.yaml   ← trigger config, params, env vars        │
│  │   └── task.ts     ← TypeScript logic (Deno runtime)         │
│  └── weekly-report/                                             │
│      ├── task.yaml                                              │
│      └── task.py     ← Python logic (uv runtime)               │
└──────────────────────┬──────────────────────────────────────────┘
                       │  git pull (every 30s or on push webhook)
                       ▼
┌─────────────────────────────────────────────────────────────────┐
│  dicode daemon                                                  │
│                                                                 │
│  ┌─────────────┐   ┌──────────────┐   ┌──────────────────────┐ │
│  │ Reconciler  │──▶│   Registry   │──▶│   Trigger Engine     │ │
│  │             │   │              │   │  cron / webhook /    │ │
│  │ add/remove/ │   │  in-memory   │   │  manual / chain /   │ │
│  │ update tasks│   │  task state  │   │  daemon              │ │
│  └─────────────┘   └──────────────┘   └──────────┬───────────┘ │
│                                                  ▼             │
│  ┌─────────────┐   ┌──────────────┐   ┌──────────────────────┐ │
│  │  AI Gen     │   │   Web UI     │   │ Runtimes             │ │
│  │             │   │   + REST API │   │  Deno (TypeScript)   │ │
│  │  prompt →   │   │   + MCP      │   │  Python (uv)         │ │
│  │  code →     │   │              │   │  Docker / Podman     │ │
│  │  commit     │   │  dashboard   │   └──────────┬───────────┘ │
│  └─────────────┘   │  logs/runs   │              │             │
│                     └──────────────┘   ┌──────────────────────┐ │
│  ┌─────────────┐                      │   SQLite             │ │
│  │ Relay       │ WSS to relay server  │   runs / kv / keys   │ │
│  │ Client      │──────────────────▶   │   sessions / secrets │ │
│  └─────────────┘                      └──────────────────────┘ │
└─────────────────────────────────────────────────────────────────┘
   ▲                                                    ▲
   │ unix socket (control)                              │
   ▼                                                    │
┌───────────────┐                                       │
│  dicode (CLI) │  run / list / logs / secrets / status │
└───────────────┘───────────────────────────────────────┘
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

Start local, add git when you're ready — no rework required. Add a git source to `dicode.yaml`, copy your task directories into the repo, and push. The reconciler takes over automatically.

---

## Run with Docker

```sh
docker pull dicodeayo/dicode-core:latest
docker run -d --name dicode \
  -p 8080:8080 \
  -v dicode-data:/data \
  dicodeayo/dicode-core:latest
```

Open http://localhost:8080. SQLite, encrypted secrets, source clones, and
the generated `dicode.yaml` all live under `/data` in the named volume
`dicode-data`, so the dashboard passphrase, registered tasks, and run
history survive `docker rm` + `docker run` against the same volume.

Multi-arch images are published for `linux/amd64` and `linux/arm64` and
mirrored on GHCR: `ghcr.io/dicode-ayo/dicode-core`. Pin to a specific
release: `dicodeayo/dicode-core:0.1.1` (or `:0.1` / `:0`).

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

runtime: deno   # deno (TypeScript/JS) | python | docker | podman

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
| --- | --- | --- |
| `cron` | `"0 9 * * *"` | Standard 5-field cron expression |
| `webhook` | `"/hooks/my-task"` | HTTP POST to this path triggers a run |
| `manual` | `true` | Only triggerable from UI or API |
| `chain` | `from: other-task` | Triggered when another task completes |
| `daemon` | `true` | Starts on app start, restarted on exit |

**Additional task.yaml fields:**

```yaml
on_failure_chain: failure-monitor   # override global default; "" to disable
mcp_port: 3000                      # declare MCP server port (daemon tasks only)
security:
  allowed_tasks: ["*"]              # tasks this script may call via dicode.run_task()
  allowed_mcp: ["github-mcp"]       # MCP daemon tasks this script may call via mcp.call()
```

**Deno runtime** — tasks use TypeScript/JavaScript with the Deno runtime (auto-installed). npm packages can be imported inline with `import ... from "npm:package"`.

**Python runtime** — tasks use Python with uv (auto-installed). PEP 723 inline dependency declarations supported.

**Docker runtime** — run containers instead of scripts. Live log streaming, kill support, daemon-compatible:

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

### Task script (task.ts / task.py)

The task script. Deno (TypeScript) and Python tasks communicate with the daemon over a Unix socket IPC protocol. The following globals are injected automatically — no imports needed.

#### `params` — task parameters from task.yaml (with user overrides)

```typescript
// Deno (TypeScript) — all methods are async
const channel = await params.get("slack_channel")   // string | null
const all = await params.all()                       // Record<string, string>
```

```python
# Python — sync and async variants
channel = params.get("slack_channel")      # str | None
channel = params.get("slack_channel", "#general")  # with default
all_params = params.all()                  # dict
```

#### `env` — environment variables declared in task.yaml `env:` list

```typescript
// Deno — uses native Deno.env
const token = Deno.env.get("SLACK_TOKEN")   // only declared vars are accessible
```

```python
# Python — env.get() wrapper
token = env.get("SLACK_TOKEN")             # str | None
token = env.get("SLACK_TOKEN", "fallback") # with default
```

#### `kv` — persistent key-value store (per-task, survives restarts)

```typescript
// Deno
await kv.set("last_id", "msg_12345")
const id = await kv.get("last_id")         // unknown | null
await kv.delete("last_id")
const all = await kv.list("prefix_")       // Record<string, unknown>
```

```python
# Python
kv.set("last_id", "msg_12345")
id = kv.get("last_id")                    # Any | None
kv.delete("last_id")
all = kv.list("prefix_")                  # dict
```

#### `input` — payload from upstream (chain triggers and webhooks)

```typescript
// Deno — module-level awaited value
console.log(input.count)                   // chain: upstream return value
console.log(input.body)                    // webhook: request body
```

```python
# Python — module-level variable
print(input["count"])
```

The `input` global contains whatever the upstream task returned (chain) or the webhook request body. Always JSON-serializable.

#### `output` — rich output rendering (HTML, text, images, files)

```typescript
// Deno
await output.html("<h1>Report</h1>", { data: { count: 42 } })  // HTML for humans, data for chains
await output.text("Done: 42 items processed")
await output.image("image/png", base64EncodedContent)
await output.file("report.csv", csvContent, "text/csv")
```

```python
# Python
output.html("<h1>Report</h1>", data={"count": 42})
output.text("Done: 42 items processed")
output.image("image/png", base64_content)
output.file("report.csv", csv_content, "text/csv")
```

Output is stored alongside the run in SQLite. The WebUI reads the content type to decide how to render it (iframe for HTML, `<pre>` for text, `<img>` for images, download button for files).

#### Logging

```typescript
// Deno — use native console (captured as structured logs)
console.log("starting email check")
console.warn("rate limited")
console.error("failed", error.message)
```

```python
# Python — log object with methods
log.info("starting email check")
log.warn("rate limited")
log.error("failed", error_message)
```

#### `dicode` — task orchestration

Run and await other tasks, inspect the registry, and read config. Requires `security.allowed_tasks` in `task.yaml`.

```typescript
// Deno
const result = await dicode.run_task("send-report", { channel: "#ops" })
const tasks = await dicode.list_tasks()
const runs = await dicode.get_runs("send-report", { limit: 5 })

// Secrets management (Deno only)
await dicode.secrets_set("MY_TOKEN", "new-value")
await dicode.secrets_delete("OLD_TOKEN")
```

```python
# Python
result = dicode.run_task("send-report", {"channel": "#ops"})
tasks = dicode.list_tasks()
runs = dicode.get_runs("send-report", limit=5)
```

```yaml
# task.yaml — must declare which tasks this task is allowed to call
security:
  allowed_tasks:
    - "send-report"   # specific task ID
    - "*"             # or allow all
```

#### `mcp` — call MCP server tools

Connect to any MCP daemon task (Docker/Python/Deno process that exposes an MCP server on a declared port).

```typescript
// Deno
const tools  = await mcp.list_tools("github-mcp")
const result = await mcp.call("github-mcp", "search_repositories", { query: "dicode" })
```

```python
# Python
tools = mcp.list_tools("github-mcp")
result = mcp.call("github-mcp", "search_repositories", {"query": "dicode"})
```

```yaml
# task.yaml
security:
  allowed_mcp:
    - "github-mcp"   # task ID of the daemon that declares mcp_port
```

#### HTTP requests

Deno and Python tasks use their native HTTP libraries — no special `http` global:

```typescript
// Deno — native fetch
const res = await fetch("https://api.example.com/data", {
  headers: { "Authorization": `Bearer ${Deno.env.get("MY_TOKEN")}` }
})
const data = await res.json()
```

```python
# Python — use any library (declare in PEP 723 inline deps)
# /// script
# dependencies = ["httpx"]
# ///
import httpx
res = httpx.get("https://api.example.com/data",
    headers={"Authorization": f"Bearer {env.get('MY_TOKEN')}"})
data = res.json()
```

#### Return values

```typescript
// Deno — return from the top-level script
return { count: 42, status: "ok" }   // must be JSON-serializable
```

```python
# Python — assign to the result variable
result = {"count": 42, "status": "ok"}  # must be JSON-serializable
```

Return values are passed to downstream chain-triggered tasks via `input`.

### Full example (Deno TypeScript)

```typescript
// task: morning-email-check (task.ts)
// Fetches recent emails via Gmail API and posts a digest to Slack

const since = await kv.get("last_check") as string ?? new Date(Date.now() - 86400000).toISOString()
const maxEmails = await params.get("max_emails") ?? "10"
const channel = await params.get("slack_channel") ?? "#general"

console.log("checking emails since", since)

const emailRes = await fetch(
  `https://gmail.googleapis.com/gmail/v1/users/me/messages?q=after:${since}+is:unread&maxResults=${maxEmails}`,
  { headers: { Authorization: `Bearer ${Deno.env.get("GMAIL_TOKEN")}` } }
)
const emails = await emailRes.json()

if (!emails.messages?.length) {
  console.log("no new emails")
  return { count: 0 }
}

const lines = emails.messages.map((m: { snippet: string }) => `• ${m.snippet}`)
const text = `*Morning email digest* (${lines.length} unread)\n${lines.join("\n")}`

await fetch("https://slack.com/api/chat.postMessage", {
  method: "POST",
  headers: {
    Authorization: `Bearer ${Deno.env.get("SLACK_TOKEN")}`,
    "Content-Type": "application/json",
  },
  body: JSON.stringify({ channel, text }),
})

await kv.set("last_check", new Date().toISOString())
console.log("digest sent", { count: lines.length, channel })
return { count: lines.length }
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

defaults:
  on_failure_chain: failure-monitor       # task to call whenever any task fails
                                          # receives input: { taskID, runID, status, output }
                                          # override per task with on_failure_chain: "" to disable

relay:
  enabled: false                          # enable WebSocket relay for public webhook URLs
  server_url: wss://relay.dicode.app      # relay server URL (wss:// for production)

runtimes:
  deno:
    version: ""                           # pin Deno version (empty = bundled default)
  python:
    version: "0.7.3"                      # pin uv version
    # disabled: true                      # disable this runtime entirely

log_level: info                           # "debug", "info", "warn", "error"
data_dir: ~/.dicode                       # where to store repo clones, sqlite db, etc.
```

**Config variables**: `${HOME}`, `${DATADIR}` (resolves to `data_dir`), and `${CONFIGDIR}` (resolves to the directory containing `dicode.yaml`) can be used in any string value.

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
> "Write a task.test.ts for this"

The panel calls the `buildin/dicodai` task (an `ai-agent` preset preloaded with the `dicode-task-dev` and `dicode-basics` skills, defaulting to OpenAI). With `OPENAI_API_KEY` set in your environment it works out of the box. The current task id and the file you're editing are sent along as context, and the agent's text reply appears in the chat — copy any generated code back into the editor yourself. Swap providers by overriding the dicodai preset in your own taskset, just like the `ai-agent-openai` / `ai-agent-ollama` / `ai-agent-groq` examples.

### Chat with an agent that calls your tasks

Dicode ships a built-in **ai-agent** task that gives you a full chat interface where the model can call any of your other dicode tasks as tools. Open `/hooks/ai` in the dashboard to start a conversation; the agent discovers every registered task, builds an OpenAI-compatible tool schema from each task's params, and invokes them on your behalf via `dicode.run_task()`.

Two vocabulary pieces worth knowing:

- **Tools** are other dicode tasks the agent can execute. Any task becomes a tool automatically — no extra registration.
- **Skills** are plain markdown files under `tasks/skills/` that get loaded into the agent's system prompt. Use them to teach the agent domain context, conventions, or workflows it wouldn't infer from task descriptions alone.

The built-in ships provider-agnostic — no network, no API keys. Choose your provider via taskset overrides (see `tasks/examples/taskset.yaml` for working Ollama / OpenAI / Groq presets), or pass `model` / `base_url` / `api_key_env` as params per request. Conversation history is persisted per `session_id` in the task's KV store and compacted into a running summary once it exceeds a configurable token budget.

See [docs/concepts/ai-agent.md](docs/concepts/ai-agent.md) for the full design.

---

## Task Store (planned)

Tasks can be shared as git repos. Any git repo with a `tasks/` directory is a valid task source — add it as a git source in `dicode.yaml` and the reconciler pulls tasks automatically.

A dedicated `dicode task install` CLI command and community registry are planned. When built, they will support:

```bash
dicode task install github.com/dicode-community/tasks/morning-email-check \
  --param slack_channel="#devops"
```

See [docs/concepts/task-store.md](docs/concepts/task-store.md) for the full design.

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
| Relay / public webhook URLs | ✅ enable `relay.enabled: true` in config |
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

1. Add a git source to `dicode.yaml`
2. Copy your task directories from the local source into the git repo
3. Git commit and push — the reconciler picks them up automatically

A `dicode task commit` CLI command to automate this is planned.

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

### Task-level notifications (north star)

A `notify` global for sending notifications from within task scripts is planned but not yet implemented in the Deno/Python SDKs. System-level notifications (on task failure/success) work automatically via the config above.

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

**Desktop notifications** are planned but not yet implemented. Mobile push (ntfy) works now.

**System tray icon** gives you quick access to the dashboard without opening a browser.

Enable in config or via flag:

```yaml
server:
  tray: true   # default: true when running interactively, false as a service
```

**Tray menu:**

```
┌─────────────────────────────┐
│ dicode                      │
├─────────────────────────────┤
│ Open Dashboard              │
│ Quit dicode                 │
└─────────────────────────────┘
```

The tray runs as a built-in Deno daemon task (`tasks/buildin/tray/`) using a portable systray helper binary — no CGo or GTK required. Works on Linux, macOS, and Windows. Disabled when running headless (`server.tray: false`).

### Approval gates (planned)

A future `notify.ask()` API will pause a task mid-execution and wait for a human decision via push notification. The run would be stored as `suspended` in SQLite and resumed when the user responds. See [docs/concepts/notifications.md](docs/concepts/notifications.md) for the design.

---

## Task → Orchestrator API

Tasks are not isolated black boxes — they can communicate back to the orchestrator while running. The `dicode` global provides this two-way channel.

### Running other tasks

Tasks can run other tasks and await their results. This is the primary way to compose behavior at runtime:

```typescript
// Deno — run another task and wait for it to complete
const result = await dicode.run_task("send-report", { channel: "#ops" })
// result: { runID, status, returnValue }
```

```python
# Python
result = dicode.run_task("send-report", {"channel": "#ops"})
```

Requires `security.allowed_tasks` in `task.yaml` — tasks opt in to which other tasks they can call.

### Querying orchestrator state

```typescript
// List all registered tasks (useful for building dynamic tool schemas)
const tasks = await dicode.list_tasks()

// Fetch recent run history for a specific task
const runs = await dicode.get_runs("nightly-backup", { limit: 5 })
```

### `dicode` global reference

| Method | Deno | Python | Description |
|---|---|---|---|
| `run_task(id, params?)` | `await dicode.run_task(...)` | `dicode.run_task(...)` | Run another task and await its result |
| `list_tasks()` | `await dicode.list_tasks()` | `dicode.list_tasks()` | List all registered tasks |
| `get_runs(id, opts?)` | `await dicode.get_runs(...)` | `dicode.get_runs(...)` | Fetch recent run history |
| `secrets_set(key, val)` | `await dicode.secrets_set(...)` | — | Store a secret (Deno only) |
| `secrets_delete(key)` | `await dicode.secrets_delete(...)` | — | Delete a secret (Deno only) |

### Chain vs `dicode.run_task()` — when to use each

| | Chain trigger | `dicode.run_task()` |
|---|---|---|
| Coupling | None — TaskB declares dependency | TaskA knows about TaskB |
| Condition | Always fires on task completion | Task code decides at runtime |
| Blocking | Async (fire-and-forget) | Synchronous (awaits result) |
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

The testing and validation system is designed with four layers. Not all are implemented yet — see status below.

### Four layers

| Layer | Command | What it catches | Status |
|---|---|---|---|
| E2E tests | Playwright | Full UI + API integration regressions | ✅ Implemented |
| Static validation | `dicode task validate` | Schema errors, syntax, chain cycles | Planned |
| Unit tests | `dicode task test` | Logic bugs, wrong HTTP calls, bad return values | ✅ Implemented (Deno; Python/Docker/Podman tracked as #159) |
| Dry run | `dicode task run --dry-run` | Secret resolution, correct endpoints | Planned |

> **Note**: E2E tests use Playwright and cover core UI flows, file changes, webhooks, config, and auth. Unit tests run via `dicode task test <task-id>` or `make test-tasks` — the CLI routes through the same executor as the MCP `test_task` tool so any caller (human, built-in agent, third-party AI) gets the same results. `task validate` and `task run --dry-run` are not yet implemented. Current CLI supports: `run`, `list`, `logs`, `status`, `ai`, `task test`, `secrets`, `relay`, `version`.

---

### Layer 1 — Static validation (planned)

```bash
dicode task validate morning-email-check   # single task
dicode task validate --all                 # every loaded task
```

Will check: `task.yaml` schema, script syntax, env var resolution, chain cycle detection.

---

### Layer 2 — Unit tests (`dicode task test`)

Write a sibling `task.test.ts` (or `.js` / `.mjs`) and run it through the task's runtime:

```bash
dicode task test buildin/webui           # via the daemon (CLI → IPC → executor)
make test-tasks                          # or directly: every task.test.* in tasks/buildin/
```

The harness (`tasks/sdk-test.ts`) provides in-memory mocks for the production SDK: `params.set`, `env.set`, `kv.set/get`, `http.mock` / `mockOnce`, `assert.*`, plus `runTask()` which invokes the task's default export. Each `test()` case gets a fresh mock state. See [tasks/buildin/webui/task.test.ts](tasks/buildin/webui/task.test.ts) and [tasks/buildin/ai-agent/task.test.ts](tasks/buildin/ai-agent/task.test.ts) for working examples.

The same executor is exposed as the `test_task` MCP tool, so any MCP-capable agent (Claude Code, Cursor) can drive testing without dicode-specific plumbing.

Runtime support: **Deno ✅**. Python, Docker, Podman tracked as #159.

### Layers 3–4 — Dry run, CI (planned)

- **Dry run**: `dicode task run --dry-run` with intercepted HTTP calls
- **CI integration**: `dicode ci init` to generate GitHub Actions / GitLab CI configs

See [docs/concepts/testing.md](docs/concepts/testing.md) for the full design.

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

**Implemented:**

| Tool | Description |
|---|---|
| `list_tasks` | All registered tasks with id, trigger, status, last run time |
| `get_task(id)` | Full task content: task.yaml + script source |
| `run_task(id)` | Trigger a live run — returns run ID |
| `test_task(id)` | Run the task's sibling `task.test.*` through its runtime. Returns pass/fail counts + full stdout. Deno supported; other runtimes return a clear "not yet supported" error (#159). |
| `list_sources` | List configured task sources with dev mode status |
| `switch_dev_mode(source, enabled, path?)` | Toggle dev mode for a source |

**Planned:**

| Tool | Description |
|---|---|
| `validate_task(id)` | Static validation with structured errors |
| `dry_run_task(id)` | Full execution with intercepted HTTP |
| `commit_task(id, source_id)` | Promote local task to git repo |
| `list_secrets` | Names of registered secrets |
| `write_task_file(path, content)` | Write a file into dev source directory |

### Agent workflow example

With the currently implemented tools, an agent can:

```
1. list_tasks     → check what already exists
2. get_task(id)   → read an existing task for reference
3. run_task(id)   → trigger a run and check the result
```

When the planned tools are implemented, the full workflow will be:

```
1. list_tasks → list_secrets → get_task (reference)
2. write_task_file (task.yaml + task.ts + task.test.ts)
3. validate_task → test_task → dry_run_task (iterate until clean)
4. commit_task (promote to git)
```

---

## Agent Skill

A skill file gives any AI agent the full context needed to develop dicode tasks correctly — task format, JS API, workflow rules, and common mistakes.

### Install

The skill lives at `tasks/skills/dicode-task-dev.md`. It is loaded automatically by the `buildin/dicodai` task (a preset of `buildin/ai-agent` preloaded with `dicode-task-dev,dicode-basics`) so the WebUI chat panel already benefits from it. To use it with a separate AI agent (Claude Code, your own chat UI, etc.) copy it into your project:

```bash
cat tasks/skills/dicode-task-dev.md >> CLAUDE.md
```

Add more skills by dropping markdown files with YAML frontmatter into `tasks/skills/` and listing them in the ai-agent's `skills` param.

### What the skill teaches the agent

**Recommended workflow** (steps 2-9 require planned MCP tools not yet implemented):
1. `list_tasks` — avoid duplicating existing tasks (implemented)
2. `list_secrets` — know what credentials are available (planned)
3. `get_task` — read an existing task for reference (implemented)
4. Write `task.yaml` + `task.ts` + `task.test.ts` via `write_task_file` (planned)
5. `validate_task` — fix all errors before proceeding (planned)
6. `test_task` — all tests must pass (planned)
7. `dry_run_task` — verify HTTP calls and secret resolution (planned)
8. `commit_task` — only when steps 5-7 are clean (planned)

**Hard rules the skill enforces:**
- Never commit if `validate_task` or `test_task` return errors
- Always write `task.test.js` — no exceptions
- `task.js` must return a JSON-serializable value (required for chain triggers)
- Never hardcode secrets — use `env.VARIABLE_NAME`; declare in `task.yaml env:`
- Keep tasks single-purpose — one task, one responsibility
- Output size under 1MB — tasks are not a data pipeline
- Deno tasks use native `fetch()`; Python tasks use any HTTP library declared in inline deps

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

# Ask the configured AI task (default: buildin/dicodai) to generate a task
# Requires an authenticated session cookie — easier via the CLI:
#   dicode ai "ping my API every 5 minutes"
# Or the REST API once logged in:
curl -X POST http://localhost:8080/api/ai/chat \
  -H "Content-Type: application/json" \
  -H "Cookie: dicode_secrets_sess=<your-session>" \
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

Each task run gets a fresh runtime instance (Deno subprocess for TypeScript, uv subprocess for Python, container for Docker/Podman). Tasks share no memory. Tasks can only make outbound network calls and read env vars explicitly listed in `task.yaml`.

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
# Install the binary (download from releases page)
# Start the daemon
dicode daemon

# Or use the CLI (auto-starts daemon if needed)
dicode list
```

### Headless server

For VPS, homelab, or any machine without a desktop session. Set `server.tray: false` in config.

```bash
# Run the daemon directly
dicode daemon
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

Enable the relay and get a stable public URL instantly — no accounts, no port forwarding, no ngrok:

```
https://relay.dicode.app/u/<uuid>/hooks/my-task
```

The daemon maintains a persistent WebSocket tunnel to the relay server. When a webhook hits your URL, it's forwarded to your local instance in real time. Works behind any NAT, VPN, or firewall.

### Setup

```yaml
# dicode.yaml
relay:
  enabled: true
  server_url: wss://relay.dicode.app   # or ws://localhost:5553 for local dev
```

On first start the daemon generates an ECDSA P-256 keypair and derives a stable UUID from the public key. The relay URL never changes as long as the daemon's database file is preserved.

The relay client authenticates via challenge-response (no passwords, no accounts) and automatically reconnects with exponential backoff on disconnect.

### What works through the relay

- Webhook task execution (`/hooks/*`)
- Webhook task UIs (HTML pages with the `dicode.js` SDK)
- Asset serving for webhook UIs (CSS, JS, images)
- dicode.js SDK serving (`/dicode.js`)

### OAuth broker (relay required)

When the relay is enabled, two built-in tasks handle the full OAuth dance for 14+ providers without requiring you to register an app with the provider:

```sh
dicode run buildin/auth-start provider=slack
# prints a signed /auth/slack URL — open it in a browser
# after consent, tokens land in SLACK_ACCESS_TOKEN (and friends)
```

The relay broker runs the code exchange on its side, ECIES-encrypts the token bundle to your daemon's P-256 public key, and forwards it over the existing WSS tunnel to a reserved `/hooks/oauth-complete` path. Plaintext tokens never cross the JS runtime — decrypt, parse, and secrets-write all happen in Go-process memory. See [docs/oauth.md](docs/oauth.md#broker-flow-relay-required) for the threat model and the full list of supported providers.

### Self-hosted relay

For high-security environments, run your own relay server instead of using `relay.dicode.app`. The relay lives in a separate repository: [dicode-ayo/dicode-relay](https://github.com/dicode-ayo/dicode-relay) — a Node.js service that combines the WebSocket tunnel, OAuth broker, multi-client support, and a status dashboard. It ships as a Docker image; see the relay repo for deployment details.

Self-hosted daemons that expose port 8080 directly don't need the relay at all.

---

## Service Management (planned)

OS service management (`dicode service install/start/stop/status/logs`) is designed but the CLI commands are not yet implemented. For now, run `dicode daemon` directly or manage it with systemd/launchd manually:

```bash
# Run directly
dicode daemon

# Or with systemd (create a unit file manually)
sudo systemctl start dicode
```

---

## Pricing

Dicode is **open source and free to self-host** — no feature limits on the core engine.

| | Self-hosted | Cloud Free | Cloud Pro | Team | Enterprise |
|---|---|---|---|---|---|
| Price | Free | Free | ~$12/mo | ~$20/seat/mo | Custom |
| Git repos | Unlimited | Public only | Public + Private | Public + Private | Public + Private |
| Database | SQLite | SQLite | Managed | Managed | BYO (Postgres/MySQL) |
| Tasks | Unlimited | 3 | Unlimited | Unlimited | Unlimited |
| Runs/month | Unlimited | 100 | Unlimited | Unlimited | Unlimited |
| AI generations | BYO API key | 10 | Unlimited | Unlimited | Custom model |
| Webhook relay | Self-hosted or dicode.app | dicode.app | Unlimited + custom domain | Unlimited | Self-managed |
| Users | 1 | 1 | 1 | Unlimited | Unlimited |
| RBAC + audit log | — | — | — | ✅ | ✅ |
| SSO / SAML | — | — | — | — | ✅ |
| SLA | — | — | — | — | ✅ |

**Self-hosted users**: bring your own Anthropic API key for unlimited AI generations. Private git repos require a paid plan only on `dicode.cloud` — self-hosted has no such restriction.

---

## Project Structure

```
dicode/
├── cmd/
│   └── dicode/         # single binary — CLI + daemon mode
├── pkg/
│   ├── config/         # config loading + validation
│   ├── task/           # task spec (task.yaml) + content hashing
│   ├── taskset/        # hierarchical task composition (TaskSet manifests)
│   ├── source/         # Source interface, git + local implementations
│   ├── trigger/        # cron, webhook, manual, chain, daemon trigger engine
│   ├── registry/       # in-memory task registry + sqlite run log + reconciler
│   ├── runtime/
│   │   ├── deno/       # Deno TypeScript/JS runtime (auto-installs deno binary)
│   │   ├── docker/     # Docker container runtime (live logs, kill, orphan cleanup)
│   │   ├── podman/     # Podman container runtime (rootless)
│   │   └── python/     # Python runtime (uv, PEP 723 inline deps)
│   ├── ipc/            # unified IPC: per-run task sockets + CLI control socket
│   ├── db/             # database abstraction (sqlite implemented; postgres/mysql stubs)
│   ├── secrets/        # provider chain: local encrypted (ChaCha20) + env fallback
│   ├── relay/          # WebSocket relay client + self-hosted server
│   ├── mcp/            # MCP server (JSON-RPC 2.0) + MCP client for daemon tasks
│   ├── notify/         # Notifier interface + ntfy provider
│   ├── tray/           # system tray icon (fyne.io/systray)
│   ├── onboarding/     # first-run config generation
│   ├── webui/          # HTTP server, REST API, auth, WebSocket hub
│   └── service/        # OS service management (interface defined, impls planned)
├── tasks/
│   ├── buildin/        # built-in tasks (webui, tray, alert, notify, ai-agent, dicodai)
│   ├── skills/         # shared markdown "skills" loaded into agent prompts (dicode-basics, dicode-task-dev)
│   ├── auth/           # legacy per-provider OAuth tasks (self-hosted flow)
│   └── examples/       # 13 example tasks (all runtimes and trigger types)
├── docs/               # comprehensive documentation (33 files)
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
| Multi-runtime (TS/Python/Docker) | ✅ | ✅ | ❌ | partial |
| MCP server for AI agents | ✅ | ❌ | ❌ | ❌ |
| Webhook relay (no ngrok) | ✅ | ❌ | ❌ | ❌ |
| Self-contained (no infra) | ✅ | ❌ | ❌ | ❌ |

---

## License

AGPL-3.0 — see [LICENSE](LICENSE).

The full engine is open source and free to self-host. The AGPL copyleft ensures the code stays open: you can use, modify, and deploy dicode freely, but if you offer it as a network service, you must share your modifications under the same license. This prevents cloud providers from strip-mining the code into a proprietary offering while keeping it fully open for self-hosters and contributors.
