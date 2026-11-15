# Task → Orchestrator API

The `dicode` global is a communication channel between a running task script and the dicode daemon. It lets tasks run other tasks, query orchestrator state, read config, and manage secrets.

> **Runtime note**: In Deno tasks all methods are async (return Promises). In Python tasks both sync and `_async` variants are available.

---

## Implemented methods

### `dicode.run_task()`

Run another task and wait for it to complete. Returns the run result.

```typescript
// Deno
const result = await dicode.run_task("send-report", { channel: "#ops" })
// result: { runID, status, returnValue }
```

```python
# Python
result = dicode.run_task("send-report", {"channel": "#ops"})
```

**Parameters:**
- `taskID` (string) — the ID of the task to run
- `params` (object, optional) — key-value params passed to the task

**Returns:** `{ runID, status, returnValue }` — blocks until the run completes.

**Requires:** `security.allowed_tasks` in `task.yaml` — tasks must declare which other tasks they can call.

---

### `dicode.list_tasks()`

List all registered tasks.

```typescript
// Deno
const tasks = await dicode.list_tasks()
// [{ id, name, description, params }, ...]
```

```python
# Python
tasks = dicode.list_tasks()
```

---

### `dicode.get_runs()`

Fetch recent run history for a task.

```typescript
// Deno
const runs = await dicode.get_runs("morning-email-check", { limit: 5 })
```

```python
# Python
runs = dicode.get_runs("morning-email-check", limit=5)
```

---

### `dicode.secrets_set()` / `dicode.secrets_delete()`

Manage secrets from within a task. Write-only: a task can never read a secret
back. Requires `permissions.dicode.secrets_write: true`.

```typescript
// Deno
await dicode.secrets_set("MY_TOKEN", "new-value")
await dicode.secrets_delete("OLD_TOKEN")
```

```python
# Python
dicode.secrets_set("MY_TOKEN", "new-value")
dicode.secrets_delete("OLD_TOKEN")
```

---

### `dicode.crypto.encrypt()` / `dicode.crypto.decrypt()`

Encrypt and decrypt bytes under a context-scoped sub-key. Requires the context
in `permissions.dicode.crypto`. Deno takes and returns `Uint8Array`; Python takes
and returns `bytes`.

```typescript
// Deno
const blob = await dicode.crypto.encrypt("my-ctx", new TextEncoder().encode("hi"))
const data = await dicode.crypto.decrypt("my-ctx", blob)
```

```python
# Python
blob = dicode.crypto.encrypt("my-ctx", b"hi")
data = dicode.crypto.decrypt("my-ctx", blob)
```

---

### `dicode.runs.*` — run-input and resume-state retention

Each method needs its own `permissions.dicode.runs_*` flag (see
[task format](task-format.md)). Python takes the same arguments in snake_case.

| Deno | Python |
|---|---|
| `runs.list_expired({ before_ts? })` | `runs.list_expired(before_ts=0)` |
| `runs.delete_input(runID)` | `runs.delete_input(run_id)` |
| `runs.delete_inputs(runIDs)` | `runs.delete_inputs(run_ids)` |
| `runs.pin_input(runID)` / `runs.unpin_input(runID)` | `runs.pin_input(run_id)` / `runs.unpin_input(run_id)` |
| `runs.get_input(runID)` | `runs.get_input(run_id)` |
| `runs.replay(runID, taskName?)` | `runs.replay(run_id, task_name=None)` |
| `runs.list_expired_resume_state({ before_ts? })` | `runs.list_expired_resume_state(before_ts=0)` |
| `runs.delete_resume_state(runID)` | `runs.delete_resume_state(run_id)` |

---

### `dicode.suspend(req)`

Pause the run and request input (a form, an approval, a webhook payload) before continuing. See [Suspendable tasks](suspendable-tasks.md) for the full request shape, resume semantics, and JSON-Schema-driven forms.

```typescript
// Deno
await dicode.suspend({ schema, state })
```

```python
# Python
dicode.suspend(state=..., schema=...)
```

---

### `dicode.secrets.has(key)`

Return `true` if a secret with the given key exists in the secrets store, without ever returning the value. Available in both SDKs.

```typescript
// Deno
const exists = await dicode.secrets.has("MY_TOKEN")
```

```python
# Python
exists = dicode.secrets.has("MY_TOKEN")
```

---

## Planned methods (not yet implemented)

| Method | Description |
|---|---|
| `dicode.progress(msg, data)` | Stream intermediate progress to WebUI |
| `dicode.trigger(id, payload)` | Fire-and-forget dispatch (non-blocking) |
| `dicode.isRunning(id)` | Check if a task has an active run |
| `dicode.listSecrets()` | List secret names (no values) |

---

## Summary

| Method | Deno | Python | Status |
|---|---|---|---|
| `run_task(id, params?)` | `await dicode.run_task(...)` | `dicode.run_task(...)` | Implemented |
| `list_tasks()` | `await dicode.list_tasks()` | `dicode.list_tasks()` | Implemented |
| `get_runs(id, opts?)` | `await dicode.get_runs(...)` | `dicode.get_runs(...)` | Implemented |
| `secrets_set(key, val)` | `await dicode.secrets_set(...)` | `dicode.secrets_set(...)` | Implemented |
| `secrets_delete(key)` | `await dicode.secrets_delete(...)` | `dicode.secrets_delete(...)` | Implemented |
| `crypto.encrypt/decrypt` | `await dicode.crypto.encrypt(...)` | `dicode.crypto.encrypt(...)` | Implemented |
| `runs.*` | `await dicode.runs.list_expired(...)` | `dicode.runs.list_expired(...)` | Implemented |
| `secrets.has(key)` | `await dicode.secrets.has(...)` | `dicode.secrets.has(...)` | Implemented |
| `suspend(req)` | `await dicode.suspend(...)` | `dicode.suspend(...)` | Implemented |
| `progress(msg, data)` | — | — | Planned |
| `trigger(id, payload)` | — | — | Planned |
| `isRunning(id)` | — | — | Planned |
