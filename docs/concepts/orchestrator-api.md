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

### `dicode.secrets_set()` / `dicode.secrets_delete()` (Deno only)

Manage secrets from within a task.

```typescript
await dicode.secrets_set("MY_TOKEN", "new-value")
await dicode.secrets_delete("OLD_TOKEN")
```

Not available in the Python SDK.

---

## Planned methods (not yet implemented)

| Method | Description |
|---|---|
| `dicode.progress(msg, data)` | Stream intermediate progress to WebUI |
| `dicode.trigger(id, payload)` | Fire-and-forget dispatch (non-blocking) |
| `dicode.isRunning(id)` | Check if a task has an active run |
| `dicode.ask(question, opts)` | Suspend run and request human approval (north star) |
| `dicode.listSecrets()` | List secret names (no values) |

---

## Summary

| Method | Deno | Python | Status |
|---|---|---|---|
| `run_task(id, params?)` | `await dicode.run_task(...)` | `dicode.run_task(...)` | Implemented |
| `list_tasks()` | `await dicode.list_tasks()` | `dicode.list_tasks()` | Implemented |
| `get_runs(id, opts?)` | `await dicode.get_runs(...)` | `dicode.get_runs(...)` | Implemented |
| `secrets_set(key, val)` | `await dicode.secrets_set(...)` | — | Implemented (Deno only) |
| `secrets_delete(key)` | `await dicode.secrets_delete(...)` | — | Implemented (Deno only) |
| `progress(msg, data)` | — | — | Planned |
| `trigger(id, payload)` | — | — | Planned |
| `isRunning(id)` | — | — | Planned |
| `ask(question, opts)` | — | — | Planned (north star) |
