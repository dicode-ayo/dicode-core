# Migration Progress

Tracking implementation of the TaskSet architecture refactor.

---

## Status Legend

- `[ ]` not started
- `[~]` in progress
- `[x]` done
- `[-]` skipped / out of scope

---

## Phase 0 — Design & Documentation

- [x] Write architecture design (README.md)
- [x] Example: `dicode.yaml` with `task_sets` entry points
- [x] Example: root `taskset.yaml` (task refs, inline tasks, nested task sets)
- [x] Example: nested `taskset.yaml` (team-owned, further nesting)
- [x] Deep research: ArgoCD override mechanisms (Helm layers, Kustomize patches, ApplicationSet templatePatch, Merge generator)
- [x] Improve override design based on ArgoCD findings:
  - [x] Explicit 6-level precedence stack
  - [x] `spec.defaults` block at TaskSet level
  - [x] `overrides.defaults` on nested TaskSet entries
  - [x] Per-field merge strategy table (env by key, params by name, etc.)
  - [x] "Lessons from ArgoCD" section documenting intentional divergences
- [ ] Resolve open questions in README.md
  - [ ] Inline task script embedding vs file reference
  - [ ] Cross-namespace chain trigger syntax (`from: infra/backend/deploy`)
  - [ ] `overrides.entries` key: relative vs absolute IDs

---

## Phase 1 — Core Types

- [ ] `pkg/taskset/spec.go` — define `TaskSetSpec`, `ConfigSpec`, `Entry`, `Ref`, `Overrides`, `Defaults`
- [ ] `pkg/taskset/loader.go` — parse and validate `taskset.yaml`; detect `kind` field
- [ ] `pkg/taskset/loader_test.go` — unit tests for parsing and validation
- [ ] Update `pkg/config/config.go` — add `name`, `path`, `config_path` to `SourceConfig`; add defaults for `branch`, `poll_interval`

---

## Phase 2 — Resolver

- [ ] `pkg/taskset/resolver.go` — walk entry tree, collect unique refs, apply override cascade
- [ ] `pkg/taskset/resolver_test.go` — namespace building, all 6 precedence levels, per-field merge strategies, repo dedup, dev mode substitution
- [ ] Repo deduplication: single `GitSource` per `(url, branch)` pair
- [ ] Override merge logic: patch-style, field-level, leaf wins
- [ ] Integration test: 3-level nested set resolves correct namespaced IDs and overrides end-to-end

---

## Phase 3 — Reconciler Integration

- [ ] Update `pkg/registry/reconciler.go` — detect `kind` in resolved file; call `taskset.Resolver` when `kind: TaskSet`
- [ ] Wire `SourceConfig` (extended with `name`, `path`, `config_path`) to existing `GitSource` / `LocalSource`
- [ ] Load `kind: Config` file alongside TaskSet; apply as precedence level 2
- [ ] Ensure per-ref `poll_interval` is respected (vs global default)
- [ ] Namespace-aware task ID registration in registry

---

## Phase 4 — Dev Mode

- [ ] `pkg/taskset/dev.go` — ref substitution (`dev_ref` replaces `ref` when `--dev` active)
- [ ] Auto-branch logic: create `dicode/dev-<taskId>` branch in cloned repo
- [ ] `--dev` CLI flag wired through to resolver
- [ ] `dev: true` config option in `dicode.yaml`

---

## Phase 5 — Web UI

- [ ] Namespace-aware task list (group by namespace segments)
- [ ] Show full task ID `infra/backend/deploy` in run logs and task detail
- [ ] Task set tree view (visualize nesting)
- [ ] Indicate overridden fields in task detail view

### Source / Ref Management UI

- [ ] **Add source dialog**: form to add a new git ref (url, branch, path, auth token env)
- [ ] **Edit source dialog**: change branch or path on an existing source (writes back to `dicode.yaml`)
- [ ] **Branch picker**: dropdown populated from remote branches via `git ls-remote` on the cloned repo
- [ ] **Path picker**: file browser scoped to the cloned repo, filters to `.yaml` files
- [ ] **Dev mode toggle per ref**: switch icon/button on each source row in the UI
  - Active dev mode shown with a visual indicator (e.g. badge on the source)
  - Toggle calls `PATCH /api/sources/:name/dev` (see Phase 7)
  - State persisted in local config / runtime state, not committed to `taskset.yaml`

---

## Phase 7 — Dev Mode API & MCP

Expose dev mode switching per git ref over both the REST API and MCP so AI agents
(Claude Code, Cursor) can activate dev mode for a specific task when asked to create
or edit a task.

### REST API

- [ ] `GET  /api/sources` — list all sources with their current dev mode state
- [ ] `PATCH /api/sources/:name/dev` — toggle dev mode for a source

  ```json
  { "enabled": true, "local_path": "/home/user/tasks/deploy" }
  ```

  - `local_path` optional: if provided, sets a runtime `dev_ref` override for that source
  - if omitted, Dicode auto-creates branch `dicode/dev-<name>` in the cloned repo

- [ ] `GET  /api/sources/:name/branches` — list remote branches (for branch picker)
- [ ] `GET  /api/sources/:name/tree?path=` — list yaml files in the cloned repo (for path picker)

### MCP Tools

- [ ] `switch_dev_mode` — turn dev mode on/off for a named source or specific task ref

  ```json
  { "source": "infra", "enabled": true, "local_path": "/home/user/..." }
  ```

  Returns: `{ source, dev_mode, branch, local_path }`

- [ ] `list_sources` — list all sources with name, url, branch, path, dev_mode state
- [ ] `add_source` — add a new source entry (writes to `dicode.yaml`)

  ```json
  { "name": "my-tasks", "url": "https://github.com/org/tasks", "branch": "main", "path": "taskset.yaml" }
  ```

  Returns: `{ source, status }`

### Typical AI agent workflow

When an AI agent is asked "create a new task for X":

1. Agent calls `list_sources` to find the right source/repo
2. Agent calls `switch_dev_mode` with `local_path` pointing to a local working directory
3. Agent writes `task.yaml` + script to the local path
4. Dicode hot-reloads via fsnotify — task appears in the registry immediately
5. Agent can run/test the task via existing MCP `run_task` tool
6. When done, agent (or user) calls `switch_dev_mode` with `enabled: false` to return to git ref

---

## Phase 6 — Migration Guide

- [ ] Document how to add `name` and `path` to an existing `sources` entry to opt in
- [ ] Document backward-compat rules (no `taskset.yaml` → existing `tasks/` scan)
- [ ] Add `dicode migrate` dry-run command: reads existing source, proposes `taskset.yaml`
- [ ] Update `examples/` in the repo with real before/after configs

---

## Open Questions

Track decisions here as they are made.

| # | Question | Decision | Date |
| --- | --- | --- | --- |
| 1 | Inline task script: embedded string or file ref? | — | — |
| 2 | Cross-namespace chain trigger syntax? | — | — |
| 3 | `overrides.entries` key: relative or absolute IDs? | — | — |
| 4 | Does parent override win over child, or child wins? | Leaf (child) wins | 2026-03-29 |
| 5 | Dev mode: per-task `dev_ref` or global `--dev` flag? | Both supported | 2026-03-29 |

---

## Testing Requirements

> **This is mission-critical functionality.** The task set resolver is the foundation
> everything else builds on — wrong namespace resolution, a silent override merge bug,
> or a missed repo dedup will cause tasks to run with wrong config or not run at all.
> Every phase must ship with full unit and integration test coverage before being merged.

### Unit Tests (per package)

- `pkg/taskset/loader_test.go` — parse valid/invalid yaml, required fields, `kind` validation
- `pkg/taskset/resolver_test.go`
  - Namespace building: single source, nested sets, 3+ levels deep
  - Override cascade: each of the 6 precedence levels in isolation and combined
  - Per-field merge strategies: `env` merge-by-key, `params` merge-by-name, `trigger` sub-field patch
  - Repo deduplication: same `(url, branch)` referenced N times → exactly 1 `GitSource`
  - Dev mode ref substitution: `dev_ref` replaces `ref` when dev mode active
- `pkg/taskset/dev_test.go` — auto-branch creation, local path override

### Integration Tests

- End-to-end: source pointing to a local `taskset.yaml` → tasks registered in registry with correct IDs
- Nested sets: 3-level deep nesting resolves all task IDs correctly
- Override correctness: parent `overrides.entries` values win over child entry values
- `kind: Config` defaults applied at correct precedence level
- Repo dedup: two entries with same `(url, branch)` share one clone, one poll loop
- Dev mode: switching a source to dev mode hot-reloads from local path via fsnotify

---

## Notes

- Design discussion: conversation on 2026-03-29
- Backward compatibility dropped (2026-03-29) — `kind` is required, no legacy `tasks/` scan
- Inspired by ArgoCD App-of-Apps pattern; ArgoCD override mechanisms researched 2026-03-29
- Existing `pkg/source/` infrastructure (GitSource, LocalSource) is reused — the resolver
  sits above it, deduplicating refs before handing them off to sources
- Key divergence from ArgoCD: explicit per-field merge strategies (no silent array replacement),
  set-level `spec.defaults`, and `overrides.defaults` for nested sets
