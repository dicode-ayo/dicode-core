# Task Store

> **Not implemented.** Everything on this page — `dicode task install`, `dicode task search`, `dicode task commit`, and the official/community task library — is planned design, not shipped behavior. `cmdTask` (`cmd/dicode/main.go`) currently handles only `test | create | edit | save | cancel | delete | approve | pending`. Today, adding a task means putting a folder with `task.yaml` (+ `task.js`/`task.ts`/`task.py`) into a watched source — see [Sources & Reconciler](./sources.md).

The north star is a task store that lets you install pre-built tasks from a GitHub repository (or any public URL) with a single command — the dicode equivalent of `npm install`.

---

## Installing tasks (planned)

```bash
dicode task install github.com/dicode/tasks/morning-email-check
dicode task install github.com/dicode/tasks/morning-email-check --param slack_channel=#devops
```

The intended flow:
1. Download the task folder (`task.yaml` + `task.js` + `task.test.js`)
2. Apply any `--param` overrides to `task.yaml` defaults
3. Write the task into your local tasks directory
4. The local source picks it up via fsnotify — task is live immediately

---

## Publishing tasks (planned)

The design goal: any task folder in a public GitHub repo is installable, with no special registry — just a GitHub URL.

```
github.com/{owner}/{repo}/{path-to-task-folder}
```

Examples:
```bash
dicode task install github.com/acmecorp/dicode-tasks/github-release-notifier
dicode task install github.com/alice/automations/daily-standup-reminder
```

For a task to be installable this way, it would only need `task.yaml` and `task.js` in the folder.

---

## Parameterized tasks (planned)

Store tasks would declare parameters that the installer fills in:

```yaml
# task.yaml (in the store)
name: Slack Daily Digest
params:
  - name: slack_channel
    description: Slack channel to post digest
    # no default — must be provided at install time
  - name: max_items
    default: "10"
```

```bash
# Required param must be provided
dicode task install github.com/dicode/tasks/slack-daily-digest \
  --param slack_channel=#general
```

Optional params (those with defaults) would not need to be specified; the installer would replace the default value with the provided one.

---

## Official task library (aspirational)

The plan is an official task library at `github.com/dicode/tasks`, with categories such as:

| Category | Examples |
|---|---|
| Monitoring | API health checks, uptime monitors, cert expiry alerts |
| Communication | Slack digests, email summaries, Telegram bots |
| Data | Database exports, S3 backups, CSV reports |
| Developer tools | GitHub PR reminders, deploy notifications, CI status |
| Finance | Invoice reminders, expense summaries, budget alerts |
| Productivity | Calendar digests, todo summaries, meeting prep |

None of this exists yet.

---

## Future: searchable registry

The further-out north star is a searchable registry at `dicode.app/store`:

```bash
dicode task search "slack notification"
# → dicode/tasks/slack-digest
# → dicode/tasks/slack-error-alert
# → community/alice/slack-standup

dicode task install dicode/tasks/slack-digest
```

The registry would index public GitHub repos that opt in by adding a `dicode-task` topic. Tasks could have ratings, install counts, and verified publisher badges.

**Revenue sharing (aspirational)**: paid marketplace tasks would share 70% of revenue with the author. See [Business Model](./business-model.md).

---

## Committing installed tasks (planned)

After installing from the store, the task would live in your local source. To version-control it in a git source:

```bash
dicode task commit morning-email-check --to my-git-source
```

See [Sources & Reconciler](./sources.md) for how local and git sources coexist today.
