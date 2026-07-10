# Task Chaining

Tasks can be chained so that the output of one task becomes the input of another. Dicode supports two chaining styles: **declarative** (chain trigger) and **imperative** (`dicode.run_task()`).

---

## Declarative chain triggers

TaskB declares that it should run when TaskA completes. TaskA is completely unaware of TaskB.

```yaml
# task-b/task.yaml
trigger:
  chain:
    from: fetch-emails
    on: success
```

```javascript
// task-a/task.js — fetch-emails
const emails = await fetchEmails()
return { emails, count: emails.length }   // returned value captured by dicode

// task-b/task.js — send-slack-digest
console.log(`Sending digest of ${input.count} emails`)
await postToSlack(input.emails)
```

**How it works:**

1. `fetch-emails` completes successfully
2. The runner checks the registry for tasks with `trigger.chain.from == "fetch-emails"` and `on` matching the outcome
3. `send-slack-digest` is dispatched with `fetch-emails`'s return value injected as `input`
4. The chained run is stored as a child run (parent_run_id foreign key in sqlite)

**`on` values:**

| `on` | Fires when |
|---|---|
| `success` (default) | TaskA completed without uncaught exception |
| `failure` | TaskA threw an uncaught exception |
| `always` | Either outcome |

---

## Linear chain example

```
fetch-emails → send-slack-digest → archive-emails
```

```yaml
# send-slack-digest/task.yaml
trigger:
  chain:
    from: fetch-emails
    on: success

# archive-emails/task.yaml
trigger:
  chain:
    from: send-slack-digest
    on: always   # archive even if digest send failed
```

Each task only knows about its immediate predecessor. Adding a new step (e.g. a logging task after archive) doesn't require modifying any existing tasks.

---

## Fan-out

Multiple tasks can declare `chain.from` the same upstream task. They all fire in parallel when that task completes.

```yaml
# notify-slack/task.yaml
trigger:
  chain:
    from: run-report

# notify-email/task.yaml
trigger:
  chain:
    from: run-report
```

Both `notify-slack` and `notify-email` fire when `run-report` completes. Their order is not guaranteed.

---

## Imperative dispatch: `dicode.run_task()`

For cases where the running task itself needs to decide what fires next, use `dicode.run_task()`:

```javascript
// scan-inventory/task.js
const items = await fetchInventory()
const lowStock = items.filter(i => i.qty < i.threshold)

if (lowStock.length > 0) {
  const result = await dicode.run_task("send-reorder-alert", {
    items: lowStock,
    count: lowStock.length
  })
}
```

`dicode.run_task()` is **fire-and-wait** — it blocks until the downstream run completes and resolves with that run's return value. The triggered task receives the passed params as its `input`.

**Declarative vs imperative:**

| | Chain trigger | `dicode.run_task()` |
|---|---|---|
| Who knows the relationship? | The downstream task | The upstream task |
| Dynamic dispatch? | No — declared statically in YAML | Yes — can fire different tasks based on logic |
| Fire-and-wait? | No (parallel with parent's next step) | Yes — blocks until the downstream run finishes |
| Use case | Pipeline steps, post-processing | Conditional dispatch, fan-out from logic |

Both patterns coexist. You can use chain triggers for the main pipeline and `dicode.run_task()` for conditional side effects within tasks.

---

## Data flow

```
TaskA returns { emails: [...], count: 5 }
          ↓
dicode captures return value, JSON-serializes it
          ↓
TaskB starts with input = { emails: [...], count: 5 }
```

The return value is stored in sqlite and injected as the `input` global in the chained task's runtime.

**Constraints:**
- Return value must be JSON-serializable (no functions, no circular refs, no `undefined`)
- Return value is capped at **1MB**. Tasks are not a data pipeline — if you need to pass large data, store it in `kv` and pass the key.
- `input` is `null` for cron and manual triggers

---

## Cycle detection

The trigger engine runs DFS on the success-chain graph at task registration time. Cycles are rejected:

```
fetch-emails → send-digest → archive → fetch-emails   ✗ cycle detected
```

The offending task (the one that closes the cycle) is not registered, and an error is logged.

---

## Run hierarchy

Chained runs are stored with a `parent_run_id` reference:

```
run:abc123   fetch-emails     success
  └─ run:def456   send-slack-digest   success
       └─ run:ghi789   archive-emails   success
```

The WebUI shows the chain as a tree. Drilling into a parent run shows all child runs.

---

## Pipeline DAG

For multi-step workflows with explicit ordering, fan-out, and per-stage overrides, use a `kind: PipelineTask` in `task.yaml` instead of chaining single tasks together. `subtype: sequential` runs stages in order; `subtype: parallel` runs stages concurrently with optional `depends_on` for DAG ordering. Individual stages remain single-purpose `kind: Task` entries; the pipeline orchestrates them.

See [Task Format — Pipelines](./task-format.md#pipelines) for the full field reference and examples.
