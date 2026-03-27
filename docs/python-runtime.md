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
import os

# Parameters are available as DICODE_PARAM_<NAME> (uppercased).
limit = int(os.environ.get("DICODE_PARAM_LIMIT", "10"))

# Env vars declared in task.yaml under `env:` are inherited from the host.
db_url = os.environ.get("DATABASE_URL")

print(f"Processing up to {limit} rows from {db_url}")
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
print(resp.json())
```

uv creates a dedicated virtual environment per script on first run and caches
it for subsequent runs. Cache location: `~/.cache/uv/`.

---

## Run context

| Environment variable | Value |
|---|---|
| `DICODE_RUN_ID` | The current run ID |
| `DICODE_PARAM_<NAME>` | Each task param value (name uppercased) |
| *(all `env:` vars)* | Inherited from the host process |

Task params listed in `task.yaml` under `params:` are merged (defaults +
per-run overrides) before the script starts.

---

## Stdout / stderr

- **stdout** lines → info-level log entries (visible in the Run detail page)
- **stderr** lines → warn-level log entries

---

## Differences from the Deno runtime

| Feature | Deno | Python |
|---|---|---|
| Binary management | dicode downloads `deno` | dicode downloads `uv` |
| SDK globals (`log`, `kv`, …) | Yes — injected via shim | No — use env vars + stdout |
| Dependency management | npm / jsr imports | PEP 723 inline deps via uv |
| Filesystem sandboxing | Yes — `--allow-read/write` | No — inherits host permissions |
| Return value / rich output | `output.html(…)`, `return` | Not supported yet |
| Chain trigger input | `input` global | Not supported yet |

SDK globals and rich output for Python are planned for a future release.

---

## Configuration reference

```yaml
runtimes:
  python:
    version: "0.7.3"   # uv version; leave blank to use the dicode default
    disabled: false     # set true to prevent registration at startup
```

See [Task Format](./concepts/task-format.md) for the full `task.yaml` reference.
