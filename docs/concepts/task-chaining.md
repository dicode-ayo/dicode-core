# Task Chaining

Tasks can be chained so that the output of one task becomes the input of another. Dicode supports two chaining styles: **declarative** (chain trigger) and **imperative** (`dicode.trigger()`).

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
log.info(`Sending digest of ${input.count} emails`)
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

## Imperative dispatch: `dicode.trigger()`

For cases where the running task itself needs to decide what fires next, use `dicode.trigger()`:

```javascript
// scan-inventory/task.js
const items = await fetchInventory()
const lowStock = items.filter(i => i.qty < i.threshold)

if (lowStock.length > 0) {
  await dicode.trigger("send-reorder-alert", {
    items: lowStock,
    count: lowStock.length
  })
}
```

`dicode.trigger()` is **fire-and-forget** — it returns once the triggered run has been scheduled (not after it completes). The triggered task receives the passed payload as its `input`.

**Declarative vs imperative:**

| | Chain trigger | `dicode.trigger()` |
|---|---|---|
| Who knows the relationship? | The downstream task | The upstream task |
| Dynamic dispatch? | No — declared statically in YAML | Yes — can fire different tasks based on logic |
| Fire-and-wait? | No (parallel with parent's next step) | No (fire-and-forget) |
| Use case | Pipeline steps, post-processing | Conditional dispatch, fan-out from logic |

Both patterns coexist. You can use chain triggers for the main pipeline and `dicode.trigger()` for conditional side effects within tasks.

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

The reconciler runs DFS on the chain graph every time a task is added or updated. Cycles are rejected:

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

## Pipeline DAG (north star)

For complex multi-step workflows with fan-out, fan-in, and conditionals, a future `pipeline.yaml` format will define an explicit DAG. Individual tasks remain single-purpose; the pipeline orchestrates them.

```yaml
# pipelines/morning-routine/pipeline.yaml
name: morning-routine
trigger:
  cron: "0 9 * * *"
steps:
  - id: fetch
    task: fetch-emails
  - id: digest
    task: send-slack-digest
    input: $steps.fetch.output
  - id: archive
    task: archive-emails
    input: $steps.fetch.output   # fan-out: both digest and archive get fetch's output
```

This is not implemented yet. Linear declarative chains plus `dicode.trigger()` are sufficient for the MVP.
