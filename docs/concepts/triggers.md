# Triggers

Every task has exactly one trigger. The trigger determines when the task runs.

---

## Cron

Runs on a fixed schedule.

```yaml
trigger:
  cron: "0 8 * * 1-5"   # weekdays at 8am
```

Uses standard 5-field cron syntax:
```
┌─ minute (0–59)
│ ┌─ hour (0–23)
│ │ ┌─ day of month (1–31)
│ │ │ ┌─ month (1–12)
│ │ │ │ ┌─ day of week (0–6, Sun=0)
│ │ │ │ │
* * * * *
```

**Common expressions:**

| Expression | Meaning |
|---|---|
| `* * * * *` | Every minute |
| `*/15 * * * *` | Every 15 minutes |
| `0 * * * *` | Every hour |
| `0 8 * * *` | Daily at 8am |
| `0 8 * * 1-5` | Weekdays at 8am |
| `0 0 * * 0` | Every Sunday at midnight |
| `0 0 1 * *` | First day of every month |

Schedule is evaluated in the dicode process's local timezone. Set the `TZ` environment variable to control the timezone on headless/Docker deployments.

**Implementation:** `robfig/cron` v3. The cron scheduler is managed by the trigger engine. A task is re-registered on every reconciler reload (e.g. an unrelated file change elsewhere in the same source, or any edit to the task itself) and at daemon startup — this can happen far more often than the schedule actually changes. When the incoming cron expression is byte-identical to what's already armed, the engine leaves the existing registration alone rather than tearing it down and re-adding it; only a genuine schedule change (or the task being removed/disabled) cancels the old registration and arms a new one. This avoids a tick landing in the gap between cancel and re-add and being silently dropped, and it means a no-op reload never resets the task's persisted `next_run_at` (see Missed-run catchup below).

**Missed-run catchup:** dicode persists each cron task's next scheduled time in the database (`cron_jobs` table). On startup, any task whose recorded `next_run_at` is in the past (but within the last 24 hours) is fired immediately with `trigger_source = "cron-catchup"`. This prevents silent skips when dicode restarts mid-schedule (e.g. after an OS reboot or deploy).

**Fire-once semantics:** at most one catchup run is fired per task per restart, regardless of how many intervals were missed. For example, if a task runs every 5 minutes and the daemon was offline for 2 hours, one catchup run fires — not 24. This avoids bulk-firing high-frequency tasks after long outages. Runs missed more than 24 hours ago are skipped with a `Warn` log. Tasks deleted between sessions are pruned from the `cron_jobs` table on startup.

---

## Webhook

Fires when an HTTP POST is received at a configured path.

```yaml
trigger:
  webhook: /github-push
```

This task is triggered by a POST to `http://localhost:8080/hooks/github-push` (or via the relay URL if configured).

The request body is parsed and available as the `input` global in `task.js`:
```javascript
console.log(`Received push to ${input.ref}`)
```

**Webhook authentication:** dicode supports a shared secret for webhook verification. Set `server.webhook_secret` in `dicode.yaml` and include it as:
- `X-Dicode-Secret: <secret>` header, or
- `?secret=<secret>` query parameter

Requests with an invalid or missing secret are rejected with 401.

**Replay protection:** when `webhook_secret` is set, dicode rejects a duplicate request within a 1-hour window (HTTP 409). The cache keys on the HMAC digest, which folds in `X-Dicode-Timestamp` when the sender includes it — so two legitimate requests with an identical body but distinct timestamps never collide. Options per task:
- `replay_protection: false` — allow duplicate requests (e.g. idempotent senders)
- `require_timestamp: true` — reject any request missing `X-Dicode-Timestamp`, closing the replay window for timestamp-less signers (recommended for relay-exposed webhooks). Defaults to `false` for GitHub-style body-only signers.
- Default: `replay_protection: true` whenever a secret is configured.

See [Webhooks](../webhooks.md) for the full HMAC signing and timestamp-binding details.

**Path rules:**
- Must start with `/`
- Alphanumeric characters, hyphens, underscores, and forward slashes only
- No two tasks can share the same webhook path

**Relay:** for webhook tasks to be reachable from the internet on a laptop, configure the webhook relay. See [Webhook Relay](./webhook-relay.md).

---

## Manual

Task only runs when explicitly triggered via the WebUI or REST API. Use this for tasks you want to control completely — no automatic firing.

```yaml
trigger:
  manual: true
```

**Trigger from UI:** open the task in the WebUI, click "Run". You can fill in parameter overrides before triggering.

**Trigger from CLI:**
```bash
dicode run morning-email-check slack_channel=#ops
```

**Trigger from API:**
```bash
curl -X POST http://localhost:8080/api/tasks/morning-email-check/run \
  -H "Content-Type: application/json" \
  -d '{"params": {"slack_channel": "#ops"}}'
```

---

## Chain

Fires when another task completes. The completing task's return value is available as the `input` global.

```yaml
trigger:
  chain:
    from: fetch-emails
    on: success    # success | failure | always
```

| `on` value | Fires when |
|---|---|
| `success` (default) | Preceding task completed without error |
| `failure` | Preceding task threw an uncaught exception |
| `always` | Preceding task completed, regardless of outcome |

**Example pipeline:**
```
fetch-emails → send-slack-digest → archive-emails
```

`fetch-emails` returns `{ emails: [...], count: 5 }`.

`send-slack-digest`:
```yaml
trigger:
  chain:
    from: fetch-emails
    on: success
```
```javascript
console.log(`Sending digest of ${input.count} emails`)
```

`archive-emails`:
```yaml
trigger:
  chain:
    from: send-slack-digest
    on: always   # archive even if digest send fails
```

**Cycle detection:** the trigger engine runs DFS on the success-chain graph at task registration time. Cycles are rejected with an error.

**Chain vs `dicode.run_task()`:** chain is **declarative** — `fetch-emails` has no knowledge of `send-slack-digest`. `dicode.run_task()` is **imperative** — the running task explicitly fires another and waits for its result. See [Task → Orchestrator API](./orchestrator-api.md).

For full chain documentation including data flow and constraints, see [Task Chaining](./task-chaining.md).

---

## Daemon ✅

Long-running tasks that start with dicode and run indefinitely.

```yaml
trigger:
  daemon: true
  restart: always   # always (default) | on-failure | never
```

Daemon tasks:
- Start automatically when dicode starts (or when the task is first registered)
- Are restarted according to the `restart` policy when they exit
- Receive a kill signal (context cancellation for JS, SIGTERM for Docker) when dicode shuts down
- Appear in the task list with a run record like any other task
- Explicitly killed tasks (status `cancelled`) are **never** restarted regardless of policy

**Restart policies:**

| Policy | Behavior |
|---|---|
| `always` (default) | Restart on any exit — success or failure |
| `on-failure` | Only restart on non-zero exit / uncaught exception |
| `never` | Start once; do not restart |

A 2-second back-off is applied between restarts to prevent tight loops on immediately-failing tasks.

**Stale run cleanup:** if dicode crashes without a clean shutdown, any `running` runs from the previous session are marked `cancelled` on the next startup. Daemon tasks start fresh.

**Orphan container cleanup:** for Docker daemon tasks, any containers from a previous session (identified by `dicode.run-id` label) are stopped and removed on startup.

**Use cases:** Docker services (nginx, postgres, custom APIs), persistent background workers, Slack socket-mode bots, custom API gateways.

**JS + `server` global (north star):** the `server` global that lets JS daemon tasks serve HTTP is not yet implemented. Docker daemon tasks work fully today — use `runtime: docker` for HTTP-serving daemons.

---

## Concurrency limit

By default dicode spawns a goroutine for every task invocation with no upper bound. Under sustained load this can cause goroutine storms and amplified SQLite write contention.

Cap how many task goroutines run in parallel via config:

```yaml
execution:
  max_concurrent_tasks: 8
```

…or via env var (overrides the config value):

```bash
DICODE_MAX_CONCURRENT_TASKS=8 dicode daemon
```

- `0` (default) — unlimited, backwards-compatible behaviour.
- `N > 0` — at most N tasks execute concurrently. Additional invocations queue inside the daemon and run as slots become free.
- **Daemon tasks bypass the cap** so long-running daemons don't starve webhook/cron tasks.
- **Synchronous webhook responses bypass the cap** — tasks fired via `fireSync` (webhooks that return a response to the HTTP caller) are not subject to the semaphore, so sync clients can never deadlock waiting for a slot. Only async triggers (cron, async webhooks, chained `dicode.run_task()` calls) count against the limit.
- **Killing a queued run** — cancelling a run that is still waiting on a slot honors the kill immediately; the run is finalized as `cancelled` and the websocket `run:finished` event fires so the UI stays in sync.
- **Shutdown safety:** queued goroutines are unblocked when the daemon shuts down, finalized as `cancelled`, and their DB rows updated under a bounded timeout, so a full slot queue never causes a hang on `SIGTERM`.

Runtime visibility of the cap is exposed via [GET /api/metrics](metrics.md) —
the `tasks` object includes `max_concurrent_tasks`, `active_task_slots`, and
`waiting_tasks`.

---

## Trigger constraints

- Exactly one trigger per task. Multiple triggers are not supported (use `dicode.run_task()` from a task for complex dispatch logic).
- All five trigger types coexist in the same task registry.
- Cron, chain, and daemon tasks can also be triggered manually via the API/UI (manual trigger on a daemon task restarts it).
