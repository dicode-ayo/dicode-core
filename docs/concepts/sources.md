# Sources & Reconciler

Dicode watches one or more **sources** for task files and reconciles them automatically. Add a file, the task is live. Delete a file, it stops. No restart needed.

---

## Source types

### TaskSet source (recommended)

A **TaskSet source** uses a `taskset.yaml` file as its entry point. Tasks are composed hierarchically — a TaskSet can reference other TaskSets, allowing large task trees to be built from smaller ones (like ArgoCD App-of-Apps).

```yaml
# dicode.yaml
sources:
  - name: infra
    path: /home/user/tasks/taskset.yaml
    type: local
```

**`taskset.yaml`** — the root entry point:

```yaml
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  defaults:
    timeout: 30m
  entries:
    deploy-backend:
      ref:
        path: ./backend/task.yaml
      overrides:
        timeout: 5m
    platform:
      ref:
        path: ./platform/taskset.yaml   # nested TaskSet — namespace: infra/platform
```

Each task file must declare its kind:

```yaml
apiVersion: dicode/v1
kind: Task
name: Deploy Backend
trigger:
  manual: true
```

**Namespace-scoped task IDs** — task IDs are built from the path of TaskSet names:
- Root TaskSet `infra` + entry `deploy-backend` → ID `infra/deploy-backend`
- Nested TaskSet `infra` > `platform` + entry `nginx-start` → ID `infra/platform/nginx-start`

**3-level precedence stack** (lowest → highest):

1. `task.yaml` base values
2. Root TaskSet `spec.defaults`
3. Per-entry `overrides` (parent entry patch merged with local entry overrides; leaf wins)

> **Deprecated:** `kind:Config spec.defaults` and `overrides.defaults` from parent TaskSets are no longer applied to the override stack. Migrate shared defaults to `dicode.yaml defaults:` instead.

**`kind:Config`** — optional shared defaults file:

```yaml
apiVersion: dicode/v1
kind: Config
metadata:
  name: infra-config
spec:
  defaults:
    timeout: 1h
    runtime: deno
```

**Dev mode** — swap the TaskSet root to a local path for live development without editing `dicode.yaml`:

```bash
# via REST API
curl -X PATCH http://localhost:8080/api/sources/infra/dev \
  -H 'Content-Type: application/json' \
  -d '{"enabled": true, "local_path": "/tmp/my-dev-tasks/taskset.yaml"}'
```

Or toggle in the web UI: **Sources** page → enable dev mode + enter local path.

Disabling dev mode immediately reverts to the original root ref.

---

### Git source

Watches a git repository. Tasks are committed into the repo — dicode polls or receives a push webhook, pulls changes, and updates the running task set accordingly.

```yaml
sources:
  - type: git
    id: team-tasks
    url: https://github.com/acme/tasks
    branch: main
    poll_interval: 60s
    auth:
      type: token
      token_env: GITHUB_TOKEN
```

**Options:**

| Field | Default | Description |
|---|---|---|
| `id` | (derived from URL) | Unique source identifier |
| `url` | required | Repository HTTPS or SSH URL |
| `branch` | `main` | Branch to track |
| `poll_interval` | `30s` | How often to check for changes |
| `auth.type` | `none` | `token`, `ssh`, `none` |
| `auth.token_env` | | Env var name containing the token |
| `tags` | | Task tag filter (future: source selectors) |

**How it works:**
1. On startup: clone repo to `~/.dicode/repos/{source-id}/`
2. Periodic poll: `git fetch` + diff to identify changed task folders
3. Push webhook (optional): HTTP endpoint at `/hooks/git/{source-id}` triggers immediate sync
4. Tasks in subdirectories deeper than one level are ignored

**Auth:** token auth sends the token as the HTTP password (standard GitHub/GitLab personal access token flow). SSH key auth (future) will use a configured key file.

---

### Local source

Watches a local directory using `fsnotify`. File saves trigger near-instant reload (~100ms). No git required.

```yaml
sources:
  - type: local
    id: dev
    path: ~/dicode-tasks
    watch: true
```

**Options:**

| Field | Default | Description |
|---|---|---|
| `id` | (derived from path) | Unique source identifier |
| `path` | required | Absolute or `~`-prefixed path to tasks directory |
| `watch` | `true` | Enable fsnotify live reload |

**How it works:**
1. On startup: scan directory, emit Added events for all valid task folders
2. If `watch: true`: start fsnotify watcher on the directory
3. On file change: debounce 100ms, re-scan the affected task folder, emit Updated event
4. On folder delete: emit Removed event

**Debouncing:** writes are debounced by 100ms to avoid partial-file events when editors do atomic saves (write to temp file → rename).

---

## Multiple sources

You can configure multiple sources simultaneously. A common pattern is a git source for stable/shared tasks and a local source for active development:

```yaml
sources:
  - type: git
    id: shared
    url: https://github.com/acme/tasks
    branch: main
  - type: local
    id: dev
    path: ~/tasks-dev
    watch: true
```

Both sources contribute tasks to the same registry. Task IDs must be unique across all sources. If two sources declare a task with the same folder name, the conflict is logged as an error and the second task is skipped.

---

## Reconciler

The reconciler is the component that consumes events from all sources and keeps the task registry in sync.

**Event types:**

| Kind | Trigger | Registry action |
|---|---|---|
| `added` | New task folder detected | Register task (load spec, add to in-memory map, schedule triggers) |
| `updated` | Existing task changed | Re-register task (reload spec, reschedule) |
| `removed` | Task folder deleted | Unregister task (cancel triggers, remove from map) |

**Fan-in:** the reconciler fans in channels from all sources using a single goroutine. Events are processed sequentially to avoid registry races.

**Error handling:**
- If a task's `task.yaml` fails validation on `added` or `updated`, the error is logged and the task is not registered (or the old version is kept for `updated`)
- Source errors (git clone failure, auth failure) are logged and retried on the next poll cycle. The reconciler does not crash.

---

## Task ownership

Each task belongs to exactly one source. When a task is registered, the source ID is recorded. This matters for `dicode task commit` — it knows which source to commit to.

If you move a task from a local source to a git source using `dicode task commit`, the local source emits a Removed event (file deleted) and the git source eventually emits an Added event (after push + pull). The task briefly disappears from the registry during this transition.

---

## Source selector tags (future)

Future feature: each task can declare tags in `task.yaml`, and each source can filter which tags it loads. This lets one dicode instance load production tasks from a prod source and dev tasks from a dev source without collision.

```yaml
# task.yaml
tags:
  - env:prod

# dicode.yaml
sources:
  - type: git
    url: https://github.com/acme/tasks
    tags: [env:prod]     # only load tasks tagged env:prod
```
