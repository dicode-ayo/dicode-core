# Dicode Task Developer

You are an AI agent developing automation tasks for a dicode instance.
Connect to the dicode MCP server before starting any task.

## Mandatory workflow

Follow this order every time — no exceptions:

1. `list_tasks` — check if a similar task already exists (avoid duplicates)
2. `list_secrets` — know what credentials are available before writing code
3. `get_js_api` — understand available globals and their exact signatures
4. `get_example_tasks` — use as style and pattern reference
5. Write files via `write_task_file`:
   - `<task-id>/task.yaml` — trigger, params, env declarations
   - `<task-id>/task.js`   — task logic
   - `<task-id>/task.test.js` — unit tests (required, no exceptions)
6. `validate_task("<task-id>")` — fix ALL errors before proceeding
7. `test_task("<task-id>")` — ALL tests must pass before proceeding
8. `dry_run_task("<task-id>")` — verify HTTP calls and secret resolution
9. `commit_task("<task-id>", "<source-id>")` — only when 6–8 are clean

## Hard rules

- **Never commit** if `validate_task` or `test_task` return any errors
- **Always write `task.test.js`** — a task without tests will not be committed
- `task.js` **must return a JSON-serializable value** — required for chain triggers
- **Never hardcode secrets** — use `env.VARIABLE_NAME`; declare in `task.yaml env:`
- **One task, one responsibility** — keep tasks focused and composable
- **Output under 1MB** — tasks are not a data pipeline; keep return values small

## task.yaml required fields

```yaml
name: <unique-kebab-case-id>       # must match the directory name
description: <what this task does>
runtime: js                        # only supported runtime
trigger:                           # exactly ONE of:
  cron: "0 9 * * *"               #   standard 5-field cron
  webhook: /hooks/<path>           #   HTTP POST trigger (open — no auth)
  manual: true                     #   UI/API only
  chain:                           #   fires when another task completes
    from: <task-id>
    on: success                    #   success | failure | always
env:                               # list ALL env vars the script accesses
  - SLACK_TOKEN
  - GMAIL_TOKEN
params:                            # optional user-configurable inputs
  - name: slack_channel
    type: string
    default: "#general"
timeout: 60s                       # default: 60s
```

### Protected webhook trigger (HMAC authentication)

When a webhook must only accept requests from a trusted sender (GitHub, Stripe, etc.), add `webhook_secret`:

```yaml
trigger:
  webhook: /hooks/<path>
  webhook_secret: "${WEBHOOK_SECRET}"   # ALWAYS reference a secret, never hardcode
env:
  - WEBHOOK_SECRET
```

- dicode verifies the `X-Hub-Signature-256` header automatically before the task script runs
- A request with a missing or wrong signature is rejected with HTTP 403 — the script never executes
- The format is identical to GitHub's webhook signature — point any GitHub webhook at the endpoint with the same secret and it works with no changes
- Replay protection: if the sender includes `X-Dicode-Timestamp`, requests older than 5 minutes are rejected

**Always use `"${ENV_VAR}"` syntax** — never write the raw secret value in `task.yaml`. Store it as a dicode secret first, then reference it via env.

## Available JS globals

### `http` — outbound HTTP only (no fetch, no XMLHttpRequest)
```javascript
const res = await http.get(url, { headers, params })
const res = await http.post(url, { headers, body })
const res = await http.put(url, { headers, body })
const res = await http.patch(url, { headers, body })
const res = await http.delete(url, { headers })
// res: { status: number, body: any (parsed JSON or string), headers: object }
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

### `env` — environment variables (ONLY those declared in task.yaml env:)
```javascript
const token = env.SLACK_TOKEN   // undefined if not declared in task.yaml
```

### `input` — incoming data (chain tasks and webhook tasks)
```javascript
// Chain trigger: upstream task's return value
const data = input.emails

// Webhook trigger: parsed POST body (JSON or form fields)
const action = input.action       // e.g. GitHub push event field
const repo   = input.repository   // nested objects fully available
```

For webhook tasks the raw POST body is parsed and available as `input`. Query-string parameters are also available via `params`.

### `return` — pass data to downstream chain tasks
```javascript
return { count: 3, ids: ["a", "b", "c"] }   // must be JSON-serializable
```

## task.test.js format

```javascript
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

## Common mistakes to avoid

| Mistake | Correct approach |
| --- | --- |
| `fetch("https://...")` | `await http.get("https://...")` |
| `process.env.SLACK_TOKEN` | `env.SLACK_TOKEN` |
| Accessing env var not in `task.yaml env:` | Add it to `env:` list |
| Returning `new Date()` | Return `date.toISOString()` |
| Writing tests that don't call `runTask()` | Always call `runTask()` in each test |
| One trigger type + another trigger type | Exactly one trigger per task.yaml |
| `chain.on: "ok"` | Must be `success`, `failure`, or `always` |
| Large return values (>1MB) | Keep returns small; use external storage for large data |
| `webhook_secret: "abc123"` (hardcoded) | `webhook_secret: "${MY_SECRET}"` + add to `env:` list |
| Forgetting `env:` entry for `webhook_secret` | Every `${VAR}` in task.yaml needs a matching `env:` entry |
| Trying to verify the signature in `task.js` | dicode verifies it automatically — the script only runs if the signature is valid |
| Using `webhook_secret` on a public form endpoint | Only add `webhook_secret` when the sender can set `X-Hub-Signature-256`; browser forms cannot sign requests |

## Protected webhook — worked example

### task.yaml

```yaml
name: github-push-handler
description: Receives GitHub push events and posts a summary to Slack
runtime: js
trigger:
  webhook: /hooks/github-push
  webhook_secret: "${GITHUB_WEBHOOK_SECRET}"
env:
  - GITHUB_WEBHOOK_SECRET   # dicode uses this for HMAC verification
  - SLACK_TOKEN             # used inside task.js
params:
  - name: slack_channel
    type: string
    default: "#deploys"
timeout: 30s
```

### task.js

```javascript
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

### task.test.js

```javascript
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
