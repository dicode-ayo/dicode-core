# Testing & Validation

> **Status**: This document describes the **planned** testing and validation system. The test harness (`pkg/testing/`), CLI commands (`dicode task validate`, `dicode task test`, `dicode task run --dry-run`, `dicode ci init`), and mock API are not yet implemented. This serves as the design spec for future development.

Dicode is designed with four validation layers, each catching different classes of problems.

```
Layer 1: Static validation     — schema + syntax, zero execution, instant        [planned]
Layer 2: Unit tests            — mocked globals, full task run, local             [planned]
Layer 3: Dry run               — real secrets, intercepted HTTP, no side effects  [planned]
Layer 4: CI guardrails         — layers 1+2 on every push, offline-safe           [planned]
```

---

## Layer 1 — Static validation

```bash
dicode task validate <id>
dicode task validate --all
```

Checks performed without executing any code:
- `task.yaml` schema validation (required fields, valid cron expression, valid `chain.on` value)
- `task.js` JavaScript syntax via goja compile-without-execute
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
dicode task test --all
dicode task test <id> --watch   # re-run on file save
```

Runs `task.test.js` with mocked globals. No real HTTP calls, no real secrets, no database side effects.

### Test file format

```javascript
// task.test.js

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

**`env.set(key, value)`** — set env/secret values for this test

**`params.set(name, value)`** — set parameter values for this test

**`kv.seed({ key: value })`** — pre-populate KV store for this test

**`runTask()`** — execute `task.js` with the mocked globals. Returns the task's return value.

### Assert API

**`assert.equal(actual, expected)`** — deep equality check
**`assert.ok(value)`** — truthy check
**`assert.throws(fn, pattern)`** — asserts `fn` throws an error matching `pattern`
**`assert.httpCalled(method, urlPattern)`** — assert HTTP mock was called
**`assert.httpNotCalled(method, urlPattern)`** — assert HTTP mock was NOT called
**`assert.httpCalledWith(method, urlPattern, options)`** — assert call with specific body/headers
**`assert.httpCallCount(method, urlPattern, n)`** — assert exact call count

### Test isolation

Each `test()` block runs in its own fresh goja runtime. State does not leak between test cases — mocks, env vars, params, and KV are reset between each `test()`.

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

## Layer 3 — Dry run

```bash
dicode task run <id> --dry-run
dicode task run <id> --dry-run --verbose
```

Runs the task with:
- **Real secrets** resolved from the configured providers
- **Real execution** of the task script
- **Intercepted HTTP** — all outbound calls are logged but not sent
- **No KV writes** — KV reads return current values, writes are logged and discarded
- **No notifications** — `notify.send()` is logged and discarded

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

## Layer 4 — CI integration

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

| Command | What it checks | Needs secrets? | Needs network? |
|---|---|---|---|
| `dicode task validate` | Schema, syntax, cycles | ⚠️ warns if missing | No |
| `dicode task test` | Unit tests with mocks | No | No |
| `dicode task run --dry-run` | End-to-end with intercepted HTTP | Yes | No |
| `dicode task run` | Live execution | Yes | Yes |
