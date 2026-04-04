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

**Implementation:** `robfig/cron` v3. The cron scheduler is managed by the trigger engine — when a task is added or updated, the engine cancels any existing cron registration and creates a new one. When a task is removed, the registration is cancelled.

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
log.info(`Received push to ${input.ref}`)
```

**Webhook authentication:** dicode supports a shared secret for webhook verification. Set `server.webhook_secret` in `dicode.yaml` and include it as:
- `X-Dicode-Secret: <secret>` header, or
- `?secret=<secret>` query parameter

Requests with an invalid or missing secret are rejected with 401.

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
dicode task run morning-email-check
dicode task run morning-email-check --param slack_channel=#ops
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
log.info(`Sending digest of ${input.count} emails`)
```

`archive-emails`:
```yaml
trigger:
  chain:
    from: send-slack-digest
    on: always   # archive even if digest send fails
```

**Cycle detection:** the reconciler runs DFS on the chain graph at task registration time. Cycles are rejected with an error.

**Chain vs `dicode.trigger()`:** chain is **declarative** — `fetch-emails` has no knowledge of `send-slack-digest`. `dicode.trigger()` is **imperative** — the running task explicitly fires another. See [Task → Orchestrator API](./orchestrator-api.md).

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

Set `DICODE_MAX_CONCURRENT_TASKS` to cap how many task goroutines run in parallel:

```bash
DICODE_MAX_CONCURRENT_TASKS=8 dicoded
```

- `0` (default) — unlimited, backwards-compatible behaviour.
- `N > 0` — at most N tasks execute concurrently. Additional invocations queue inside the daemon and run as slots become free.
- **Shutdown safety:** queued goroutines are unblocked when the semaphore releases or the daemon shuts down, so a full slot queue never causes a hang on `SIGTERM`.

---

## Trigger constraints

- Exactly one trigger per task. Multiple triggers are not supported (use `dicode.trigger()` from a task for complex dispatch logic).
- All five trigger types coexist in the same task registry.
- Cron, chain, and daemon tasks can also be triggered manually via the API/UI (manual trigger on a daemon task restarts it).
