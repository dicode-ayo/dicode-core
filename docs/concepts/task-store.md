# Task Store

The task store lets you install pre-built tasks from a GitHub repository (or any public URL) with a single command. It's the dicode equivalent of `npm install`.

---

## Installing tasks

```bash
dicode task install github.com/dicode/tasks/morning-email-check
dicode task install github.com/dicode/tasks/morning-email-check --param slack_channel=#devops
```

This:
1. Downloads the task folder (`task.yaml` + `task.js` + `task.test.js`)
2. Applies any `--param` overrides to `task.yaml` defaults
3. Writes the task into your local tasks directory
4. The local source picks it up via fsnotify — task is live immediately

---

## Publishing tasks

Any task folder in a public GitHub repo is installable. There's no special registry — just a GitHub URL.

```
github.com/{owner}/{repo}/{path-to-task-folder}
```

Examples:
```bash
dicode task install github.com/acmecorp/dicode-tasks/github-release-notifier
dicode task install github.com/alice/automations/daily-standup-reminder
```

For the task to be installable, it only needs `task.yaml` and `task.js` in the folder.

---

## Parameterized tasks

Store tasks can declare parameters that the installer fills in:

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

Optional params (those with defaults) don't need to be specified. The installer replaces the default value with the provided one.

---

## Official task library

Dicode maintains an official task library at `github.com/dicode/tasks`. Categories include:

| Category | Examples |
|---|---|
| Monitoring | API health checks, uptime monitors, cert expiry alerts |
| Communication | Slack digests, email summaries, Telegram bots |
| Data | Database exports, S3 backups, CSV reports |
| Developer tools | GitHub PR reminders, deploy notifications, CI status |
| Finance | Invoice reminders, expense summaries, budget alerts |
| Productivity | Calendar digests, todo summaries, meeting prep |

Community contributions welcome via pull request.

---

## Future: searchable registry

The north star is a searchable registry at `dicode.app/store`:

```bash
dicode task search "slack notification"
# → dicode/tasks/slack-digest
# → dicode/tasks/slack-error-alert
# → community/alice/slack-standup

dicode task install dicode/tasks/slack-digest
```

The registry indexes public GitHub repos that opt in by adding a `dicode-task` topic. Tasks can have ratings, install counts, and verified publisher badges.

**Revenue sharing**: paid marketplace tasks share 70% of revenue with the author. See [Business Model](./business-model.md).

---

## Committing installed tasks

After installing from the store, the task lives in your local source. If you want it version-controlled in your git source:

```bash
dicode task commit morning-email-check --to my-git-source
```

See [Sources & Reconciler](./sources.md) for how local and git sources coexist.
