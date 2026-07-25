# Testing & Validation

> **Status**: This document describes the testing and validation system. Layer 2
> (`dicode task test`) is implemented for the Deno runtime: `pkg/tasktest`
> drives `deno test` and parses its summary, and `tasks/sdk-test.ts` provides
> a real (opt-in) mock harness — `params`/`env`/`kv`/`http.mock`/`assert.*`/
> `runTask()` — used by this repo's built-in and example tasks. Layer 1
> (`dicode task validate`), Layer 3 (`dicode run --dry-run`), and
> `dicode ci init` remain planned.

Dicode is designed with four validation layers, each catching different classes of problems.

```text
Layer 1: Static validation     — schema + syntax, zero execution, instant        [planned]
Layer 2: Unit tests            — mocked globals, full task run, local             [implemented: Deno]
Layer 3: Dry run               — real secrets, intercepted HTTP, no side effects  [planned]
Layer 4: CI guardrails         — layers 1+2 on every push, offline-safe           [partial: see CI job below]
```

---

## Layer 1 — Static validation (planned design — not implemented)

```bash
dicode task validate <id>
dicode task validate --all
```

Checks performed without executing any code:
- `task.yaml` schema validation (required fields, valid cron expression, valid `chain.on` value)
- `task.js`/`task.ts` syntax check
- Warning if any declared `env:` secrets have no registered value in any provider
- Chain cycle detection (DFS across all task chain declarations)

**Exit codes:** 0 = all valid, 1 = any error. Suitable for CI.

**Output:**
```
✅ task.yaml valid
✅ task.js syntax ok
⚠️  SLACK_TOKEN not found in any provider (registered: GMAIL_TOKEN)
```

---

## Layer 2 — Unit tests

```bash
dicode task test <id>
dicode task test <id> --format=junit        # JUnit XML to stdout; human output to stderr
dicode task test <id> --format=gh-summary   # GitHub Markdown to stdout + $GITHUB_STEP_SUMMARY
dicode task test --all                      # planned — <id> is currently required
dicode task test <id> --watch               # planned — re-run on file save
```

Runs `task.test.ts` (Deno runtime) through the Deno test runner. The current
implementation captures aggregate passed/failed/skipped counts from Deno's
summary line.

### Output formats

| Flag | stdout | $GITHUB_STEP_SUMMARY |
|---|---|---|
| `--format=text` (default) | Human-readable | not written |
| `--format=junit` | JUnit XML | written if env var is set |
| `--format=gh-summary` | GitHub Markdown | written if env var is set |

When `--format=junit` is used, human-readable output goes to stderr so CI logs
remain readable alongside the machine-readable XML.

### CI job: `test-tasks`

The `.github/workflows/ci.yml` job `test-tasks` runs `deno test` directly
(no daemon) against every `task.test.ts` under **both** `tasks/buildin/**`
and `tasks/examples/**`, plus `tasks/examples/repo-prune/prune-stale-refs.test.sh`
(a shell-level suite that drives the real prune script against a throwaway
repo — `task.test.ts` only mocks `Deno.Command`, so it never exercises the
script that actually deletes branches/worktrees). `make test-tasks` runs the
same three commands locally. Only `runtime: deno` tasks can ship a
`task.test.ts` today — the harness below doesn't support Python/Docker/Podman
(#159) — so nothing under `tasks/examples/**` is silently excluded; there's
simply nothing else the glob could match yet.

### CI job: `test-tasks-cli`

The `.github/workflows/ci.yml` job `test-tasks-cli` boots the dicode daemon
against the built-in task set (using `ci/dicode-tasktest.yaml`) and runs:

```bash
./dicode task test buildin/webui --format=junit
```

The resulting JUnit XML is uploaded as a workflow artifact. The daemon log is
uploaded on failure for post-mortem inspection.

`pkg/tasktest` itself only shells out to `deno test` and parses the summary
line — it does not provide any mocking on its own. The mocked globals below
(`http.mock`, `env.set`, `runTask()`, `assert.*`) come from a separate,
repo-local test harness: `tasks/sdk-test.ts`. A `task.test.ts` opts in with:

```typescript
import { setupHarness } from "../../sdk-test.ts";
await setupHarness(import.meta.url);
```

`setupHarness` dynamically imports the sibling `task.ts`'s default export,
intercepts `fetch` and `Deno.env.get`, and installs `test`/`params`/`env`/`kv`/
`http`/`assert`/`runTask`/`dicode` as globals for the rest of the file. See
[tasks/buildin/webui/task.test.ts](../../tasks/buildin/webui/task.test.ts) and
[tasks/buildin/blob-storage/task.test.ts](../../tasks/buildin/blob-storage/task.test.ts)
for working examples. This harness ships in this repo for the built-in/example
tasks; it is not (yet) published as a standalone package for external task
repos to import.

### Test file format

```javascript
// task.test.js
import { setupHarness } from "../../sdk-test.ts";
await setupHarness(import.meta.url);

test("sends digest when emails present", async () => {
  // Set up mocks
  http.mock("GET", "https://gmail.googleapis.com/*", {
    status: 200,
    body: { messages: [{ id: "1", snippet: "Hello" }] }
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
  assert.httpCalledWith("POST", "https://slack.com/api/chat.postMessage", {
    body: { channel: "#test", text: /1 unread email/ }
  })
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
  assert.httpNotCalled("POST", "https://slack.com/api/chat.postMessage")
})
```

### Mock API

**`http.mock(method, urlPattern, response)`** — intercept outbound HTTP calls
- `method`: `GET`, `POST`, `PUT`, `PATCH`, `DELETE`, or `*` (any)
- `urlPattern`: exact URL or glob pattern (`*`, `**` supported)
- `response`: `{ status, headers, body }`. `body` objects are JSON-serialized.
- Unmatched calls throw an error (no accidental real HTTP in tests)

**`http.mockOnce(method, urlPattern, response)`** — like `http.mock` but consumed after one matching call

**`http.lastRequestBody(method, urlPattern)`** — the body of the most recent matching call

**`env.set(key, value)`** — set env/secret values for this test (backs `Deno.env.get`)

**`params.set(name, value)`** — set parameter values for this test

**`kv.set(key, value)` / `kv.get(key)` / `kv.delete(key)` / `kv.list(prefix?)`** — pre-populate or inspect the in-memory KV store for this test (there is no separate `seed` method — use `kv.set`)

**`runTask()`** — invoke `task.ts`'s default export with the mocked SDK context. Returns the task's return value.

### Assert API

**`assert.equal(actual, expected, msg?)`** — deep equality check
**`assert.ok(value, msg?)`** — truthy check
**`assert.throws(fn, pattern?)`** — asserts `fn` throws an error matching `pattern`
**`assert.httpCalled(method, urlPattern)`** — assert HTTP mock was called
**`assert.httpNotCalled(method, urlPattern)`** — assert HTTP mock was NOT called
**`assert.httpCalledWith(method, urlPattern, { body })`** — assert call with a specific body

There is no `assert.httpCallCount` — assert on `http.lastRequestBody` or count matches yourself if you need exact call counts.

### Test isolation

All tests in a `task.test.ts` run in the same Deno process — there's no per-test goja/interpreter isolation. `setupHarness`'s `test()` wrapper calls `resetMocks()` before each case, which clears the in-memory params/env/kv maps, HTTP mocks, and call log, and re-seeds params from `task.yaml` defaults. State does not leak between test cases as long as you go through the mocked globals rather than module-level variables in `task.ts`.

### Output

```
morning-email-check
  ✅ sends digest when emails present
  ✅ handles empty inbox gracefully

daily-backup
  ✅ backs up all tables
  ❌ handles connection failure
     AssertionError: expected http called POST https://slack.com/api/chat.postMessage
     at assert.httpCalled (test.js:31)

2 passed, 1 failed
```

---

## Layer 3 — Dry run (planned design — not implemented)

```bash
dicode run <id> --dry-run
dicode run <id> --dry-run --verbose
```

Runs the task with:
- **Real secrets** resolved from the configured providers
- **Real execution** of the task script
- **Intercepted HTTP** — all outbound calls are logged but not sent
- **No KV writes** — KV reads return current values, writes are logged and discarded

Useful for verifying that secret resolution works and the task targets the right endpoints before a live run.

**Output:**
```
[dry-run] fetch-emails
  → Deno.env.get("GMAIL_TOKEN") = "xoxb-..." [resolved from local store]
  → http.get("https://gmail.googleapis.com/gmail/v1/users/me/messages") [intercepted]
  ← would return { status: 200, body: [mock response omitted] }
  → return { count: 42 }
```

---

## Layer 4 — CI integration (planned design — not implemented)

```bash
dicode ci init --github
dicode ci init --gitlab
```

Generates a CI workflow that runs layers 1+2 on every push. Entirely offline — no secrets, no database, no network access required.

**Generated GitHub Actions workflow:**
```yaml
# .github/workflows/dicode.yml
name: Dicode task validation
on: [push, pull_request]

jobs:
  validate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: dicode/setup-action@v1    # installs dicode binary
      - run: dicode task validate --all
      - run: dicode task test --all
```

The `dicode/setup-action` downloads the dicode binary for the current platform and version — no Go toolchain required.

**GitLab CI:**
```yaml
# .gitlab-ci.yml
dicode:
  stage: test
  image: ubuntu:22.04
  script:
    - curl -sL https://dicode.app/install.sh | sh
    - dicode task validate --all
    - dicode task test --all
```

---

## AI-generated tests

When the AI generator creates `task.js`, it also generates `task.test.js`. Both are shown in the diff before the user confirms. The AI retry loop (max 3 attempts) fixes test files too if they fail validation.

Rule of thumb: if the AI can't generate passing tests for a task it just wrote, it's a signal the task logic is wrong.

---

## Summary

| Command | What it checks | Needs secrets? | Needs network? | Status |
|---|---|---|---|---|
| `dicode task validate` | Schema, syntax, cycles | ⚠️ warns if missing | No | Planned |
| `dicode task test <id>` | Unit tests, mocks via `tasks/sdk-test.ts` | No | No | Implemented (Deno) |
| `dicode run <id> --dry-run` | End-to-end with intercepted HTTP | Yes | No | Planned |
| `dicode run <id>` | Live execution | Yes | Yes | Implemented |
