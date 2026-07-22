# Sources & Reconciler

Dicode watches one or more **sources** for task files and reconciles them automatically. Add a file, the task is live. Delete a file, it stops. No restart needed.

---

## dicode.yaml as a root TaskSet

`dicode.yaml` is now treated as a **root TaskSet**. Its `spec.entries` block declares every source the daemon loads. Each entry key is the namespace for the tasks it contains; the `ref` block points at a `taskset.yaml` file (local or git).

This is a pure structural unification: the same `parent.overrides.entries` mechanism that operators use inside a `taskset.yaml` now works at the top level too, so you can disable or patch buildin tasks directly in `dicode.yaml` without forking the taskset.

```yaml
# dicode.yaml
spec:
  entries:
    buildin:
      ref:
        path: ${CONFIGDIR}/tasks/buildin/taskset.yaml
      overrides:
        entries:
          relay-client:
            enabled: false       # disable one buildin task without forking the set
    examples:
      ref:
        url: https://github.com/dicode-ayo/dicode-core
        branch: main
        path: tasks/examples/taskset.yaml
        poll_interval: 5m
        auth:
          token_env: GITHUB_TOKEN
```

### Field reference for `spec.entries.<name>.ref`

| Field | Default | Description |
|---|---|---|
| `path` | required (local) | Absolute path to `taskset.yaml`; `${CONFIGDIR}` and `${HOME}` expanded |
| `url` | required (git) | HTTPS or SSH git URL |
| `branch` | `main` | Branch to track (git only) |
| `poll_interval` | `30s` | How often to fetch (git only) |
| `auth.token_env` | | Env var holding a personal access token |
| `auth.ssh_key` | | Path to an SSH private key |
| `watch` | `true` | Enable fsnotify live reload (local refs) |
| `dev_ref` | | Substitute ref when dev mode is active |

---

## Source types

### TaskSet source (recommended)

A **TaskSet source** uses a `taskset.yaml` file as its entry point. Tasks are composed hierarchically — a TaskSet can reference other TaskSets, allowing large task trees to be built from smaller ones (like ArgoCD App-of-Apps).

**`taskset.yaml`** — the entry point for one source:

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
- Root entry `buildin` + inner entry `relay-client` → ID `buildin/relay-client`
- Nested entry `buildin` > `platform` + inner entry `nginx-start` → ID `buildin/platform/nginx-start`

**3-level precedence stack** (lowest → highest):

1. `task.yaml` base values
2. TaskSet `spec.defaults`
3. Per-entry `overrides` (parent entry patch merged with local entry overrides; leaf wins)

> **Deprecated:** `kind:Config spec.defaults` and `overrides.defaults` from parent TaskSets are no longer applied to the override stack. Migrate shared defaults to `dicode.yaml defaults:` instead.

**Disabling an entry** — set `enabled: false` to disable a task without deleting its definition. Disabled tasks remain visible in the API (with `enabled: false`) and the registry, but are not scheduled (no cron), not spawned (no daemon), and not routed (no webhook).

```yaml
spec:
  entries:
    relay-client:
      enabled: false        # one-liner shortcut; default is true when omitted
      ref:
        path: ./relay-client/task.yaml
```

The longer nested form (`overrides.enabled: false`) is equivalent and still supported; setting both is a parse error.

A parent TaskSet (or `dicode.yaml`) can also flip an entry's enabled state via its own `overrides.entries.<key>.enabled`. This lets a higher-level operator disable a buildin task without forking the taskset. Parent-level override wins over child-level.

**Dev mode** — swap the TaskSet root to a local path for live development without editing `dicode.yaml`:

```bash
# via REST API
curl -X PATCH http://localhost:8080/api/sources/buildin/dev \
  -H 'Content-Type: application/json' \
  -d '{"enabled": true, "local_path": "/tmp/my-dev-tasks/taskset.yaml"}'
```

Or toggle in the web UI: **Sources** page → enable dev mode + enter local path.

Disabling dev mode immediately reverts to the original root ref.

---

## Multiple sources

Configure multiple sources by adding entries to `spec.entries`. Each entry key is the namespace:

```yaml
spec:
  entries:
    shared:
      ref:
        url: https://github.com/acme/tasks
        branch: main
    dev:
      ref:
        path: ~/tasks-dev
        watch: true
```

Both sources contribute tasks to the same registry. Task IDs must be unique across all sources.

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

Internally, each source's identity (used by the reconciler to track and tear down its watch/poll loop) is derived from **both** its `spec.entries` name and its `ref` (git URL or local path) — not the ref alone. Two entries that happen to reference the identical `taskset.yaml` path or git URL (e.g. a source added dynamically via the web UI's "Add source" form pointed at a path another entry already watches) still get distinct internal IDs, so removing one never disturbs the other's reconciler bookkeeping.

---

## Migration from old `sources:` array

The old `sources:` array was removed in v0.1+. The format change is mechanical:

**Before:**
```yaml
sources:
  - name: buildin
    type: local
    path: ${CONFIGDIR}/tasks/buildin/taskset.yaml
    watch: true
  - name: examples
    type: git
    url: https://github.com/dicode-ayo/dicode-core
    branch: main
    entry_path: tasks/examples/taskset.yaml
    poll_interval: 5m
    auth:
      type: token
      token_env: GITHUB_TOKEN
```

**After:**
```yaml
spec:
  entries:
    buildin:
      ref:
        path: ${CONFIGDIR}/tasks/buildin/taskset.yaml
        watch: true
    examples:
      ref:
        url: https://github.com/dicode-ayo/dicode-core
        branch: main
        path: tasks/examples/taskset.yaml
        poll_interval: 5m
        auth:
          token_env: GITHUB_TOKEN
```

**Field mapping:**

| Old `sources[]` field | New location |
|---|---|
| `name` | entry key (e.g. `buildin:`) |
| `type` | inferred: `url` present → git; `path` present → local |
| `path` | `ref.path` |
| `url` | `ref.url` |
| `branch` | `ref.branch` |
| `poll_interval` | `ref.poll_interval` |
| `auth.token_env` | `ref.auth.token_env` |
| `auth.ssh_key` | `ref.auth.ssh_key` |
| `watch` | `ref.watch` |
| `dev_ref` | `ref.dev_ref` |
| `tags` | `entry.tags` |

If you have a `sources:` array in your `dicode.yaml`, the daemon will refuse to start and print an error pointing at this migration guide. The error message includes the issue tracker URL: see [#261](https://github.com/dicode-ayo/dicode-core/issues/261).
