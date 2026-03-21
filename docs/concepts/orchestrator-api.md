# Task → Orchestrator API

The `dicode` global is a two-way communication channel between a running task script and the dicode orchestrator. It lets tasks report progress, trigger other tasks, query orchestrator state, and request human approval.

---

## `dicode.progress()`

Emit an intermediate progress update. Streamed live to the WebUI run log and visible in `dicode task run --verbose`.

```javascript
dicode.progress("Processing emails", { done: 0, total: 100 })

for (let i = 0; i < emails.length; i++) {
  await processEmail(emails[i])
  dicode.progress(`Processed ${i + 1} of ${emails.length}`, {
    done: i + 1,
    total: emails.length,
    percent: Math.round(((i + 1) / emails.length) * 100)
  })
}
```

**Parameters:**
- `message` (string) — human-readable progress description
- `data` (object, optional) — structured data attached to the progress event

Progress events appear in the run log with a `progress` level. The WebUI can render the `percent` field as a progress bar.

`dicode.progress()` is synchronous (fire-and-forget, no await needed).

---

## `dicode.trigger()`

Imperatively dispatch another task. Fire-and-forget — returns once the run has been **scheduled**, not after it completes.

```javascript
// Fire-and-forget
await dicode.trigger("send-alert", {
  reason: "spike",
  value: 99.7
})

// The triggered task receives the payload as its `input` global
```

**Parameters:**
- `taskID` (string) — the ID of the task to trigger
- `payload` (object, optional) — passed as `input` to the triggered task

**Difference from chain triggers:**
- Chain trigger: TaskB *declares* it follows TaskA. TaskA is unaware.
- `dicode.trigger()`: TaskA *imperatively* fires TaskB. TaskA controls the dispatch.

Use `dicode.trigger()` when the decision of what to fire next depends on runtime logic. Use chain triggers when the relationship is fixed and declarative.

---

## `dicode.isRunning()`

Query whether another task is currently executing. Useful for skipping a run if a dependency is busy.

```javascript
const backupRunning = await dicode.isRunning("database-backup")
if (backupRunning) {
  log.warn("Backup already running, skipping report generation")
  return
}

await generateReport()
```

**Parameters:**
- `taskID` (string) — the task ID to check

**Returns:** `boolean` — `true` if the task has at least one run in `running` state.

---

## `dicode.ask()` (north star)

Suspend the current run and send an actionable notification requesting human approval. The run resumes when the user responds or the timeout expires.

```javascript
const decision = await dicode.ask("Deploy to 500 production users?", {
  timeout: "30m",
  options: ["approve", "reject"]
})

if (decision === "approve") {
  await deploy()
  log.info("Deployment complete")
} else {
  log.info(`Deployment cancelled (response: ${decision})`)
}
```

**Parameters:**
- `question` (string) — the question shown in the notification
- `options.timeout` (string, default `"1h"`) — how long to wait before resuming with `null`
- `options.options` (array, default `["approve", "reject"]`) — button labels

**Returns:** the string the user selected, or `null` if the timeout expired.

**How it works:**
1. Run state is saved to sqlite as `suspended`
2. A notification is sent via the configured provider with action buttons
3. The goroutine running the task blocks on a channel
4. User taps a button (via notification or WebUI)
5. The response is written to the channel
6. Task execution resumes from the point of `await dicode.ask()`

**Timeout:** if no response is received within `timeout`, the run resumes with `return null`. The task should handle `null` as a cancellation.

This feature is the north star for human-in-the-loop AI workflows. Requires the suspension mechanism in the runtime (not yet implemented — see [Implementation Plan](../implementation-plan.md)).

---

## Summary

| Method | Sync/Async | Blocks? | Description |
|---|---|---|---|
| `dicode.progress(msg, data)` | Sync | No | Emit progress update |
| `await dicode.trigger(id, payload)` | Async | No (fire-and-forget) | Schedule another task |
| `await dicode.isRunning(id)` | Async | No | Check if task is running |
| `await dicode.ask(question, opts)` | Async | Yes — until response | Request human approval |
