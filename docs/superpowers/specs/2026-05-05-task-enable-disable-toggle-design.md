# Task Enable/Disable Toggle — Design

**Status:** Spec for review
**Author:** drojdestvensky / Claude
**Related:** PR #262 (`enabled` shortcut + dicode.yaml-as-TaskSet), Issue #261 (user-config-as-git follow-up)

---

## Goal

Let an operator flip a task between **enabled** and **disabled** from the dicode webui. The change persists to `dicode.yaml` (single source of truth), is reflected immediately in the task list (no daemon restart), and survives restarts.

## Non-goals

- A separate runtime overlay store. The flip lives in `dicode.yaml` exactly where a hand-edit would live.
- Versioning / undo / git history. That belongs in #261's user-config-as-git epic.
- Bulk enable/disable. One task at a time. (Easy to add later.)
- Editing `task.yaml` on disk. Surprising for git-backed sources; rejected during brainstorming.

## Background

PR #262 already laid the groundwork:

- Resolver emits disabled tasks with `Spec.Enabled = false` ([pkg/taskset/resolver.go:170](pkg/taskset/resolver.go#L170)) — they appear in `/api/tasks` instead of being filtered out.
- Engine skips scheduling for disabled tasks ([pkg/trigger/engine.go:328-334](pkg/trigger/engine.go#L328-L334)) — no triggers, no webhook routes, no daemon spawn.
- `Spec.Enabled` is JSON-tagged at [pkg/task/spec.go:364](pkg/task/spec.go#L364) so the frontend already receives it.

What's missing is the **write path** and the **UI control**.

## Architecture

```
┌──────────────┐   PATCH /api/tasks/{id}    ┌──────────────────────┐
│ dc-task-list │──── {"enabled": false} ───▶│ apiPatchTask         │
│ toggle       │                            │   (pkg/webui)        │
└──────────────┘◀─── 200 / 409 conflict ────└──────────┬───────────┘
                                                       │
                                              ┌────────▼────────────┐
                                              │ persistTaskEnabled  │
                                              │ (config writer)     │
                                              │ - mtime check       │
                                              │ - YAML node patch   │
                                              │ - atomic rename     │
                                              └────────┬────────────┘
                                                       │
                                              ┌────────▼────────────┐
                                              │ reconciler.Sync()   │
                                              │ — picks up new ts   │
                                              │ on next 30s tick    │
                                              │ OR explicit kick    │
                                              └─────────────────────┘
```

The flow stays inside the existing taskset/registry/reconciler loop. We don't bypass any layer; we just add a write that the existing reconciler will pick up.

## Component breakdown

### 1. Task ID → `dicode.yaml` path resolution

**Problem:** task IDs are `source/sub/.../leaf` (e.g. `buildin/dev-clones-cleanup` or `infra/platform/nginx`). The dicode.yaml only has top-level `spec.entries`. Sub-tasks live in nested taskset.yamls — but their `enabled` override goes on the **top-level entry's `overrides.entries.<rest-of-path>.enabled`** in dicode.yaml.

**Helper:** `pkg/config/taskpath.go` (new file)

```go
// SplitTaskID splits a namespaced task ID into the top-level source key
// (matches dicode.yaml spec.entries.<key>) and the sub-path used for
// overrides.entries.<sub>. Returns ("", "", false) if id has no separator.
//
//   "buildin/temp-cleanup"             → ("buildin", "temp-cleanup", true)
//   "infra/platform/nginx"             → ("infra",  "platform/nginx", true)
//   "buildin"                          → ("", "", false)  // top-level entries are sources, not tasks
func SplitTaskID(id string) (source, sub string, ok bool)
```

**Note:** sub-paths with multiple segments (`platform/nginx`) currently encode as a flat key in `overrides.entries` — the resolver already merges parent-entry overrides into nested resolution, but it does not split sub-paths. This means the override key in YAML ends up as `"platform/nginx": {enabled: false}`, which the resolver's `parentOverrides.Entries[key]` lookup *does* match because it joins `namespace + key` to build the full ID and looks up the override map by the leaf key. Verify in implementation; if mismatched, add a 2nd helper that walks nested entries.

### 2. Config persistence

**File:** `pkg/config/persist.go` (new) or extend existing `pkg/webui/sources.go` `persistConfig`

```go
// SetTaskEnabled writes spec.entries.<source>.overrides.entries.<sub>.enabled
// in the dicode.yaml at path. Preserves comments/whitespace using yaml.v3 node
// API. Returns ErrConcurrentModification if the file's mtime changed since
// expectedMtime.
func SetTaskEnabled(path, taskID string, enabled bool, expectedMtime time.Time) error
```

Internal steps:
1. `os.Stat` → compare `ModTime()` to `expectedMtime`. Mismatch → `ErrConcurrentModification`.
2. Read file, parse to `*yaml.Node` (preserves comments).
3. Locate `spec.entries.<source>` → get-or-create `overrides.entries.<sub>` mapping → set `enabled: <bool>`.
4. Write to a temp file in same dir + `os.Rename` (atomic).

**Race story:** read mtime before patch, compare. If file changed → 409, frontend shows "config changed externally, reload". No 3-way merge.

**Cleanup:** if the new value would equal the resolved task.yaml base value (i.e., we're explicitly setting `enabled: true` on a task whose base is already true with no other overrides), prune the empty override mapping to avoid yaml clutter. Optional — start without; add if testing shows accumulation.

### 3. REST API

**Route:** `PATCH /api/tasks/{id}` — protected by `requireAuth` (same as `/api/tasks/{id}/run`).

**Body:** `{"enabled": true|false}`

**Response:**
- `200 OK` `{"id": "...", "enabled": false}` on success
- `404` if task ID not in registry
- `409 Conflict` `{"error": "config file modified externally, please reload"}` on mtime mismatch
- `400` on invalid body or unsplittable ID
- `500` on YAML write failure (with file untouched — atomic rename guarantee)

**Handler outline:**

```go
func (s *Server) apiPatchTask(w http.ResponseWriter, r *http.Request) {
    id := taskIDParam(r)
    spec, ok := s.registry.Get(id)
    if !ok { jsonErr(w, "task not found", 404); return }

    var body struct{ Enabled bool `json:"enabled"` }
    if err := json.NewDecoder(r.Body).Decode(&body); err != nil { ... }

    fi, err := os.Stat(s.configPath)
    if err != nil { ... 500 ... }

    if err := config.SetTaskEnabled(s.configPath, id, body.Enabled, fi.ModTime()); err != nil {
        if errors.Is(err, config.ErrConcurrentModification) {
            jsonErr(w, "...", 409); return
        }
        jsonErr(w, err.Error(), 500); return
    }

    // Trigger an explicit reconciler sync so the UI sees the change without
    // waiting for the next 30s tick.
    if s.reconciler != nil {
        go s.reconciler.SyncOnce(r.Context())
    }

    jsonOK(w, map[string]any{"id": id, "enabled": body.Enabled})
}
```

**Why explicit reconciler kick:** after writing the file, the next reconciler tick (≤30s) would pick it up automatically. Calling `SyncOnce` makes the toggle feel instant. Frontend optimistic-updates the row; reconciler kick reconciles in <500ms; if anything fails, the next /api/tasks GET shows the canonical state.

### 4. Frontend — `dc-task-list.js`

**Toggle placement:** add a small switch icon at the right of each task row, between trigger label and last-run dot. Use a circle/slash icon (Lucide-style, inline SVG, no new deps).

**Visual states:**

| State | Row style | Toggle icon |
|-------|-----------|-------------|
| Enabled | normal | filled circle |
| Disabled | `opacity: 0.55`, italics on name, "paused" badge next to name | hollow circle with slash |
| Pending toggle | toggle spins / disabled | spinner |
| Error (409 or 500) | brief red flash, revert to server state | original |

**Event flow:**

1. Click toggle → optimistic flip state + spinner.
2. `fetch('/api/tasks/' + id, {method:'PATCH', body:{enabled: newState}})`.
3. On 200 → keep optimistic state; trust upcoming SSE/poll.
4. On 409 → toast "config file changed; reloading", `_loadTasks()` to resync.
5. On other error → revert + toast.

**No SSE listener needed.** Existing `_loadTasks()` runs on a 5s interval (already in component); the reconciler kick + 5s poll guarantees convergence within ~6s for any client.

### 5. Tests

| Layer | File | Coverage |
|-------|------|----------|
| Unit | `pkg/config/taskpath_test.go` | SplitTaskID for 1-segment, 2-segment, 3-segment IDs; rejects no-separator IDs |
| Unit | `pkg/config/persist_test.go` | SetTaskEnabled creates entries[].overrides.entries[]; preserves comments; atomic rename; mtime mismatch returns ErrConcurrentModification; pruning (if implemented) |
| Integration | `pkg/webui/server_test.go` | PATCH /api/tasks/{id} happy path, 404 unknown task, 409 mtime conflict, 400 bad body |
| Integration | `pkg/taskset/resolver_test.go` (existing) | Already covers parent.overrides.entries.X.enabled cascade — no new test needed |
| E2E | `tests/e2e/task-toggle.spec.ts` (new) | Click toggle → row goes to "paused"; reload page → state persists; second toggle re-enables |

Round-trip test: persist → reload via `config.Load` → resolve → confirm task `Enabled` is the new value.

## File structure

```
pkg/config/
├── taskpath.go         (new) — SplitTaskID helper
├── taskpath_test.go    (new)
├── persist.go          (new or extend webui/sources.go) — SetTaskEnabled
└── persist_test.go     (new)

pkg/webui/
├── server.go           (modified) — register PATCH route, add apiPatchTask handler
├── server_test.go      (modified) — add 4 integration tests

tasks/buildin/webui/app/components/
├── dc-task-list.js     (modified) — toggle UI + handler

tests/e2e/
├── task-toggle.spec.ts (new) — full UI flow

docs/superpowers/specs/
└── 2026-05-05-task-enable-disable-toggle-design.md  (this file)
```

## Edge cases

| Case | Behavior |
|------|----------|
| Task disabled in task.yaml base, then UI re-enabled | Override layer flips to `enabled: true`, leaf wins per resolver precedence. Correct. |
| Task ID not under any source (impossible — every ID is namespaced) | 400 from SplitTaskID. |
| Task disabled at parent TaskSet level (e.g. `relay-client: enabled: false` in buildin/taskset.yaml) | UI flip writes to `dicode.yaml` overrides — overrides win over the in-source disable. ✅ Operator can re-enable a buildin task this way. |
| dicode.yaml hand-edited mid-flight | mtime check → 409 → frontend reloads. |
| Multiple toggles in quick succession | Each is sequential by mtime check; second arrives, re-reads file (now updated mtime), re-checks. Effectively a per-file lock. |
| Reconciler still has old state when /api/tasks/{id} returns 200 | Frontend already optimistic. Polling converges within 5s. |
| dicode.yaml comment block above the entry | Preserved by yaml.v3 node API patch. |
| Toggling a task whose source itself has `enabled: false` (entire source disabled) | Top-level source disabled propagates to all children. Toggling a child to enabled creates an override but the propagation still wins → confusing. Handle: in apiPatchTask, if any ancestor is disabled, return 400 "ancestor source is disabled; enable it first". |

## Tradeoffs / open questions

1. **Pruning empty override mappings** — start without (simpler), revisit if YAML grows. Marked optional in §2.
2. **Sub-path splitting for nested IDs** — verify the resolver's lookup model during implementation; the override key may need to be the leaf only, or the full sub-path. Implementation-time decision; spec describes both.
3. **Permission model** — same as `/api/tasks/{id}/run` (session + auth). No need for a separate scope.

## Out of scope (filed under #261)

- Versioned config history / rollback
- Multi-user merge of UI edits
- A general "edit any task field from the UI" surface

---

*Self-review notes:* No TBDs. Internal consistency: PATCH endpoint path matches /api/tasks/{id} convention; mtime story is consistent across §2 and §3; visual states in §4 match error responses in §3. Scope: focused on one capability (toggle); could fit one PR. Ambiguity: §2 mentions "Internal steps" with 4 numbered items; §1 has the explicit-vs-implementation-time ambiguity called out under "Tradeoffs" rather than left silent.
