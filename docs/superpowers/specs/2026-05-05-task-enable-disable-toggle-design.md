# Task Overrides Patch API + Enable/Disable Toggle — Design

**Status:** Spec for review
**Author:** drojdestvensky / Claude
**Related:** PR #262 (`enabled` shortcut + dicode.yaml-as-TaskSet), Issue #261 (user-config-as-git follow-up)

---

## Goal

Build a **generic per-task overrides patch API** that lets the webui (and any future client) mutate a task's override layer in `dicode.yaml`. Ship the **enable/disable toggle** as the first consumer of that API. Future feature work (param editor, env editor, timeout slider, trigger tweaks) reuses the same endpoint without backend changes.

The persisted state is always a partial `Overrides` block under `spec.entries.<source>.overrides.entries.<sub>` in `dicode.yaml` — the canonical override layer that PR #262 established. There is no separate runtime store.

## Non-goals

- A separate runtime overlay store. The patch lives in `dicode.yaml` exactly where a hand-edit would live.
- Versioning / undo / git history — that belongs in #261.
- Bulk multi-task patching — one task per request. (Easy to add later as `/api/tasks:patch` if needed.)
- Editing `task.yaml` on disk. Surprising for git-backed sources; rejected during brainstorming.
- Frontend UI for *anything beyond* the enable/disable toggle in this PR. Future override UIs (params/env/etc.) ship in their own PRs against the same endpoint.

## Background

PR #262 established the override cascade: `task.yaml` base → root TaskSet `spec.defaults` → per-entry overrides (leaf wins). Disabled tasks are emitted by the resolver with `Spec.Enabled = false` ([pkg/taskset/resolver.go:170](pkg/taskset/resolver.go#L170)) and skipped by the engine at [pkg/trigger/engine.go:328-334](pkg/trigger/engine.go#L328-L334). The JSON shape at `/api/tasks` already includes `enabled` and the full resolved spec.

What's missing is the **mutation path** that writes back into `dicode.yaml`'s override layer.

## Architecture

```
┌──────────────────┐  PATCH /api/tasks/{id}/overrides   ┌─────────────────────┐
│ dc-task-list     │       {"enabled": false}            │ apiPatchTask        │
│ toggle (today)   │────────────────────────────────────▶│ Overrides           │
└──────────────────┘                                     │ (pkg/webui)         │
                                                         └──────────┬──────────┘
┌──────────────────┐  PATCH /api/tasks/{id}/overrides              │
│ dc-task-detail   │       {"params": {"x": "y"}}                  │
│ params editor    │  (future, no backend change needed)            │
│ (future)         │                                                │
└──────────────────┘                                       ┌────────▼─────────┐
                                                           │ MergeTaskOverride│
                                                           │ (pkg/config)     │
                                                           │ - mtime check    │
                                                           │ - JSON Merge     │
                                                           │   Patch into     │
                                                           │   yaml.v3 node   │
                                                           │ - atomic rename  │
                                                           └────────┬─────────┘
                                                                    │
                                                           ┌────────▼─────────┐
                                                           │ reconciler.Sync()│
                                                           └──────────────────┘
```

## Component breakdown

### 1. Task ID → `dicode.yaml` path resolution

**File:** `pkg/config/taskpath.go` (new)

```go
// SplitTaskID splits a namespaced task ID into the top-level source key
// (matches dicode.yaml spec.entries.<key>) and the sub-path used for
// overrides.entries.<sub>. Returns ok=false if id has no separator.
//
//   "buildin/temp-cleanup"             → ("buildin", "temp-cleanup", true)
//   "infra/platform/nginx"             → ("infra",   "platform/nginx", true)
//   "buildin"                          → ("",        "",               false)
func SplitTaskID(id string) (source, sub string, ok bool)
```

**Implementation note:** sub-paths with multiple segments may need to walk nested `overrides.entries` mappings (one level per segment) instead of using a flat `"platform/nginx"` key. Resolve during implementation by adding a focused unit test against the resolver's parent-override matching logic; both encodings are valid YAML, so we pick whichever the resolver already honors. Document the decision in the helper's godoc.

### 2. Generic config persistence — JSON Merge Patch into `dicode.yaml`

**File:** `pkg/config/persist.go` (new)

```go
// MergeTaskOverride applies a JSON Merge Patch (RFC 7396) to the override
// block at spec.entries.<source>.overrides.entries.<sub> in the dicode.yaml
// at path. patch is the raw JSON body from the client — already-validated
// against the taskset.Overrides schema by the caller.
//
// Semantics:
//   - Scalars & objects in patch SET the corresponding YAML key.
//   - JSON null in patch DELETES the corresponding YAML key.
//   - Maps merge recursively. Lists replace whole.
//   - Missing keys in patch leave the YAML untouched.
//
// Concurrency:
//   - Compares file mtime against expectedMtime; mismatch → ErrConcurrentModification.
//   - Writes via temp file + atomic rename in the same dir.
//
// The yaml.v3 node API preserves comments and whitespace surrounding patched keys.
func MergeTaskOverride(path, taskID string, patch json.RawMessage, expectedMtime time.Time) error

var ErrConcurrentModification = errors.New("config file modified externally")
```

**Why JSON Merge Patch (RFC 7396):**
- Trivial mental model: send only what you want to change.
- `null` → delete maps cleanly onto yaml.v3 mapping operations and lets a future "reset to default" UI work without a separate endpoint.
- Smaller wire format than JSON Patch (RFC 6902) op arrays; more readable in network logs.
- Implementations are tiny (~100 LoC); we write our own tied to yaml.v3 instead of pulling a JSON-tree library.

**Schema validation lives in the API layer**, not in `MergeTaskOverride`. The caller decodes into `taskset.Overrides` (with `DisallowUnknownFields`), re-marshals to JSON to canonicalize, and hands the canonical bytes to the merger. This keeps the merger ignorant of the schema and lets future Overrides field additions work without touching this code.

**Pruning:** if after merging an entry's `overrides:` block becomes empty (`{}`), prune it. If `overrides.entries.<sub>` becomes empty, prune that key. Avoids YAML clutter from churn. Pure node-level cleanup, deterministic, easy to test.

### 3. REST API

**Route:** `PATCH /api/tasks/{id}/overrides` — protected by `requireAuth` (same as `/api/tasks/{id}/run`).

**Request body:** a partial `taskset.Overrides` JSON object. Today only `enabled` is meaningfully consumed by the toggle UI, but every field on `Overrides` (params, env, timeout, retry, runtime, notify, trigger, fs, net, dicode, name, description) is accepted on day one. Unknown fields → 400.

**Examples:**

```jsonc
// Disable a task
{ "enabled": false }

// Future: override two params and bump timeout
{ "params": { "model": "gpt-4o" }, "timeout": "5m" }

// Future: clear the timeout override (revert to base/defaults)
{ "timeout": null }
```

**Responses:**
- `200 OK` `{"id": "...", "overrides": <merged-overrides>}` on success
- `400` invalid JSON, unknown override field, or unsplittable task ID
- `404` task ID not in registry
- `409 Conflict` on mtime mismatch — body: `{"error": "config file modified externally", "code": "concurrent_modification"}`
- `422` patch would create a logically invalid state (e.g. enabling a task whose ancestor source is disabled — see edge cases)
- `500` on YAML write failure (with file untouched — atomic rename guarantee)

**Handler outline:**

```go
func (s *Server) apiPatchTaskOverrides(w http.ResponseWriter, r *http.Request) {
    id := taskIDParam(r)
    spec, ok := s.registry.Get(id)
    if !ok { jsonErr(w, "task not found", 404); return }

    raw, err := io.ReadAll(r.Body)
    if err != nil { ... 400 ... }

    // Schema-validate by decoding into Overrides with strict field checking.
    dec := json.NewDecoder(bytes.NewReader(raw))
    dec.DisallowUnknownFields()
    var ov taskset.Overrides
    if err := dec.Decode(&ov); err != nil { ... 400 ... }

    // Logical validation (e.g. ancestor-disabled check for enable=true).
    if err := s.validateOverridePatch(spec, &ov); err != nil { ... 422 ... }

    // Re-canonicalize so unknown-field-ness can't smuggle past the merger.
    canonical, err := json.Marshal(ov)
    if err != nil { ... 500 ... }

    fi, err := os.Stat(s.configPath)
    if err != nil { ... 500 ... }

    if err := config.MergeTaskOverride(s.configPath, id, canonical, fi.ModTime()); err != nil {
        if errors.Is(err, config.ErrConcurrentModification) { ... 409 ... }
        ... 500 ...
    }

    if s.reconciler != nil { go s.reconciler.SyncOnce(r.Context()) }

    jsonOK(w, map[string]any{"id": id, "overrides": ov})
}
```

**Why a re-canonicalize step:** the body might have arrived in a non-strict shape that `json.Decode` was lenient about (e.g. extra whitespace, key ordering). Marshaling back from the typed Overrides gives the merger a known-valid JSON tree.

### 4. Frontend — `dc-task-list.js` (today's only consumer)

**Toggle placement:** small switch icon at the right of each task row, between trigger label and last-run dot. Inline SVG (Lucide-style circle / circle-with-slash), no new deps.

**Visual states:**

| State | Row style | Toggle icon |
|-------|-----------|-------------|
| Enabled | normal | filled circle |
| Disabled | `opacity: 0.55`, italics on name, "paused" badge next to name | hollow circle with slash |
| Pending toggle | toggle disabled, spinner | spinner |
| Error (409) | brief red flash, reload list, toast: "Config changed externally — reloaded" | original |
| Error (422 ancestor-disabled) | toast: "Source <X> is disabled — enable it first" | revert |
| Error (other) | toast: error message | revert |

**Event flow:**

1. Click → optimistic flip + spinner.
2. `fetch('/api/tasks/' + id + '/overrides', {method:'PATCH', body: JSON.stringify({enabled: newState})})`.
3. 200 → keep optimistic state.
4. 409 → `_loadTasks()` to resync, toast.
5. 422 → revert + targeted toast.
6. Other error → revert + generic toast.

**Reconciler convergence:** existing `_loadTasks()` 5s poll + the API's `SyncOnce` kick guarantees the canonical state lands within ~6s for every client.

### 5. Tests

| Layer | File | Coverage |
|-------|------|----------|
| Unit | `pkg/config/taskpath_test.go` | SplitTaskID for 1/2/3-segment IDs; rejects no-separator |
| Unit | `pkg/config/persist_test.go` | MergeTaskOverride for: (a) `{enabled:false}` first patch, (b) `{enabled:true}` overwriting prior, (c) `{params:{x:"y"}}` adding nested map, (d) `{timeout:null}` deleting key, (e) auto-prune empty overrides, (f) preserves comments above unrelated entries, (g) atomic rename, (h) ErrConcurrentModification on mtime mismatch |
| Integration | `pkg/webui/server_test.go` | PATCH happy path; 400 unknown field; 400 invalid JSON; 404 unknown task; 409 mtime conflict; 422 ancestor-disabled |
| Existing | `pkg/taskset/resolver_test.go` | Cascade behavior already covered — no new test |
| E2E | `tests/e2e/task-toggle.spec.ts` (new) | Click toggle → row paused; reload → state persists; second toggle re-enables; concurrent edit → 409 toast & resync |

The unit tests deliberately exercise *several* override fields (`enabled`, `params`, `timeout`) to lock in the genericity claim — if a future contributor accidentally specializes the merger to `enabled`, these tests fail.

## File structure

```
pkg/config/
├── taskpath.go               (new) — SplitTaskID
├── taskpath_test.go          (new)
├── persist.go                (new) — MergeTaskOverride + JSON-merge-into-yaml-node helpers
├── persist_test.go           (new)

pkg/webui/
├── server.go                 (modified) — register PATCH route, apiPatchTaskOverrides
├── server_test.go            (modified) — add 6 integration tests
├── (existing source persist) (untouched) — sources.go's persistConfig is for
│                                            sources only; tasks go through
│                                            MergeTaskOverride to keep concerns split

tasks/buildin/webui/app/components/
├── dc-task-list.js           (modified) — toggle UI + handler

tests/e2e/
├── task-toggle.spec.ts       (new) — full UI flow

docs/superpowers/specs/
└── 2026-05-05-task-enable-disable-toggle-design.md  (this file)
```

**Why a new `persist.go` instead of extending `pkg/webui/sources.go` `persistConfig`:** that function YAML-marshals an entire `*Entry` value to replace one source. The override-merge case is fundamentally different — it's a JSON-merge-patch into a sub-tree of an existing entry, comment-preserving. Keeping them separate lets each path stay simple.

## Edge cases

| Case | Behavior |
|------|----------|
| Task disabled in task.yaml base, then UI re-enables | Override layer flips to `enabled: true`, leaf wins. ✅ |
| Task ID not under any source | 400 from SplitTaskID. |
| Task disabled by parent TaskSet (e.g. `relay-client: enabled: false` in buildin/taskset.yaml) | UI override goes into dicode.yaml; dicode.yaml override wins → operator can re-enable. ✅ |
| dicode.yaml hand-edited mid-flight | mtime → 409 → frontend reloads. |
| Multiple toggles in quick succession | Each is a separate request; second arrives, re-stats file, mtime now matches the first write. Effective per-file lock. |
| Comment block above an entry | Preserved (yaml.v3 node API). |
| Toggling a child to enabled while ancestor source is disabled | 422 with explicit message. Spec field `validateOverridePatch` enforces. |
| Patch sets a field already equal to base + has no other overrides | After write, prune step removes the empty-now overrides block. Repeated toggles don't accumulate cruft. |
| Patch contains `null` for a field that doesn't exist in current overrides | No-op. RFC 7396-correct. |
| Patch is `{}` (empty object) | No-op write; still bumps mtime — acceptable, simpler than detecting. |

## Tradeoffs / open questions

1. **Sub-path encoding for nested IDs (flat `"platform/nginx"` vs nested mappings)** — implementation-time decision driven by what the resolver already honors. Spec accommodates either; godoc on SplitTaskID will record the choice.
2. **422 vs 409 for ancestor-disabled** — went with 422 (semantic violation) over 409 (concurrent change) because the operator's mental model is "this is invalid right now," not "something else changed." Open to change.
3. **No PUT for full overrides replacement** — could add later if a "reset all overrides" UI emerges. PATCH-with-explicit-nulls covers the case today.
4. **No bulk endpoint** — see non-goals.

## Out of scope (filed under #261)

- Versioned config history / rollback
- Multi-user merge of UI edits
- A general "edit any task field from the UI" surface (this spec defines the *backend* for it; the frontends ship later)

---

*Self-review notes:* No TBDs. Internal consistency: PATCH endpoint path matches REST resource convention; mtime story consistent across §2/§3; visual states in §4 match status codes in §3. Scope: backend is generic but frontend is intentionally minimal — fits one PR. Ambiguity: §1 sub-path encoding is the only open implementation-time decision and it's explicitly flagged with a remediation plan (focused unit test).
