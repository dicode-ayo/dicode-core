# Testing & Validation

> **Status**: This document describes the testing and validation system. Layer 2
> (`dicode task test`) is implemented for the Deno and Python runtimes:
> `pkg/tasktest` drives `deno test` (Deno) or `uv run` + pytest (Python) and
> parses each runtime's summary output. `tasks/sdk-test.ts` (Deno) and
> `tasks/sdk_test.py` (Python) provide real (opt-in) mock harnesses —
> `params`/`env`/`kv`/HTTP mocking/`runTask()`|`run_task()` — used by this
> repo's built-in and example tasks. Docker and Podman are **not yet
> supported** (tracked as Phase 3 of [#159](https://github.com/dicode-ayo/dicode-core/issues/159)).
> Layer 1 (`dicode task validate`), Layer 3 (`dicode run --dry-run`), and
> `dicode ci init` remain planned.

Dicode is designed with four validation layers, each catching different classes of problems.

```text
Layer 1: Static validation     — schema + syntax, zero execution, instant        [planned]
Layer 2: Unit tests            — mocked globals, full task run, local             [implemented: Deno, Python]
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

Runs the task's sibling `task.test.*` through its runtime's test runner:
`task.test.ts`/`.js`/`.mjs` (Deno runtime) through `deno test`, or
`task.test.py` (Python runtime) through `uv run` + pytest. `pkg/tasktest`
captures aggregate passed/failed/skipped counts from each runtime's own
summary output — Deno's `ok | N passed | N failed (Nms)` line, or pytest's
`N passed, N failed in N.NNs` line. Docker and Podman tasks cannot ship a
test file yet (#159 Phase 3).

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
`task.test.ts` — `runtime: python` tasks ship a `task.test.py` instead (see
[Python](#python) below); Docker/Podman tasks can't ship a test file at all
yet (#159 Phase 3).

### CI job: `test-tasks-cli`

The `.github/workflows/ci.yml` job `test-tasks-cli` boots the dicode daemon
against the built-in task set (using `ci/dicode-tasktest.yaml`) and runs:

```bash
./dicode task test buildin/webui --format=junit
```

The resulting JUnit XML is uploaded as a workflow artifact. The daemon log is
uploaded on failure for post-mortem inspection. `buildin/webui` is a Deno
task; there is currently no built-in Python task in the daemon's task set for
this job to exercise the same way, so this job's daemon-backed coverage stays
Deno-only for now. Python coverage instead comes from `pkg/tasktest`'s own Go
tests (`TestRun_Python`, which drives `runPython` through a real `uv`
subprocess) plus a dedicated `test-tasks` job step that runs
`tasks/examples/hello-python/task.test.py` directly via `uv run` (no
daemon needed — see that job's YAML for why a task.test.py can be invoked
standalone).

`pkg/tasktest` itself only shells out to the runtime's own test runner
(`deno test`, or `uv run` for Python) and parses its summary output — it does
not provide any mocking on its own. Mocking comes from a separate,
repo-local test harness per runtime: `tasks/sdk-test.ts` (Deno) or
`tasks/sdk_test.py` (Python).

## Deno

A `task.test.ts` opts into the harness with:

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

## Python

`runtime: python` tasks ship a `task.test.py` next to `task.py`. Two things
make it work with `dicode task test`/`pkg/tasktest.runPython`, both
non-negotiable:

1. **It's a PEP 723 script.** `pkg/tasktest` invokes it as a single
   `uv run <task.test.py>` — no extra flags, no `--with` packages bolted on
   from the Go side. That means the file's own inline `# /// script`
   header must declare every dependency it needs: `pytest`, plus whatever
   the adjacent `task.py` imports (e.g. `httpx`).
2. **It ends with a `run_pytest_main(__file__)` call under
   `if __name__ == "__main__":`.** `uv run` executes the file as `__main__`;
   that block is what actually invokes pytest. `run_pytest_main` (from
   `sdk_test.py`) also bakes in `--import-mode=importlib`, which is required
   — pytest's default import mode derives a dotted module name from a file's
   basename by stripping only the trailing `.py`, so `task.test.py` becomes
   module name `task.test`, which Python then tries to import as submodule
   `test` of package `task` and fails with `ModuleNotFoundError: No module
   named 'task'`. `--import-mode=importlib` sidesteps this entirely.

```python
# task.test.py
# /// script
# requires-python = ">=3.11"
# dependencies = ["pytest", "httpx>=0.27"]
# ///
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))
from sdk_test import (
    _dicode_harness_reset,  # noqa: F401 — registers the autouse per-test reset fixture
    http,
    params,
    run_pytest_main,
    run_task,
    setup_harness,
)

setup_harness(__file__)


def test_greets_by_name():
    params.set("name", "Ada")
    http.mock("GET", "https://httpbin.org/get", {"status": 200, "body": {"origin": "127.0.0.1"}})

    result = run_task()

    assert result["greeting"] == "Hello, Ada! (run #1)"


if __name__ == "__main__":
    run_pytest_main(__file__)
```

See [tasks/examples/hello-python/task.test.py](../../tasks/examples/hello-python/task.test.py)
for a complete, working example (params, kv persistence, httpx mocking, and
an explicit "unmocked call fails loudly" test), and
[tasks/sdk_test.py](../../tasks/sdk_test.py)'s module docstring for the full
design rationale.

### Mock API

**`http.mock(method, urlPattern, response)`** / **`http.mock_once(...)`** — intercept `httpx` calls (`httpx.Client`/`httpx.AsyncClient`, both sync and async — the client every Python example task in this repo uses today). `response` is `{"status": ..., "headers": {...}, "body": ...}`; dict/list bodies are returned as a JSON response. Tasks using `urllib`/`requests` directly instead of `httpx` are **not** intercepted by this harness — plain stdlib HTTP mocking is out of scope for this pass.

**`http.last_request_body(method, urlPattern)`** — the body of the most recent matching call

**`env.set(key, value)`** / **`env.get(key, default=None)`** — env values for this test (independent of real `os.environ` — a task that reads `os.environ` directly, like `hello-python`'s `HTTPBIN_URL` lookup, bypasses this mock; set real env vars via `monkeypatch.setenv` in that case)

**`params.set(name, value)`** / **`params.get(name, default=None)`** / **`params.get_async(...)`** — parameter values for this test

**`kv.set(key, value)` / `kv.get(key)` / `kv.delete(key)` / `kv.list(prefix="")`** (plus `_async` variants) — pre-populate or inspect the in-memory KV store

**`run_task()`** — exec the sibling `task.py`'s body (PEP 723 header stripped) against the mocked globals and call its `main()` (sync or async, auto-detected), returning its result. Falls back to the module-level `result` variable for a no-`main` task.

**`dicode`** — a `MockDicode` shaped like the real `dicode` module (`run_task`, `list_tasks`, `get_runs`, `set_group` + `.group_calls`, `.runs`/`.tasks`/`.sources`/`.git`/`.secrets`/`.audit`). `suspend()`/resume simulation is out of scope for this harness.

### Assert API

Python's `assert` statement is a keyword, so unlike the TS harness there is no `assert.equal`/`assert.ok` namespace — use plain `assert` statements (pytest rewrites them with full introspection on failure). HTTP-specific assertions are free functions:

**`assert_http_called(method, urlPattern)`** / **`assert_http_not_called(method, urlPattern)`** / **`assert_http_called_with(method, urlPattern, body=...)`**

### Test isolation

Unlike the Deno harness (which imports `task.ts` once and reuses the loaded module across every `test()` case), `run_task()` re-reads and re-`exec()`s `task.py`'s body fresh on every call — there is no task-module-level state to leak between test cases in the first place. The autouse `_dicode_harness_reset` pytest fixture (import it by name into your test file to register it — pytest discovers fixtures via a test's enclosing module namespace) clears params/env/kv/http mocks/call log before every test function.

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

When the AI generator creates `task.ts`, it also generates `task.test.ts`. Both are shown in the diff before the user confirms. The AI retry loop (max 3 attempts) fixes test files too if they fail validation.

Rule of thumb: if the AI can't generate passing tests for a task it just wrote, it's a signal the task logic is wrong.

---

## Summary

| Command | What it checks | Needs secrets? | Needs network? | Status |
|---|---|---|---|---|
| `dicode task validate` | Schema, syntax, cycles | ⚠️ warns if missing | No | Planned |
| `dicode task test <id>` | Unit tests, mocks via `tasks/sdk-test.ts` / `tasks/sdk_test.py` | No | No | Implemented (Deno, Python); Docker/Podman tracked as #159 Phase 3 |
| `dicode run <id> --dry-run` | End-to-end with intercepted HTTP | Yes | No | Planned |
| `dicode run <id>` | Live execution | Yes | Yes | Implemented |
