# buildin/telegram — notification delivery over the Telegram Bot API

Sends one message to a Telegram chat via
`POST https://api.telegram.org/bot<token>/sendMessage`. Manual trigger — it is
fired by dicode's notification hooks, or by hand with
`dicode run buildin/telegram title=... body=...`.

Unlike `buildin/notifications`, which needs a desktop session and a running
notification daemon, this reaches you from a headless server.

## The three sources it serves

| Source | Config key | How the data arrives |
|---|---|---|
| A run suspended, or a suspended conversation ended | `ai.notify_task` | params: `title`, `body`, `priority`, `event`, `run_id`, `task_id`, `resume_url`, `status` (on `ended`) |
| A task went pending approval | `approval.notify_task` | params: `title`, `body`, `priority`, `event`, `task_id`, `hash`, `approve_url` |
| A run failed | `defaults.on_failure_chain` | chain **input**: `taskID`, `runID`, `status`, `output` |

Params are read first, then `input` — the failure chain fires through
`fireAsync` with an `Input:` map, not params, and stamps its task/run keys in
camelCase.

Both hooks send rendered `title`/`body`; the failure chain sends neither, so
the task composes its own headline and text from whatever fields are present.
A notification with no recognized field at all still sends. No field is
required, and none is validated at fire time.

## Setup

1. Talk to [@BotFather](https://t.me/BotFather), `/newbot`, keep the token.
2. `dicode secrets set TELEGRAM_BOT_TOKEN <token>`
3. Get the chat ID: message the bot (or add it to the group/channel as an
   admin), then read `chat.id` from
   `https://api.telegram.org/bot<token>/getUpdates`. Group and channel IDs are
   negative, e.g. `-1001234567890`.
4. `dicode secrets set TELEGRAM_CHAT_ID <id>` — or pass `chat_id=<id>` per run;
   the param wins when it is non-empty.

## Wiring (opt-in)

Nothing points here by default; `ai.notify_task` ships as
`buildin/notifications`. In `dicode.yaml`:

```yaml
ai:
  notify_task: buildin/telegram
approval:
  notify_task: buildin/telegram
defaults:
  on_failure_chain: buildin/telegram
```

Each is independent — wire one, two, or all three.

## Formatting

Messages use Telegram's **HTML** parse mode, which reserves exactly three
characters (`&`, `<`, `>`). MarkdownV2 reserves about eighteen, `-` among them,
so a task ID like `buildin/ai-agent-claude-cli` would fail the send with HTTP
400 exactly when the notification matters most.

Text is capped at Telegram's 4096-character limit by dropping whole trailing
lines, so truncation can never split a tag. `priority: min` and `priority: low`
send with `disable_notification` (no sound).

`resume_url` and `approve_url` are included verbatim. They are built from
`WebUIBaseURL()`, which reads `localhost:<port>` — over `https` only when the
daemon terminates TLS itself — unless `server.public_url` is set. Set it, or
the link that arrives on your phone resolves to your phone. `approve_url` then works as-is, since its single-use
token is the credential; `resume_url` reaches the dashboard and still needs a
login at the far end.

A `Logs:` line linking to the failed run is added when the caller supplies
`base_url`; it is built to the same form and carries the same caveat, so point
it at whatever `server.public_url` holds. The task cannot derive the address
itself, which is why it arrives as a field — for the failure chain, set it under
`defaults.on_failure_chain.params`. The line is omitted when no run id arrived,
or when it would merely repeat `resume_url`.

The API answers `200` with `{"ok": false, "description": "..."}` for several
errors, so the response body — not the status — decides. A rejected send throws
with the `description`, failing the run visibly instead of dropping the
notification. Error messages never carry the URL, since the URL carries the bot
token.

## Retries

Up to three attempts, backing off 500ms then 1s, each attempt bounded by its
own 8s timeout so one hung connection cannot consume the whole task budget. A
`429` honors the `retry_after` the API returns — and gives up rather than
retrying early when that hint is longer than the budget can absorb, since
retrying sooner than Telegram asked only spends an attempt on a certain second
`429`. Anything 5xx or a connection-level failure is retried on the curve. Every other `4xx` is a rejected message — a bad
`chat_id`, malformed HTML — and fails immediately, since resending reproduces
the rejection.

A send that exhausts its attempts fails the run. Note that the daemon logs a
failed notify at Debug (so a headless host does not spam its log every
suspend), so a persistently broken delivery is quiet: the run record is the
place it shows up.

## Out of scope

Outbound only. No long-polling daemon, no inline keyboards, no
approve-or-resume from inside Telegram — acting on a notification means
following the link into the WebUI.

## Tests

```
make test-tasks
# or just this task:
deno test --allow-all --config=tasks/deno.json tasks/buildin/telegram/task.test.ts
```
