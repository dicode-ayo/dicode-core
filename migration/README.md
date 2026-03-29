# Task Set Architecture — Migration Design

This document describes the planned refactor of how Dicode discovers and manages tasks,
introducing a hierarchical **TaskSet** model inspired by ArgoCD's App-of-Apps pattern
while keeping the existing `sources` mechanism intact.

---

## Motivation

The current `sources` model points at a git repo or local directory and scans a flat
`tasks/` folder. There is no way to:

- Reference individual tasks from different repositories
- Override a task's trigger, params, or env without forking it
- Compose task groups hierarchically (team → project → task)
- Avoid redundant clones when multiple entries reference the same repo

The new model adds a `kind` field to the file a source resolves to. A source can now
point to a single `Task` or a `TaskSet` — a declarative file that references other tasks
and task sets, with per-entry overrides, namespace scoping, and repo deduplication.

---

## Core Concepts

| Concept | Description |
| --- | --- |
| **Source** | Unchanged entry in `dicode.yaml`. `path` field (default `taskset.yaml`) points at the entry file. Sensible defaults for `branch`, `poll_interval`, `auth`. |
| **kind: Task** | Existing `task.yaml` format, unchanged. |
| **kind: TaskSet** | New file format. Named map of entries, each pointing at a `task.yaml`, another `taskset.yaml`, or defined inline. |
| **kind: Config** | Optional config file shipped alongside a TaskSet. Scopes runtime defaults, notifications, secrets, and timeouts to that source. |
| **Ref** | `url` present → git ref. `path` only → local ref. `path` always points to a `.yaml` file. |
| **Overrides** | Patch-style. Leaf wins. See full precedence stack below. |
| **Namespace** | Source `name` + entry key joined with `/` → fully-qualified task ID. |

---

## Source Changes

`sources` stays as-is. Three new optional fields:

| Field | Default | Description |
| --- | --- | --- |
| `name` | last URL/dir segment | root namespace for tasks from this source |
| `path` | `taskset.yaml` | path to the entry yaml within the repo or dir |
| `config_path` | `dicode-config.yaml` | path to an optional `kind: Config` file |

Other field defaults:

| Field | Default |
| --- | --- |
| `branch` | `main` |
| `poll_interval` | `30s` |
| `auth` | none |

```yaml
sources:

  # Minimal — all defaults applied
  - name: infra
    url: https://github.com/org/infra-tasks

  # Explicit overrides
  - name: backend
    url: https://github.com/team/backend-tasks
    branch: develop
    path: ops/taskset.yaml
    poll_interval: 120s
    auth:
      type: token
      token_env: GITHUB_TOKEN

  # Local
  - name: local-dev
    path: /home/user/dev-tasks/taskset.yaml
```

The file at `path` declares its type via `kind`:

```text
kind: TaskSet  →  task set resolution (new)
kind: Task     →  single task (existing task.yaml behaviour)
kind: Config   →  scoped config (runtimes, notifications, secrets, defaults)
<no kind>      →  backward-compatible: scans tasks/ directory as before
```

A source also looks for `dicode-config.yaml` (or `config_path`) at the same location.
Config values apply only to tasks resolved through that source and its nested task sets.

---

## Ref Format

No `type` field — the resolver infers:

- `url` present → git ref (clone/pull from remote)
- no `url`, only `path` → local ref (watch with fsnotify)

`path` always points directly to a `.yaml` file, never a directory.

```yaml
# Git ref
ref:
  url: https://github.com/org/tasks
  path: tasks/deploy/task.yaml   # path within repo
  branch: main                   # optional, defaults to main
  poll_interval: 30s             # optional
  auth:                          # optional
    type: token
    token_env: GITHUB_TOKEN

# Local ref
ref:
  path: /home/user/tasks/deploy/task.yaml
```

---

## `taskset.yaml` Format

Entries are a **named map** — the key becomes the namespace segment and task ID.
No `type` or `id` fields needed.

```yaml
apiVersion: dicode/v1
kind: TaskSet

metadata:
  name: infra

spec:

  # ── Set-level defaults ────────────────────────────────────────────────────
  # Applied to ALL entries in this set before per-entry overrides.
  # Equivalent to a base layer — any entry can further patch on top.
  defaults:
    timeout: 120s
    env:
      - LOG_LEVEL=info
    trigger:
      cron: "0 6 * * *"   # fallback cron for any task without a trigger

  entries:

    # ── Task reference ────────────────────────────────────────────────────
    # Resolver fetches task.yaml, sees kind: Task → loads it.
    # dev_ref substitutes the ref when --dev is active.
    deploy-prod:
      ref:
        url: https://github.com/org/tasks
        path: tasks/deploy/task.yaml
        dev_ref:
          path: /home/user/tasks/deploy/task.yaml
      overrides:
        enabled: true
        trigger:
          cron: "0 2 * * *"
        params:
          - name: environment
            default: production
        env:
          - DEPLOY_TARGET=prod

    # ── Inline task ───────────────────────────────────────────────────────
    health-check:
      inline:
        name: Health Check
        runtime: deno
        trigger:
          cron: "*/5 * * * *"
        timeout: 10s

    # ── Nested TaskSet ────────────────────────────────────────────────────
    # Resolver sees kind: TaskSet → recurses.
    # overrides.entries patches specific entries in the nested set.
    backend:
      ref:
        url: https://github.com/team/backend-tasks
        path: taskset.yaml
      overrides:
        # Set-level defaults applied to all tasks in the nested set
        defaults:
          env:
            - REGION=eu-west-1
        # Per-entry patches within the nested set
        entries:
          hello-world:
            enabled: false
          deploy:
            trigger:
              cron: "0 3 * * *"
```

---

## Override Precedence Stack

Applied lowest → highest (last writer wins at each field level):

```text
1. task.yaml base values                    (in the referenced repo)
2. kind: Config defaults                    (dicode-config.yaml — timeout, retry, env only)
3. TaskSet spec.defaults                    (this set's baseline for all entries)
4. Parent TaskSet overrides.defaults        (parent pushes defaults into this nested set)
5. Parent TaskSet overrides.entries.<key>   (parent patches a specific entry)
6. Entry-level overrides                    (overrides block on the entry itself)  ← highest
```

Example showing all six levels:

```text
task.yaml:             timeout=60s,  cron="0 8 * * *",  env=[APP=base],  retry=none
Config defaults:       timeout=120s,                                      retry=3x/10s
TaskSet defaults:                    cron="0 6 * * *",  env=[LOG=info]
Parent defaults:                                        env=[REGION=eu]
Parent entry patch:                  cron="0 3 * * *"
Entry overrides:       timeout=30s
─────────────────────────────────────────────────────────────────────────────────────
Result:                timeout=30s,  cron="0 3 * * *",  env=[APP=base, LOG=info, REGION=eu],  retry=3x/10s
```

---

## Field-Level Merge Strategies

Each field has an explicit, documented merge strategy. No silent drops or replacements.

| Field | Strategy | Rule | Available in |
| --- | --- | --- | --- |
| `trigger.*` | Sub-field patch | Patching `cron` does not clear `webhook` or `daemon` | TaskSet defaults, entry overrides |
| `params` | Merge by `name` | Only `default` and `required` patched; unmatched params kept | Entry overrides only |
| `env` | Merge by key | `KEY=val` merged by key; new keys appended; existing keys overwritten | All levels |
| `enabled` | Replace | Boolean, last writer wins | Entry overrides only |
| `timeout` | Replace | Duration, last writer wins | All levels |
| `retry.attempts` | Replace | Integer, last writer wins | Config defaults, TaskSet defaults |
| `retry.backoff` | Replace | Duration, last writer wins | Config defaults, TaskSet defaults |
| `runtime` | Replace | String, last writer wins | Entry overrides only |

**Why this matters**: ArgoCD silently replaces entire arrays when you patch a `values` field.
This caused widespread production incidents. Dicode's explicit per-field strategies mean you
can safely add one env var from a parent without wiping the task's own env list.

### What Each Level Can Set

| Field | `kind: Config` defaults | `spec.defaults` | `overrides.defaults` | entry `overrides` |
| --- | --- | --- | --- | --- |
| `timeout` | yes | yes | yes | yes |
| `retry` | yes | yes | yes | — |
| `env` | yes | yes | yes | yes |
| `trigger.*` | — | yes | yes | yes |
| `params` | — | — | — | yes |
| `enabled` | — | — | — | yes |
| `runtime` | — | — | — | yes |

`kind: Config` is intentionally restricted to runtime/infra concerns (timeout, retry, env).
Trigger schedules and enable/disable are task-set and entry concerns — they should not be
set globally by a config file.

---

## Namespace Resolution

```text
sources[].name  infra
  └── entry     backend          (resolves to kind: TaskSet)
        └── entry  api-deploy    (resolves to kind: Task)
              → full ID: infra/backend/api-deploy
```

- Chain triggers use full IDs: `from: infra/backend/api-deploy`
- Within a task set, relative IDs (`api-deploy`) resolve against the current namespace
- UI, run logs, and API use full namespaced IDs

---

## Repo Deduplication

Resolution happens in two phases:

**Phase 1 — Collect**: Walk the entire task set tree, gather all unique `(url, branch)` pairs.

**Phase 2 — Clone**: One `GitSource` per unique pair. All entries sharing the same
`(url, branch)` reuse the same clone at `~/.dicode/repos/<hash>/`.

```text
(github.com/org/infra-tasks, main)       → ~/.dicode/repos/abc123/
(github.com/org/tasks, main)             → ~/.dicode/repos/def456/
(github.com/team/backend-tasks, develop) → ~/.dicode/repos/ghi789/
```

---

## Dev Mode

When `--dev` flag or `dev: true` in config:

1. Any ref with a `dev_ref` block uses the local ref instead
2. Git refs without `dev_ref` auto-create branch `dicode/dev-<taskId>` and watch it
3. Local `dev_ref` paths watched with fsnotify for instant hot-reload

---

## Backward Compatibility

> **Decision (2026-03-29): backward compatibility is dropped.**
>
> All sources must point to a `taskset.yaml` (or explicit `path`). The old flat `tasks/`
> directory scan is removed. This eliminates an entire detection/fallback branch in the
> reconciler, removes the `<no kind>` code path, and means every source has a single,
> unambiguous entry point.
>
> **Migration cost is low**: add a `taskset.yaml` at the root of each existing repo that
> references each task directory. The `dicode migrate` command (Phase 6) will generate
> this file automatically.

Field defaults still apply — `branch`, `poll_interval`, and `auth` remain optional:

| Field | Default |
| --- | --- |
| `branch` | `main` |
| `poll_interval` | `30s` |
| `auth` | none |

### What Gets Simpler Without Backward Compat

- **Reconciler**: no `kind` detection fallback, no directory scan, no ambiguity between
  "is this a task dir or a taskset dir?" — every source path is a yaml file, full stop
- **Loader**: no special-casing for missing `kind` field; `kind` is required and validated
- **Tests**: no need to test the legacy scan path
- **Docs**: one code path to explain instead of two

---

## Lessons from ArgoCD — What We Do Differently

ArgoCD was the primary inspiration. These are areas where its design has known issues
and how Dicode intentionally diverges:

### 1. Set-Level Defaults (ArgoCD has none natively)

ArgoCD has no way to say "all apps in this ApplicationSet share these Helm values."
The workaround is a Merge generator acting as a base layer — awkward and non-obvious.

Dicode solves this with `spec.defaults` at the TaskSet level. Set once, applies to all
entries. Per-entry `overrides` patches on top.

### 2. Explicit Array Merge Strategies (ArgoCD silently replaces)

ArgoCD's biggest production footgun: if you specify `valueFiles` in a `templatePatch`,
it replaces the whole array rather than appending. Keys in `values:` are silently dropped
if `valuesObject:` is also present (tracked in argocd/argo-cd#15706, #16800).

Dicode has a documented merge strategy per field (see table above). `env` always merges
by key. `params` always merges by name. No silent drops.

### 3. No Two Ways to Set the Same Thing

ArgoCD has `values` (string), `valuesObject` (structured), and `parameters` — three
overlapping mechanisms with subtle precedence rules. One wins silently over another.

Dicode has one override block per entry. One way to set env, one way to set params,
one way to set trigger. No ambiguity.

### 4. Cascade Inheritance (ArgoCD requires workarounds)

ArgoCD feature request #1784 ("cross-app defaults") has been open for years. Users work
around it with Kustomize base layers in Git rather than ArgoCD constructs.

Dicode's precedence stack handles this natively: source config → set defaults → nested
set defaults → parent entry patch → entry overrides. Each layer adds only what it needs.

### 5. Nested Set Overrides Are First-Class

In ArgoCD's App-of-Apps, a parent app cannot patch values in child apps without forking
the child's source. In ApplicationSet you use `templatePatch` but it replaces lists.

In Dicode, `overrides.entries` and `overrides.defaults` on a nested TaskSet entry let a
parent layer cleanly patch into a child set without touching the child's file.

---

## New Go Packages

| Package | Responsibility |
| --- | --- |
| `pkg/taskset/spec.go` | `TaskSetSpec`, `ConfigSpec`, `Entry`, `Ref`, `Overrides`, `Defaults` types |
| `pkg/taskset/loader.go` | Parse `taskset.yaml`; detect `kind`; validate |
| `pkg/taskset/resolver.go` | Tree walk, repo dedup, override cascade |
| `pkg/taskset/dev.go` | Dev mode ref substitution, auto-branch |

### Updated Packages

| Package | Change |
| --- | --- |
| `pkg/config/config.go` | Add `name`, `path`, `config_path` to `SourceConfig`; add field defaults |
| `pkg/registry/reconciler.go` | Detect `kind`; route to `taskset.Resolver` when `kind: TaskSet` |

---

## Open Questions

- [ ] Should inline task scripts be embedded as a string in `taskset.yaml`, or reference
      a file relative to the task set's location?
- [ ] For chain triggers across namespaces, is `from: infra/backend/deploy` the final syntax?
- [ ] Should `overrides.entries` keys use relative or absolute IDs?
