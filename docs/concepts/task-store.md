# Task Store

> **Partly implemented.** `dicode task search` is shipped (below). `dicode task install`, `dicode task commit`, and the official/community task library are planned design, not shipped behavior. Today, adding a task means putting a folder with `task.yaml` (+ `task.js`/`task.ts`/`task.py`) into a watched source — see [Sources & Reconciler](./sources.md) — or adding the TaskSet you found with `task search` as a source in `dicode.yaml`.

The north star is a task store that lets you install pre-built tasks from a GitHub repository (or any public URL) with a single command — the dicode equivalent of `npm install`.

---

## Searching TaskSets

```bash
dicode task search <query...> [--taskset <git-url-or-path>[@ref]]... [--config PATH] [--json]
```

`task search` reads TaskSets directly and needs no running daemon — it never dials or auto-starts one.

**Matching.** The query is split into words. A task matches when **every** word occurs, case-insensitively, as a substring of its id, `name`, or `description` (each word may hit a different field). `dicode task search slack digest` finds a task named "Slack Digest" and a task with id `slack-notify` whose description mentions a digest. Names and descriptions are the resolved values — taskset `defaults` and entry `overrides` are applied, so they match what the daemon shows.

**Sources.**

- With no `--taskset`, every source in `spec.entries` of `dicode.yaml` is searched (local and git; `--config` names a different file, default `./dicode.yaml`). The config is only read when it is a regular file owned by the calling user. Git sources honour the same `ref.auth.token_env`, `source_security.allowed_token_envs`, and `source_security.allow_internal_hosts` settings the daemon does.
- `--taskset` (repeatable) searches the named TaskSets **instead of** the configured ones — this is how you browse a remote or community TaskSet before adding it. A value with a scheme (`https://`, `http://`, `ssh://`) or in SCP form (`git@host:org/repo.git`) is a git remote; anything else is a local path to a `taskset.yaml` or the directory holding one. A git remote's `taskset.yaml` is read from the repo root. `@ref` selects a branch or tag: the branch is tried first, then a tag of that name; with no `@ref` the `main` branch is used. `--taskset` never uses credentials: it carries none of its own, and every `ref.auth.token_env` declared by the TaskSet's own entries is ignored — a local path included, since it may be an untrusted checkout — so a TaskSet you are only browsing cannot have your environment's tokens sent to a host it names. It is subject to the default SSRF guard (internal hosts are refused).

Git TaskSets are fetched with go-git into a temporary directory that is removed when the command exits; your sources are never written. (Loading `dicode.yaml` for the default sources creates `<data_dir>/ai-tasks/` if it is missing, exactly as the daemon's config load does; `--taskset` alone does not read the config.) Credentials in a URL's userinfo are stripped from every URL and error the command prints.

**Output.** Columns, with a header row, sorted by id:

```
ID                                       NAME                           DESCRIPTION
community/slack-digest                   Slack Daily Digest             Posts a digest of the day's messages to a channel.
```

`ID` is `<taskset>/<task-id>`: for a configured source, the `dicode.yaml` entry key (the id the daemon registers); for `--taskset`, the last element of the URL or path (minus `.git`, or the parent directory for a `taskset.yaml` path); when two `--taskset` values share that name, later ones get `-2`, `-3`, … in flag order. Descriptions are cut to one line of 60 characters. When nothing matches, stdout is empty and `no tasks match "<query>"` is written to stderr.

`--json` prints a JSON array (`[]` when nothing matches). Every object carries the same keys, empty strings when unknown:

| Key | Value |
|---|---|
| `source` | the TaskSet label used as the id prefix |
| `id` | the namespaced task id |
| `name` | resolved `name` |
| `description` | resolved `description` (full text) |
| `location` | the TaskSet's git URL (credentials stripped) or absolute local path |
| `ref` | the branch or tag searched (git only) |
| `commit` | the HEAD commit of the TaskSet's own repo (git only) — for a task pulled in from another repo by a nested ref, this is still the root TaskSet's commit |

**Exit status.** `0` on success, including when nothing matches. Non-zero on bad usage (no query, unknown flag, missing flag value), a missing or untrusted config, or when any TaskSet could not be fetched or parsed — results from the TaskSets that did resolve are still printed. Per-entry load failures inside a TaskSet are warnings on stderr and do not change the exit status.

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

`dicode task search` today searches TaskSets you name or have configured. The further-out north star is a hosted registry at `dicode.app/store` that it could also query, so a search spans repos you have not heard of yet:

```bash
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
