---
name: dicode-task-dev
description: Mandatory workflow, rules, and conventions for developing dicode tasks — trigger/permissions schema, test format, and common mistakes.
---

# Dicode Task Developer

You are an AI agent developing automation tasks for a dicode instance.

## The tools you actually have

The dicode MCP server exposes exactly six tools. Each one acts — none of them
hand back an instruction to make the call yourself.

| Tool | What it does |
|---|---|
| `list_tasks` | Lists the registered tasks that opted in with `mcp_exposed: true`, with IDs, names, descriptions, and declared params. |
| `get_task(id)` | Returns one task's spec (id, name, description, params) by namespaced ID. |
| `run_task(id, params?)` | Triggers a task, blocks until it finishes, returns its run result. `params` is a string-valued object. |
| `list_sources` | Lists the configured sources: name, type, git URL, branch, dev-mode state. No host paths. |
| `switch_dev_mode(source, enabled, branch?, base?, run_id?)` | Enters or leaves dev mode. Entering with a branch clones the source and returns `clone_path` — edit there, not in the live source. `run_id` names the clone; omit it, the daemon binds it to your run. |
| `test_task(id)` | Runs a task's sibling test file and returns the results. Refused while the approval gate holds the task pending. |

A call you are not entitled to make comes back as a JSON-RPC error naming the
capability, not as a hint. That is a real answer: the token you hold was minted
from a task's declared `permissions.dicode`, and no phrasing of the call will
widen it.

There is no MCP tool that lists secrets, dumps the JS API, fetches example
tasks, writes files, validates, dry-runs, or commits. Discover credentials with
the `dicode secrets list` CLI, read this skill and `tasks/examples/*` for the
SDK surface and patterns, and write files through the editor / dev-mode clone —
not through an MCP call.

## Mandatory workflow

Follow this order every time — no exceptions:

1. `list_tasks` — check if a similar task already exists (avoid duplicates);
   `get_task("<id>")` to read a close analog's spec and copy its patterns.
2. Know what credentials exist — `dicode secrets list` on the CLI. Never invent
   secret names; declare only what exists under `permissions.env`.
3. Enter dev mode on the target source — `list_sources` to find its name, then
   `switch_dev_mode("<source>", true, { branch: "<branch>" })`. Edit inside the
   `clone_path` it returns, never in the live source.
4. Write the three files (via the editor / dev-mode clone):
   - `<task-id>/task.yaml` — trigger, params, env declarations
   - `<task-id>/task.ts`   — task logic (TypeScript for `runtime: deno`; use `task.py` for `runtime: python`)
   - `<task-id>/task.test.ts` (or `task.test.py` for `runtime: python`) — unit tests (required, no exceptions)
5. Test — ALL tests must pass before proceeding. `test_task("<task-id>")` runs
   them. A task the approval gate still holds pending is refused: approve it
   first (`dicode task approve <task-id>`). On the CLI: `dicode task test
   <task-id>` (or `make test-tasks` for the full sweep).
   Deno and Python are supported; Docker/Podman parity tracked in
   [#159](https://github.com/dicode-ayo/dicode-core/issues/159) Phase 3.
6. Exercise it — `run_task("<task-id>", { key: "value" })` triggers a real run
   and returns the result; verify HTTP calls and secret resolution from it.

## Hard rules

- **Never ship** a task whose `task.yaml` fails to parse or whose tests fail
- **Always write a test file** (`task.test.ts` for `runtime: deno`, `task.test.py` for `runtime: python`) — a task without tests should not ship
- `task.ts` **must return a JSON-serializable value** — required for chain triggers
- **Never hardcode secrets** — use `env.get("VAR")` in the script; declare the var in `permissions.env`
- **Never declare `DICODE_SOCKET` or `DICODE_TOKEN` in `permissions.env`** — these are internal IPC variables injected automatically; declaring them leaks the token to user code
- **One task, one responsibility** — keep tasks focused and composable
- **Output under 1MB** — tasks are not a data pipeline; keep return values small

## task.yaml required fields

```yaml
name: <unique-kebab-case-id>       # must match the directory name
description: <what this task does>
runtime: deno                      # deno (default), python, docker
trigger:                           # exactly ONE of:
  cron: "0 9 * * *"               #   standard 5-field cron
  webhook: /hooks/<path>           #   HTTP POST trigger (open — no auth)
  auth: true                       #   optional: require dicode session for webhook GET/POST
  manual: true                     #   UI/API only
  chain:                           #   fires when another task completes
    from: <task-id>
    on: success                    #   success | failure | always
  daemon: true                     #   starts on app start, restarts on exit
permissions:                       # explicit access grants — nothing is implicit
  env:                             # env vars the script may read (four forms):
    - SLACK_TOKEN                  #   bare: allowlist $SLACK_TOKEN from host env, same name
    - name: API_KEY                #   from: read $GH_TOKEN from host OS env, inject as API_KEY
      from: GH_TOKEN
    - name: DB_PASS                #   secret: resolve "db_password" from secrets store only
      secret: db_password
    - name: STATUS_PW              #   default: literal fallback when the secret is unset
      secret: relay_status_password
      default: "dev-fallback"
    - name: LOG_LEVEL              #   value: literal injection (taskset overrides)
      value: "info"
  net:                             # outbound network (Deno only)
    - "api.github.com"             #   list specific hosts; omit for unrestricted; [] to deny all
  fs:                              # filesystem paths (Deno only)
    - path: ~/data
      permission: r                #   r | w | rw
  run:                             # executables the script may spawn (Deno only)
    - curl                         #   list binaries, or use ["*"] to allow all
  sys:                             # Deno system-info APIs (Deno only; omit = deny all)
    - hostname                     #   or use ["*"] for all
params:                            # optional user-configurable inputs
  slack_channel:                   #   key = param name
    type: string
    default: "#general"
timeout: 60s                       # default: 60s

# Agent / orchestration fields (optional):
on_failure_chain: <task-id>        # task to invoke when this task fails; "" disables global default
mcp_port: 3000                     # declare MCP server port (daemon tasks only)
permissions:
  dicode:                          # dicode runtime API access — all denied by default
    tasks:                         # task IDs this script may call via dicode.run_task()
      - "*"                        # use "*" to allow all, or list specific IDs
    mcp:                           # MCP daemon task IDs accessible via mcp.call()
      - "github-mcp"
    list_tasks: true               # allow dicode.list_tasks()
    get_runs: true                 # allow dicode.get_runs()
    secrets_write: true            # allow dicode.secrets_set() and dicode.secrets_delete() — write-only
```

### Protected webhook trigger (HMAC authentication)

When a webhook must only accept requests from a trusted sender (GitHub, Stripe, etc.), add `webhook_secret`:

```yaml
trigger:
  webhook: /hooks/<path>
  webhook_secret: "${WEBHOOK_SECRET}"   # ALWAYS reference a secret, never hardcode
permissions:
  env:
    - WEBHOOK_SECRET
```

- dicode verifies the `X-Hub-Signature-256` header automatically before the task script runs
- A request with a missing or wrong signature is rejected with HTTP 403 — the script never executes
- The format is identical to GitHub's webhook signature — point any GitHub webhook at the endpoint with the same secret and it works with no changes
- Replay protection: if the sender includes `X-Dicode-Timestamp`, requests older than 5 minutes are rejected

**Always use `"${ENV_VAR}"` syntax** — never write the raw secret value in `task.yaml`. Store it as a dicode secret first, then reference it via env.

### Session-authenticated webhook (internal tools)

When the webhook UI should only be accessible by logged-in dicode users, add `auth: true`:

```yaml
trigger:
  webhook: /hooks/my-internal-tool
  auth: true
```

- `GET /hooks/…` (serving `index.html`) requires a valid session → redirects to `/?auth=required` if missing
- `POST /hooks/…` (running the task) requires a valid session → returns `401` JSON if missing
- `dicode.js` handles 401 automatically: silent refresh via device token, then redirects to login
- Open webhooks (no `auth: true`) remain fully public — no behaviour change

## Available globals (Deno runtime)

### HTTP — use native `fetch` (Deno)
```typescript
const res = await fetch("https://api.example.com/data", {
  method: "POST",
  headers: { "Authorization": `Bearer ${env.MY_TOKEN}`, "Content-Type": "application/json" },
  body: JSON.stringify({ key: "value" }),
})
const data = await res.json()
```

### npm packages — import inline
```typescript
import OpenAI from "npm:openai"
import { z } from "npm:zod"
```

### `kv` — persistent key-value store (survives restarts, scoped to task)
```javascript
kv.set("key", value)   // value must be JSON-serializable
const val = kv.get("key")   // returns null if not found
kv.delete("key")
```

### `log` — structured logging (appears in run log in WebUI)
```javascript
log.info("message", { optional: "context" })
log.warn("message", { optional: "context" })
log.error("message", { optional: "context" })
```

### `params` — values from task.yaml params (user-configurable)
```javascript
const channel = params.slack_channel   // string, uses default if not overridden
```

### `env` — environment variables (ONLY those declared in permissions.env)
```javascript
const token = await env.get("SLACK_TOKEN")  // null if not declared in permissions.env
```

### `input` — incoming data (chain tasks and webhook tasks)
```typescript
// Chain trigger: upstream task's return value
const data = input.emails

// Webhook trigger: parsed POST body (JSON or form fields)
const action = input.action       // e.g. GitHub push event field
const repo   = input.repository   // nested objects fully available
```

For webhook tasks the raw POST body is parsed and available as `input`. Query-string parameters are also available via `params`.

### `dicode` — task orchestration (requires `permissions.dicode`)
```typescript
// Run another task and await its result (requires permissions.dicode.tasks)
const result = await dicode.run_task("send-report", { channel: "#ops" })
// result: { runID, status, returnValue }

// List all registered tasks (requires permissions.dicode.list_tasks: true)
const tasks = await dicode.list_tasks()

// Get recent run history (requires permissions.dicode.get_runs: true)
const runs = await dicode.get_runs("send-report", { limit: 5 })

// Write or replace a secret (requires permissions.dicode.secrets_write: true)
// Tasks can NEVER read secrets back — use permissions.env for secret injection
await dicode.secrets_set("MY_TOKEN", newValue)
await dicode.secrets_delete("OLD_TOKEN")
```

### `mcp` — MCP server tools (requires `permissions.dicode.mcp`)
```typescript
const tools  = await mcp.list_tools("github-mcp")
const result = await mcp.call("github-mcp", "search_repositories", { query: "dicode" })
```

### `suspend` — pause for user input, auto-dispatched
Pause a run to collect input from a human, then continue. `dicode.suspend()`
never returns: the process exits, the run becomes `suspended`, and on resume the
runner **re-runs the file and dispatches the right handler** — you never write an
`if (resume_state)` switch. Each handler reads `ctx.state` (the blob you carried,
`undefined`/`None` on the first run) and `ctx.input` (the validated submission).
No permission declaration needed; Deno/Python only (not docker/podman).

Export **`main`** (first run) + optionally **`resume`** (the continuation), or a
**`steps`** map named by `suspend({ to })` for a multi-step wizard. `schema` is a
JSON Schema (draft 2020-12); the daemon validates the submission against it
before resuming, so `ctx.input` conforms.

```typescript
export default async function main({ dicode }) {
  await dicode.suspend({
    schema: {
      type: "object",
      title: "Deploy to production?",
      properties: {
        approve: { type: "boolean", title: "Approve?" },
      },
      required: ["approve"],
    },
    // deadline: optional Unix-ms; default 24h. On lapse the run is cancelled (resume_timeout).
  })
  // unreachable — suspend() never returns
}

export async function resume({ input }) {
  return { deployed: Boolean(input.approve) }
}
```

Default WebUI renderer maps the common subset: `type:string`→text (`format:"textarea"`→textarea),
`enum`→select, `type:boolean`→checkbox, `type:number|integer`→number; honors
`title`/`description`/`default`/`required`.

Rules: never wrap `suspend()` in a `try/catch` that swallows the control signal
(the run fails loudly if you do); `params` survive a resume but the original
trigger `input` does not — stash anything you need into `state`. Python defines
`main` / `resume` / `steps` and reads the module-global `ctx` (`ctx.state`,
`ctx.input`), or accepts it as a handler argument (`async def resume(ctx):`);
`dicode.suspend(schema=..., to=..., state=...)`.
Full reference: [docs/concepts/suspendable-tasks.md](../../docs/concepts/suspendable-tasks.md).

### `return` — pass data to downstream chain tasks
```typescript
return { count: 3, ids: ["a", "b", "c"] }   // must be JSON-serializable
```

## task.test.ts format

(For `runtime: python`, write `task.test.py` instead — same mocking
philosophy via `tasks/sdk_test.py`, but it's a PEP 723 script with a couple
of non-obvious requirements around how it's invoked. See
[docs/concepts/testing.md § Python](../../docs/concepts/testing.md#python)
and [tasks/examples/hello-python/task.test.py](../examples/hello-python/task.test.py)
before writing one.)

```typescript
// Each test() gets a fresh mock state — mocks don't leak between tests.

test("description of happy path", async () => {
  // 1. arrange mocks
  http.mock("GET", "https://api.example.com/*", { status: 200, body: { items: [1, 2] } })
  http.mock("POST", "https://slack.com/api/chat.postMessage", { ok: true })
  env.set("SLACK_TOKEN", "xoxb-test")
  params.set("slack_channel", "#test")

  // 2. run the task
  const result = await runTask()

  // 3. assert
  assert.equal(result.count, 2)
  assert.httpCalled("POST", "https://slack.com/api/chat.postMessage")
  assert.httpCalledWith("POST", "https://slack.com/api/chat.postMessage", {
    body: { channel: "#test" }
  })
})

test("edge case: empty result", async () => {
  http.mock("GET", "https://api.example.com/*", { status: 200, body: { items: [] } })
  env.set("SLACK_TOKEN", "xoxb-test")

  await runTask()

  assert.httpNotCalled("POST", "https://slack.com/api/chat.postMessage")
})
```

### Test globals
| Global | Signature | Description |
|---|---|---|
| `test(name, fn)` | `fn` can be async | Define a test case |
| `runTask()` | `async () => any` | Evaluate task.js with current mocks |
| `http.mock(method, pattern, response)` | pattern supports `*` | Intercept matching calls |
| `http.mockOnce(method, pattern, response)` | | Match first call only |
| `env.set(key, value)` | | Mock env var |
| `params.set(key, value)` | | Mock param |
| `kv.set(key, value)` | | Pre-populate kv store |
| `assert.equal(a, b)` | | Deep equality |
| `assert.ok(val)` | | Truthy assertion |
| `assert.throws(fn)` | | Expect thrown error |
| `assert.httpCalled(method, pattern)` | | Assert call was made |
| `assert.httpCalledWith(method, url, opts)` | | Assert call with body/headers |
| `assert.httpNotCalled(method, pattern)` | | Assert no matching call |

## taskset.yaml format

When creating or editing a `taskset.yaml` (the root entry point for a TaskSet source), use this format:

```yaml
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: <source-name>
spec:
  defaults:             # optional — applied at precedence level 2
    timeout: 30m
  entries:              # required — map of entry-key → task or nested TaskSet ref
    my-task:
      ref:
        path: ./my-task   # path to task.yaml or a nested taskset.yaml
      overrides:          # optional — precedence level 3 (highest)
        timeout: 5m
    nested-set:
      ref:
        path: ./platform/taskset.yaml   # nested TaskSet — namespace: <source>/nested-set
```

**3-level precedence stack** (lowest → highest):
1. `task.yaml` base values
2. `spec.defaults` (this TaskSet)
3. Per-entry `overrides` (leaf wins)

**Common mistakes:**
| Mistake | Correct |
|---|---|
| `tasks: [{path: ./foo}]` (old flat format) | Use `spec.entries:` map |
| `name: foo` at top level (no metadata block) | Use `metadata: {name: foo}` |
| Expecting `kind:Config spec.defaults` to apply | Deprecated — use `dicode.yaml defaults:` instead |

## Common mistakes to avoid

| Mistake | Correct approach |
| --- | --- |
| `process.env.SLACK_TOKEN` | `await env.get("SLACK_TOKEN")` |
| Accessing env var not in `permissions.env` | Add it to `permissions.env` |
| Using `from:` to read from secrets store | Use `secret:` for secrets store; `from:` is host OS env rename only |
| Using `secret:` for a host env var | Use bare name or `from:` for host env; `secret:` is secrets store only |
| Returning `new Date()` | Return `date.toISOString()` |
| Writing tests that don't call `runTask()` | Always call `runTask()` in each test |
| One trigger type + another trigger type | Exactly one trigger per task.yaml |
| `chain.on: "ok"` | Must be `success`, `failure`, or `always` |
| Large return values (>1MB) | Keep returns small; use external storage for large data |
| `webhook_secret: "abc123"` (hardcoded) | `webhook_secret: "${MY_SECRET}"` + add to `permissions.env` |
| Forgetting `permissions.env` entry for `webhook_secret` | Every `${VAR}` in task.yaml needs a matching entry in `permissions.env` |
| Trying to verify the signature in `task.ts` | dicode verifies it automatically — the script only runs if the signature is valid |
| Using `webhook_secret` on a public form endpoint | Only add `webhook_secret` when the sender can set `X-Hub-Signature-256`; browser forms cannot sign requests |
| Calling `dicode.run_task()` without `permissions.dicode.tasks` | Add `permissions.dicode.tasks` listing callable task IDs; calls are blocked otherwise |
| Calling `mcp.call()` without `permissions.dicode.mcp` | Add `permissions.dicode.mcp` listing the daemon task IDs |
| Calling `dicode.list_tasks()` without `permissions.dicode.list_tasks: true` | Add `permissions.dicode.list_tasks: true`; denied by default |
| Using `security:` top-level field | `security:` is removed — use `permissions.dicode:` instead |
| Calling `dicode.secrets_set()` without `permissions.dicode.secrets_write: true` | Add `permissions.dicode.secrets_write: true`; denied by default |
| Trying to read a secret via `dicode.secrets_get()` | No such method — secrets are injected at startup via `permissions.env`; tasks never read them at runtime |
| Creating task.js instead of task.ts | Use TypeScript (`.ts`) for Deno runtime tasks |

## Protected webhook — worked example

### task.yaml

```yaml
name: github-push-handler
description: Receives GitHub push events and posts a summary to Slack
runtime: deno
trigger:
  webhook: /hooks/github-push
  webhook_secret: "${GITHUB_WEBHOOK_SECRET}"
permissions:
  env:
    - GITHUB_WEBHOOK_SECRET   # dicode uses this for HMAC verification
    - SLACK_TOKEN             # used inside task.ts
params:
  slack_channel:
    type: string
    default: "#deploys"
timeout: 30s
```

### task.ts

```typescript
// input contains the parsed GitHub push payload.
// dicode has already verified the HMAC signature — no need to check it here.

const branch  = input.ref?.replace("refs/heads/", "") ?? "unknown"
const repo    = input.repository?.full_name ?? "unknown"
const commits = input.commits ?? []
const pusher  = input.pusher?.name ?? "someone"

if (commits.length === 0) {
  log.info("push event with no commits — skipping")
  return { skipped: true }
}

const lines = commits.map(c => `• \`${c.id.slice(0,7)}\` ${c.message.split("\n")[0]}`)
const text  = `*${pusher}* pushed ${commits.length} commit(s) to \`${repo}@${branch}\`\n${lines.join("\n")}`

const res = await http.post("https://slack.com/api/chat.postMessage", {
  headers: { Authorization: `Bearer ${env.SLACK_TOKEN}` },
  body: { channel: params.slack_channel, text }
})

if (!res.body.ok) throw new Error(`Slack error: ${res.body.error}`)

return { commits: commits.length, branch, repo }
```

### task.test.ts

```typescript
test("posts commit summary to Slack on valid push", async () => {
  env.set("SLACK_TOKEN", "xoxb-test")
  params.set("slack_channel", "#test-deploys")
  http.mock("POST", "https://slack.com/api/chat.postMessage", { status: 200, body: { ok: true } })

  // Simulate webhook payload via input mock
  input.set({
    ref: "refs/heads/main",
    pusher: { name: "alice" },
    repository: { full_name: "acme/api" },
    commits: [
      { id: "abc1234567890", message: "fix: null pointer in auth" },
      { id: "def0987654321", message: "chore: bump dependencies" }
    ]
  })

  const result = await runTask()

  assert.equal(result.commits, 2)
  assert.equal(result.branch, "main")
  assert.httpCalled("POST", "https://slack.com/api/chat.postMessage")
})

test("skips when push has no commits", async () => {
  env.set("SLACK_TOKEN", "xoxb-test")
  input.set({ ref: "refs/heads/main", repository: { full_name: "acme/api" }, commits: [] })

  const result = await runTask()

  assert.equal(result.skipped, true)
  assert.httpNotCalled("POST", "https://slack.com/api/chat.postMessage")
})
```

### Setting up on the sender side (GitHub example)

After deploying the task, configure the GitHub webhook:

- **Payload URL**: `https://your-dicode-host/hooks/github-push`
- **Content type**: `application/json`
- **Secret**: the value of `GITHUB_WEBHOOK_SECRET` stored in dicode secrets
- **Events**: choose whichever events the task needs (`push`, `pull_request`, etc.)
