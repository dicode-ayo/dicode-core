# Dicode — High-Level Design

## What it is
A single Go binary that watches a git repo of task scripts, reconciles them automatically (ArgoCD-style), executes them on schedule/webhook/manual, and lets AI generate task code from natural language.

---

## Components

### 1. Core Binary & Config
Single `dicode` binary. Configured via `dicode.yaml` (tasks repo URL, auth, server port, AI key, poll interval).

### 2. Task Spec
Each task = a folder in the tasks git repo:
```
tasks/morning-email-check/
├── task.yaml       ← name, trigger (cron/webhook/manual/chain/daemon), params, env, fs
├── task.js         ← JS logic
└── task.test.js    ← optional unit tests (picked up automatically)
```

### 3. JS Runtime (goja)
Tasks run as JS scripts with injected helpers: `http`, `kv`, `log`, `params`, `env`, `notify`, `dicode`. No filesystem or shell access. Each run is isolated (fresh runtime instance).

Full globals: `http`, `kv`, `log`, `params`, `env`, `input` (chain), `notify`, `output`, `dicode`

**MVP globals**: `log`, `env`, `params`, `http`, `kv`, `output`, `fs`
**Post-MVP**: `notify`, `input`, `dicode` (full API)
**North star**: `server` (daemon tasks only)

### 3e. Notifications

**Three delivery targets:**

**1. Mobile push** (remote, via ntfy/gotify/etc.)
```yaml
notifications:
  on_failure: true
  provider:
    type: ntfy             # ntfy (Apache 2.0) | gotify (MIT) | pushover | telegram
    url: https://ntfy.sh
    topic: my-dicode-alerts
    token_env: NTFY_TOKEN
```

**2. OS desktop notifications** (local, via libnotify/macOS NSUserNotification)
Fires automatically alongside mobile push when dicode is running locally. No config needed — uses OS native notification system (`pkg/notify/desktop.go`).

**3. System tray icon** (`pkg/tray/`)
A tray icon (using `systray` library — MIT licensed) that:
- Shows overall status: green (all ok) / yellow (running) / red (last run failed)
- Left-click → open WebUI in browser
- Right-click menu:
  - Open Web UI
  - Run task > [task list submenu]
  - Last run status
  - Pause/Resume reconciler
  - Quit

Tray icon is optional — enabled via `--tray` flag or `server.tray: true` in config. Runs as a goroutine alongside the HTTP server. Only compiled on platforms where systray is supported (Linux/macOS/Windows).

**Task-level** (`notify` global in JS):
```javascript
await notify.send("Digest sent", { priority: "low" })
await notify.send("API DOWN", { priority: "urgent", tags: ["warning"] })
```

**Approval gates** (north star — suspends run, sends actionable notification):
```javascript
const ok = await notify.ask("Deploy to production?", {
  timeout: "30m", options: ["approve", "reject"]
})
// run pauses in sqlite as "suspended", resumes when user responds via app/URL
```

**Packages**: `pkg/notify/` — `Notifier` interface + ntfy/gotify/desktop implementations. `pkg/tray/` — systray integration.

### 3f. Task → Orchestrator API (`dicode` global)

Two-way communication channel between a running task script and the orchestrator. Injected as the `dicode` global.

```javascript
// Intermediate progress (streamed live to WebUI run log)
dicode.progress("processed 42 of 200", { done: 42, total: 200 })

// Send notification through configured provider
await dicode.notify("Threshold exceeded", { priority: "high" })

// Imperative task dispatch (fire-and-forget, distinct from declarative chain)
await dicode.trigger("send-alert", { reason: "spike", value: 99 })

// Query orchestrator state
const running = await dicode.isRunning("backup-task")

// Human approval gate — suspends run until response (north star)
const ok = await dicode.ask("Send to 500 users?", { timeout: "1h", options: ["yes","no"] })
```

`dicode.trigger()` vs chain: chain is **declarative** (TaskB declares it follows TaskA, TaskA is unaware). `dicode.trigger()` is **imperative** — the running task explicitly fires another task with a payload. Both patterns coexist.

**Package**: `pkg/runtime/js/dicode.go`
**North star**: daemon task type (`trigger: daemon: true`) + agent-orchestration tasks that use `dicode.ask()` + `dicode.trigger()` to build human-in-the-loop AI workflows.

**`dicode` read query methods** (for daemon tasks / WebUI task):
```javascript
await dicode.listTasks()          // all task specs
await dicode.getTask(id)          // single task
await dicode.listRuns(id, n)      // run history
await dicode.getRun(runId)        // run detail
await dicode.getRunLogs(runId)    // log entries
await dicode.listSecrets()        // names only
```

### 3a. Secrets
Tasks declare required secrets in `task.yaml` under `env:`. The runtime resolves them via a **provider chain** — tried in order until found.

**MVP providers** (implemented now):
- `local` — encrypted SQLite store. Key = auto-generated `~/.dicode/master.key` (chmod 600) or `DICODE_MASTER_KEY` env var. Encryption: ChaCha20-Poly1305, key derivation: Argon2id.
- `env` — host environment variables (fallback)

**North star providers** (future, same interface):
- `vault` — HashiCorp Vault
- `aws-secrets-manager`
- `gcp-secret-manager`
- `doppler`, `1password`, `infisical`

**Go interface** (`pkg/secrets/provider.go`):
```go
type Provider interface {
    Name() string
    Get(ctx context.Context, key string) (string, error)
}
```
Chain = `[]Provider`, iterated in order. First hit wins.

**Config** (`dicode.yaml`):
```yaml
secrets:
  providers:
    - type: local     # encrypted SQLite, checked first
    - type: env       # host env vars, fallback
    # future:
    # - type: vault
    #   address: https://vault.example.com
    #   token_env: VAULT_TOKEN
```

**task.yaml** — tasks never change regardless of which provider is used:
```yaml
env:
  - SLACK_TOKEN    # dicode resolves from provider chain
  - GMAIL_TOKEN
```

**CLI**:
```bash
dicode secrets set SLACK_TOKEN xoxb-...
dicode secrets get SLACK_TOKEN
dicode secrets list
dicode secrets delete SLACK_TOKEN
```

### 3b. Testing & Validation

Four layers, each catching different classes of problems:

```
Layer 1: Static validation     — schema + syntax, zero execution, instant
Layer 2: Unit tests            — mocked globals, full task run in goja, local
Layer 3: Dry run               — real secrets, intercepted HTTP, no side effects
Layer 4: CI guardrails         — runs layers 1+2 on every push, offline-safe
```

**Layer 1 — Static validation** (`dicode task validate [--all] [id]`)
- `task.yaml` schema check (required fields, valid cron, valid `chain.on`)
- JS syntax via goja compile-without-execute
- Warn if declared `env:` vars have no secret registered
- Chain cycle detection (DFS across all tasks)
- CI-friendly exit code 1 on any failure

**Layer 2 — Unit tests** (`dicode task test [--all] [id]`)

`task.test.js` ships alongside `task.js`. Test runtime injects mock globals:

```javascript
// task.test.js
test("sends digest to slack on new emails", async () => {
  http.mock("GET", "https://gmail.googleapis.com/*", {
    status: 200, body: { messages: [{ snippet: "Hello" }] }
  })
  http.mock("POST", "https://slack.com/api/chat.postMessage", { ok: true })
  env.set("SLACK_TOKEN", "xoxb-test")
  params.set("slack_channel", "#test")

  const result = await runTask()   // evals task.js with mocked globals

  assert.equal(result.count, 1)
  assert.httpCalled("POST", "https://slack.com/api/chat.postMessage")
})
```

Implementation: fresh goja runtime per `test()` call, mock `http`/`kv`/`log`/`env`/`params` injected, `runTask()` evals `task.js` in same runtime. Collects pass/fail per case.

Package: `pkg/testing/harness.go`

**Layer 3 — Dry run** (`dicode task run <id> --dry-run`)
Real secrets, real execution path, but all outbound `http` calls intercepted and logged. Validates secret resolution and endpoint targeting without side effects.

**Layer 4 — CI integration** (`dicode ci init --github|--gitlab`)
Generates CI config. Runs validate + test entirely offline (no secrets, no DB needed):
```yaml
# .github/workflows/dicode.yml (generated)
on: [push, pull_request]
jobs:
  validate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: dicode/setup-action@v1
      - run: dicode task validate --all
      - run: dicode task test --all
```

**AI generation includes tests**: when Claude generates `task.js`, it also generates `task.test.js`. Both shown in the diff. AI retry loop fixes test files too if they fail syntax check.

**Full CLI reference**:
```bash
dicode task validate <id|--all>          # schema + syntax + cycle check
dicode task test <id|--all> [--watch]    # run task.test.js
dicode task run <id> [--dry-run] [--verbose]
dicode task commit <id> --to <source>    # promote local task to git
dicode task diff <id>                    # changes vs committed version
dicode secrets set/get/list/delete
dicode ci init --github|--gitlab
dicode agent skill show                  # print agent skill to stdout
dicode agent skill install [--claude-code]
```

**New packages**: `pkg/testing/harness.go`

### 3c. MCP Server

Exposes dicode capabilities as MCP tools so any MCP-capable agent (Claude Code, Cursor, custom agents) can develop tasks autonomously using the full validate→test→dry-run→commit workflow.

Runs in the same process as the WebUI. Enabled via `server.mcp: true` (default). Mounted at `/mcp`.

**Tools**:
| Tool | Description |
|---|---|
| `list_tasks` | All registered tasks with id, status, trigger, last run |
| `get_task(id)` | task.yaml + task.js + task.test.js content |
| `get_js_api` | Full JS globals reference |
| `get_example_tasks` | 2-3 curated examples for few-shot context |
| `list_secrets` | Registered secret names (never values) |
| `write_task_file(path, content)` | Write file into local dev source |
| `validate_task(id_or_path)` | Static validation, structured errors |
| `test_task(id_or_path)` | Run task.test.js, pass/fail per case |
| `dry_run_task(id)` | Intercepted HTTP run, returns log + return value |
| `run_task(id)` | Live manual trigger, returns run ID |
| `get_run_log(run_id)` | Execution log for a run |
| `commit_task(id, source_id)` | Promote local task to git, returns commit SHA |

**Package**: `pkg/mcp/server.go` — wraps existing registry, runner, secrets chain. No duplicate logic.

### 3d. Agent Skill

A markdown skill file giving any AI agent full context to develop dicode tasks correctly. Distributed with the binary.

```bash
dicode agent skill show                     # print to stdout
dicode agent skill install                  # write to ~/.dicode/skill.md
dicode agent skill install --claude-code    # write to ~/.claude/skills/dicode-task-developer.md
```

**Skill covers**:
- Mandatory workflow: `list_tasks` → `list_secrets` → `get_js_api` → write → `validate_task` → `test_task` → `dry_run_task` → `commit_task`
- Hard rules: never commit if validate or test fail; always write tests; return JSON-serializable value; never hardcode secrets
- task.yaml required fields and common mistakes
- JS globals with signatures
- Chain trigger constraints (output < 1MB, JSON-serializable)

**File**: `pkg/agent/skill.go` — embeds `skill.md` via `//go:embed`; `dicode agent skill show` prints it.

### 3f-2. Filesystem Access (`fs` global)

Zero filesystem access by default. Tasks opt in by declaring paths + permissions in `task.yaml`:

```yaml
fs:
  - path: ~/data
    permission: r       # read-only
  - path: ~/reports
    permission: rw      # read + write + delete
  - path: /tmp/dicode
    permission: rw
```

**Permissions**: `r` (read only), `w` (write/delete/mkdir), `rw` (both).

**`fs` global** (`pkg/runtime/js/globals/fs.go`):
```javascript
const text     = await fs.read("~/data/users.csv")
const obj      = await fs.readJSON("~/data/config.json")
await fs.write("~/reports/weekly.html", content)
await fs.writeJSON("~/reports/summary.json", data)
await fs.append("~/log.txt", line)
const entries  = await fs.list("~/data")       // [{ name, path, isDir, size, modified }]
const paths    = await fs.glob("~/data/**/*.csv")
const info     = await fs.stat("~/reports/weekly.html")
const exists   = await fs.exists("~/reports/weekly.html")
await fs.mkdir("~/reports/2026/march")
await fs.copy(src, dst)
await fs.move(src, dst)
await fs.delete("~/reports/old.txt")
```

**Security model** — enforced in Go before every call:
1. `filepath.Abs` → absolute path
2. `filepath.EvalSymlinks` → resolve symlinks (blocks symlink escapes)
3. Check resolved path is prefixed by a declared `fs:` entry
4. Check operation matches declared permission
5. Throw `PermissionError` on any violation — no silent fallback

**`fs` global is not injected at all** when `task.yaml` has no `fs:` section — no possibility of accidental access.

**Use cases**: agent tasks (read codebase, write reports), cleanup tasks (archive old files), report generators (read data CSVs, write HTML output), any task that needs to read config files or write artifacts.

### 3g. Rich Task Output (`output` global)

Tasks return typed content that renders in the WebUI run detail view:

```javascript
return output.html(`<h1>Daily Report</h1><table>...</table>`)
return output.text("Done: 42 items\n3 errors")
return output.image("image/png", base64Data)
return output.file("report.csv", csvContent, "text/csv")
// HTML for humans + structured data for chain triggers:
return output.html(htmlContent, { data: { count, errors } })
```

**WebUI rendering**: `text/html` → sandboxed iframe, `text/plain` → `<pre>`, `image/*` → `<img>`, file types → download button, plain return → JSON tree.

**Chain compatibility**: `output.html(html, { data })` — chained tasks receive `data` as `input`, not the HTML. Visual output and machine-readable output are independent.

**Go implementation**: `globals/output.go` returns a struct `{ ContentType, Content, Data }` stored in sqlite alongside the run.

### 3h. Daemon Tasks + `server` Global (north star)

**Daemon task type** — long-running tasks that start with dicode and run forever:
```yaml
trigger:
  daemon: true
  restart: always   # always | never | on-failure
```

**`server` global** — HTTP serving from inside a task:
```javascript
// Mount on dicode's main server (ideal for WebUI task)
const app = server.mount("/")
app.get("/api/tasks", async (req, res) => res.json(await dicode.listTasks()))
app.static("/", "./dist")
await app.start()

// Or standalone port
server.get("/health", (req, res) => res.json({ ok: true }))
await server.listen(9090)
```

**WebUI-as-task**: The embedded Go WebUI is the MVP. The north star is the WebUI running as a daemon task served from a TypeScript/React app in a separate repo (`github.com/dicode/webui`). The dicode binary becomes a pure orchestrator — no UI opinion baked in. Community can build alternative UIs.

**Bootstrap**: when no daemon task mounts `/`, the binary serves a minimal page: "Install WebUI: `dicode task install github.com/dicode/webui`". The REST API at `/api/` is always served by the binary.

**`pkg/runtime/js/globals/server.go`** — `server.mount(path)` (wires into chi router) + `server.listen(port)` (standalone net/http server in goroutine).

### 4. Source Abstraction + Reconciler

Tasks can come from multiple sources simultaneously. All sources emit the same `SourceEvent` (Added/Removed/Updated); the reconciler is source-agnostic.

**`pkg/source/source.go`** — `Source` interface: `ID()`, `Start(ctx) (<-chan Event, error)`, `Sync(ctx)`

**`pkg/source/git/`** — GitSource: `go-git` poll/push-webhook, pause-safe (won't pull while local dev is active on same dir)

**`pkg/source/local/`** — LocalSource: `fsnotify` filesystem watcher, instant reload on file save (~100ms)

Config supports a `sources:` array (replaces single `repo:`):
```yaml
sources:
  - type: git
    url: https://github.com/team/shared-tasks
    branch: main
    poll_interval: 60s
    auth: { type: token, token_env: GITHUB_TOKEN }
  - type: local
    path: ~/tasks-dev
    watch: true   # fsnotify instant reload
```

Task IDs must be unique across all sources. Conflict = error logged, second task skipped.

### 5. Trigger System
- `cron` — `robfig/cron` scheduler
- `webhook` — HTTP POST to `/hooks/<path>`
- `manual` — WebUI button or API call
- `chain` — fired when another task completes (see Task Chaining)

### 6. Task Registry
In-memory map of loaded tasks + sqlite-backed run history (status, logs, timestamps).

### 7. Task Store (install/share)
```
dicode task install github.com/someone/tasks/morning-email-check --param slack_channel=#devops
```
Copies task into local tasks repo, commits, pushes. Reconciler picks it up. Future: searchable community registry index.

### 8. AI Generator
User types a prompt in WebUI → AI (Claude) generates `task.yaml` + `task.js` → syntax validated → shown as diff → user confirms → committed to tasks repo → reconciler picks it up.

### 9. Web UI & API
- REST API for tasks/runs/triggers/AI generation
- UI: task list, run history, log viewer, AI prompt box
- Minimal frontend: HTMX (no build step, embedded in binary)

---

## Task Chaining

**MVP (Option 2): Declared chain trigger**

TaskA returns a JS value. TaskB declares it listens for TaskA's completion. Dicode wires them — TaskA is completely unaware of TaskB.

```yaml
# task-b/task.yaml
trigger:
  chain:
    from: fetch-emails     # task ID to listen for
    on: success            # success | failure | always
```

```javascript
// task-a/task.js
return { emails, count }       // captured by dicode, injected into task-b as `input`

// task-b/task.js
log.info(input.count)          // `input` global contains task-a's return value
```

**Implementation**: when a run completes, the runner checks the registry for tasks with `trigger.chain.from == completedTaskID`, fires them with the output injected as the `input` global. Chained runs are stored as child runs of the parent (parentRunID foreign key in sqlite).

**Constraints for MVP**:
- Linear chains only (A→B→C)
- Output must be JSON-serializable
- Output size capped (e.g. 1MB) — tasks are not a data pipeline
- Cycle detection at register time (DFS on chain graph)

**North Star (Option 4): Pipeline definition file**

A separate `pipeline.yaml` defines a full DAG with fan-out, fan-in, and conditionals. Tasks remain single-purpose; the pipeline orchestrates them. This is the Airflow/Temporal territory — implement only when linear chains prove insufficient.

```yaml
# pipelines/morning-routine/pipeline.yaml
name: morning-routine
trigger:
  cron: "0 9 * * *"
steps:
  - id: fetch
    task: fetch-emails
  - id: digest
    task: send-slack-digest
    input: $steps.fetch.output
  - id: archive
    task: archive-emails
    input: $steps.fetch.output   # fan-out
```

---

## Dev Workflow

When writing or generating a task locally, there's no need for a git round-trip. The local source provides instant feedback:

1. Configure a `local` source pointing at a working directory
2. Write or generate a task — dicode picks it up via fsnotify within ~100ms
3. Test it (manual trigger from UI or API)
4. When ready: `dicode task commit <task-id> --to <git-source-id>` — moves the task into the git repo, commits, pushes
5. The git source sees the push (webhook) or next poll and takes over ownership

**Separating dev from production**: local and git sources coexist. Use a local source for active development, git sources for stable/shared tasks. No overlap, no conflict, no "dev mode" flag needed.

**North star — task selectors**: filter which tasks a source loads based on tags declared in `task.yaml`. Lets one dicode instance serve multiple environments (prod vs staging vs dev) from the same repos.

---

## Key Tech Choices
| Concern | Choice | Why |
|---|---|---|
| Task scripting | goja (JS) | AI generates great JS; pure Go; hot-loadable |
| Git client | go-git | Pure Go, no system git binary needed |
| Filesystem watch | fsnotify | Cross-platform, pure Go |
| DB | sqlite (modernc) | Pure Go, no CGo, simple |
| HTTP router | chi | Lightweight, idiomatic |
| AI | Claude API | Best code generation quality |
| Frontend | HTMX | No npm, single binary, enough for MVP |
| Mobile push | ntfy | Apache 2.0, self-hostable, action buttons, simple HTTP API |
| System tray | systray | MIT, cross-platform (Linux/macOS/Windows) |
| Secret encryption | ChaCha20-Poly1305 + Argon2id | Fast, authenticated, no padding oracle |

---

## Local-Only Mode

Git is **fully optional**. A user with no GitHub account can run dicode entirely on their machine using only a `local` source.

**Minimal config** (auto-generated on first run if user chooses local-only):
```yaml
sources:
  - type: local
    path: ~/dicode-tasks
    watch: true
database:
  type: sqlite
```

Everything works: task execution, JS runtime, secrets, MCP, testing, AI generation (writes to local dir), tray icon, WebUI. No relay (disabled when no account token). No version history (no git).

**Migration path**: user can add a git source later and `dicode task commit` their local tasks into it — no rework required.

**First-run onboarding** (`pkg/onboarding/`): when no `dicode.yaml` exists, dicode launches an onboarding wizard (WebUI page or tray-triggered browser window):

```
Welcome to dicode

How do you want to store your tasks?

  ○ Local only   — no accounts needed, tasks stay on this machine
                   great for personal automation, try it out
  ○ Git repo     — tasks versioned in GitHub/GitLab
                   shareable, auditable, works with CI

[Get started]
```

Choosing "local only":
1. Creates `~/dicode-tasks/` directory
2. Writes minimal `dicode.yaml` with single local source
3. Opens WebUI dashboard — ready to create first task

Choosing "git repo":
1. Prompts for repo URL + auth token
2. Clones repo, writes full `dicode.yaml`
3. Opens WebUI dashboard

Both paths land on the same experience. The onboarding is a one-time flow — subsequent runs skip it if `dicode.yaml` exists.

**`pkg/onboarding/`** — first-run detection (`dicode.yaml` absent) + wizard handler (served by WebUI on first request).

## Business Model

See `BUSINESSPLAN.md` for full detail. Summary of locked decisions:

**Free vs Paid gate**: public git repos + SQLite = free forever. Private git + bring-your-own DB (Postgres/MySQL) = paid. Core engine never gated.

**Deployment profiles**:
- **Desktop** (tray + OS notifications + relay) — default when `$DISPLAY` exists
- **Headless** (`--headless` or auto when no desktop) — for servers/Docker/systemd
- **Docker** — official image, env-var config, health endpoint, `DICODE_HEADLESS=true`

**Run on startup**:
- Desktop: `dicode service install` writes LaunchAgent (macOS) / XDG autostart (Linux) / Registry (Windows)
- Server: `dicode service install` writes systemd unit or Windows Service

**Webhook relay** (`pkg/relay/`): local dicode connects to `dicode.app` relay via persistent WebSocket on startup. Stable public URLs: `dicode.app/u/{uid}/hooks/{path}` → forwarded to local instance. Free (500/mo) → Pro (unlimited + custom subdomain + replay). Self-hosted server users expose their port directly (no relay needed).

**Database abstraction** (`pkg/db/`):
```yaml
database:
  type: sqlite    # free, default
  # type: postgres / mysql — paid, for HA / multi-instance
  # url_env: DATABASE_URL
```

**Tiers**: Free → Pro ($12/mo) → Team ($20/seat/mo) → Enterprise (custom).
**CLI additions**: `dicode service install/start/stop/status/logs/uninstall`, `dicode relay status`

## Data Flow

**Production (git source):**
```
User prompt
  → AI generates task.yaml + task.js
  → shown as diff in WebUI → user confirms
  → commit + push to tasks repo
  → git source: poll/push-webhook → pull
  → reconciler: Added/Updated event
  → task loaded into registry
  → trigger fires (cron/webhook/manual/chain)
  → goja executes task.js
  → run logged to sqlite → visible in WebUI
```

**Local development (local source):**
```
User writes/edits task file
  → fsnotify detects change (~100ms)
  → reconciler: Added/Updated event
  → task reloaded in registry immediately
  → test via manual trigger in WebUI
  → when ready: dicode task commit <id> --to <git-source>
  → git source takes over ownership
```
