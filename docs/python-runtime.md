# Python Runtime

dicode supports Python tasks via [uv](https://github.com/astral-sh/uv) — a
fast Python package manager and script runner. dicode downloads and caches the
uv binary automatically; no system Python, pip, or virtual-environment setup
is required.

---

## Setup

1. Open **Config → Runtimes** in the dicode web UI.
2. Find **Python (uv)** in the table.
3. Optionally change the version (defaults to `0.7.3`).
4. Click **Install** — dicode downloads the uv binary to `~/.cache/dicode/uv/<version>/uv`.
5. Tasks with `runtime: python` will now run.

Alternatively, add this to `dicode.yaml` and restart dicode — the runtime is
registered automatically if the binary is already cached:

```yaml
runtimes:
  python:
    version: "0.7.3"
```

---

## Task structure

```
tasks/
└── my-python-task/
    ├── task.yaml
    └── task.py
```

### task.yaml

```yaml
name: My Python Task
runtime: python
trigger:
  manual: true

params:
  - name: limit
    default: "10"
    description: Maximum rows to process

env:
  - DATABASE_URL
```

### task.py

```python
# SDK globals are injected automatically — no imports needed.

name = params.get("name")
db_url = env.get("DATABASE_URL")

log.info(f"Processing up to {name} rows from {db_url}")

previous = kv.get("last_run_count")
if previous:
    log.info(f"Last run processed: {previous}")

kv.set("last_run_count", 42)

result = {"processed": 42}
```

---

## SDK globals

The Python runtime injects the same SDK globals as the Deno runtime via a Unix
socket bridge. No imports are needed — all globals are available at module level.

### `log`

```python
log.info("message", extra_arg)
log.warn("something looks off")
log.error("it broke")
log.debug("verbose detail")
```

### `params`

```python
value = params.get("my_param")          # returns default if not set
all_params = params.all()               # dict of all params
```

### `env`

```python
token = env.get("SLACK_TOKEN")          # reads from host environment
```

### `kv`

Persistent key-value store scoped to the task.

```python
kv.set("counter", 42)
value = kv.get("counter")              # returns None if not set
keys  = kv.list()                      # list all keys
keys  = kv.list("prefix_")            # list keys with prefix
kv.delete("counter")
```

### `input`

The return value of the upstream task (chain triggers). `None` for other trigger types.

```python
if input:
    log.info(f"upstream returned: {input}")
```

### `output`

Rich output types rendered in the Web UI.

```python
output.html("<h1>Report</h1><table>...</table>")
output.text("plain text result")
output.image("image/png", base64_data)
output.file("report.csv", csv_content, "text/csv")

# HTML with structured data for chain triggers
output.html(html, data={"count": 5})   # chained tasks receive {"count": 5}
```

### Return value

Assign `result` at module level. The value is passed to chained tasks via `input`.

```python
result = {"count": 42, "status": "ok"}
```

### `suspend` — pause for human input, auto-dispatched

Pause a run to collect input from a human. `dicode.suspend(schema=...)` never
returns — it raises a control signal, the process exits, the run becomes
`suspended`, and on resume the runner re-runs the file and **dispatches the right
handler for you**: no hand-rolled `if ctx.state is None` switch. Each handler
reads `ctx.state` (the blob you carried) and `ctx.input` (the validated
submission), both `None` on a first run.

Define **`main`** (the first run) and optionally **`resume`** (the continuation):

```python
async def main():
    dicode.suspend(
        schema={
            "type": "object",
            "properties": {"ok": {"type": "boolean", "title": "OK?"}},
            "required": ["ok"],
        },
    )  # unreachable — never returns

async def resume():
    return {"confirmed": ctx.input["ok"]}
```

For a multi-step wizard, define a **`steps`** map and name the next handler with
`suspend(to=...)`. A `to` that names no defined step fails the run loudly rather
than falling back to `main`/`resume`. A handler may also take `ctx` as an argument
(`async def resume(ctx):`) instead of reading the module-global `ctx`. `schema`
is a [JSON Schema](https://json-schema.org) (draft 2020-12) the daemon compiles at
`suspend()` time (rejecting a malformed schema up front) and validates against
before resuming.

Do not wrap `suspend()` in a `try/except` that swallows the signal — the run
fails loudly if you do. See [Suspendable Tasks](./concepts/suspendable-tasks.md)
for the dispatch rules, the `steps` wizard shape, lifecycle (`suspended` →
`resumed`), and how to resume via the Web UI or `dicode resume`.

### Async tasks

Define `async def main()` and return a value from it. The shim detects the coroutine and runs it with `asyncio.run()`.

```python
async def main():
    email = await params.get_async("email")
    await log.info_async(f"processing {email}")
    count = await kv.get_async("count") or 0
    await kv.set_async("count", count + 1)
    return {"email": email, "count": count + 1}
```

All SDK globals expose `_async` variants (`log.info_async`, `kv.get_async`, `params.get_async`, `dicode.run_task_async`, `mcp.call_async`, etc.) that are non-blocking from within an async context.

> **Implementation note**: `_async` methods submit requests directly to the background IO loop via `asyncio.wrap_future` — no thread pool is involved. Many concurrent `asyncio.gather` calls are safe and do not block each other.

Sync-style scripts (module-level code with `result = ...`) continue to work unchanged — the async interface is opt-in.

### `dicode` — task orchestration

Allows a task to orchestrate other tasks. Requires `security.allowed_tasks` in `task.yaml`.

```python
# Run another task and await its result
result = dicode.run_task("send-report", {"channel": "#ops"})
# result: { "runID": ..., "status": ..., "returnValue": ... }

# List all registered tasks
tasks = dicode.list_tasks()

# Get recent run history
runs = dicode.get_runs("send-report", limit=5)
```

```yaml
# task.yaml
security:
  allowed_tasks:
    - "send-report"
    - "*"   # or allow all
```

### `mcp` — MCP server tools

Calls tools on daemon tasks that declare `mcp_port`. Requires `security.allowed_mcp`.

```python
tools  = mcp.list_tools("github-mcp")
result = mcp.call("github-mcp", "search_repositories", {"query": "dicode"})
```

```yaml
# task.yaml
security:
  allowed_mcp:
    - "github-mcp"
```

---

## Inline dependencies (PEP 723)

uv supports inline dependency declarations directly inside the script — no
`requirements.txt` or `pyproject.toml` needed:

```python
# /// script
# dependencies = ["requests>=2.31", "boto3>=1.34"]
# requires-python = ">=3.11"
# ///

import requests
import boto3

resp = requests.get("https://api.example.com/data")
log.info(str(resp.json()))

result = resp.json()
```

The `# /// script` block must appear near the top of `task.py`. uv creates a
dedicated virtual environment per script on first run and caches it for
subsequent runs (`~/.cache/uv/`).

### Dependency pinning

If a `task.py.lock` sidecar exists next to `task.py` (written by
`uv lock --script`), the runtime stages it alongside the temporary wrapper and
invokes uv with `--locked`. This prevents per-run resolution of newer versions
within a range (`>=`, `~=`, `^`-style caret ranges) — packages are pinned to
the exact versions and hashes recorded in the sidecar, and a stale lock fails
the run loudly instead of silently re-resolving. Tasks without a sidecar (for
example, tasks with no PEP 723 block at all) run exactly as before — the same
degrade behaviour as the Deno runtime when no `deno.lock` is present.

When a task's dependencies change, regenerate the sidecar with
`dicode python relock [dir]` (dir defaults to `tasks`). It provisions the
pinned uv and runs `uv lock --script` for every `task.py` under the tree that
carries a PEP 723 block, so the locks are deterministic regardless of any
system uv; sidecars orphaned by a removed block are deleted. `dicode python
relock --check` verifies the sidecars without modifying them (exit non-zero if
one is missing, stale, or orphaned) — run it in CI to catch drift before it
reaches the runtime. Buildin/example tasks ship committed `task.py.lock`
sidecars that are automatically detected and enforced.

The runtime-spanning `dicode relock [--check] [dir]` runs the Deno and Python
lock passes together for whichever task kinds exist under the tree — one
command (and one CI step) covering both `tasks/deno.lock` and the
`task.py.lock` sidecars.

> **Pin `requires-python` for a reproducible lock.** A lock only reproduces if
> the PEP 723 block declares a `requires-python` constraint. Without one, uv
> resolves against whatever Python is default in the current environment
> (e.g. `>=3.11` on a dev box vs `>=3.12` in CI), producing a *different* lock
> that then fails `--locked` on the other machine. `dicode python relock` warns
> when a lockable script omits it — add e.g. `# requires-python = ">=3.11"` to
> the block.

---

## Run context

In addition to SDK globals, the following environment variables are always set:

| Environment variable | Value |
|---|---|
| `DICODE_RUN_ID` | The current run ID |
| *(all `env:` vars)* | Inherited from the host process |

---

## Differences from the Deno runtime

| Feature | Deno | Python |
| --- | --- | --- |
| Binary management | dicode downloads `deno` | dicode downloads `uv` |
| SDK globals (`log`, `kv`, `dicode`, `mcp`, …) | Yes — injected via JS shim | Yes — injected via `dicode_sdk.py` shim |
| Dependency management | npm / jsr imports | PEP 723 inline deps via uv |
| Filesystem sandboxing | Yes — `--allow-read/write` | No — inherits host permissions |
| Return value | `return` statement | `result = ...` module-level variable, or `return` from `async def main()` |
| Rich output | `output.html(…)`, etc. | Same — `output.html(…)`, etc. |
| Chain trigger input | `input` global | `input` global |
| Agent orchestration (`dicode`, `mcp`) | Yes | Yes |

---

## Configuration reference

```yaml
runtimes:
  python:
    version: "0.7.3"   # uv version; leave blank to use the dicode default
    disabled: false     # set true to prevent registration at startup
```

See [Task Format](./concepts/task-format.md) for the full `task.yaml` reference.
