# Override-machinery duplication audit

**Date:** 2026-05-15
**Branch:** `chore/overrides-survey`
**Base:** `origin/main @ 154a3b5`
**Status:** Survey — no implementation. Refactor decision pending.

## TL;DR

The recently-landed 7-PR epic (capped by #303 / #304) **already unified the
override-application path through a single shared helper**: every site that
turns an `Overrides` patch into a merged `*task.Spec` flows through
`taskset.ApplyOverrides` (which is a thin re-export of the unexported
`applyOverrides`, which calls `applyLayer`). Three engine call-sites
(`validateBeforeRefs`, `runPrereqs`, `FireChain`) and two resolver
call-sites (`Entry.Inline` path, `KindTask` path) all use the same
function with the same semantics. There is no duplicate `applyLayer`.

The duplication that **does** exist is **smaller and in a different
shape** than the prompt anticipated:

1. **Two separate "merge an Overrides into an Overrides" helpers** —
   `taskset.mergeOverrides` (combines a parent-entry patch with the
   entry's own overrides) and the layered-application loop inside
   `applyOverrides`. They overlap conceptually but operate on different
   types (the former merges two `*Overrides`, the latter merges
   `*Overrides` onto `*task.Spec`). ~80 LOC of related-but-not-identical
   merge logic.

2. **Failure-chain params are merged inline** in `engine.fireFailureChain`
   (engine.go ~1200) rather than going through `applyOverrides`. This is
   not really an override merge — it's an *input map* merge for a
   different type (`map[string]any` vs `[]ParamOverride`) — but it shares
   the reserved-key contract.

3. **`reservedChainParamKeys` is consulted at three different sites**
   (`OnFailureChainSpec.Validate`, `validatePerEdgeOverrides`, and
   inline in `Spec.validate` for `trigger.chain.params`). Each emits a
   slightly different error message, but the rule is identical.

4. **`config.MergeTaskOverride` is a completely separate code path** —
   it operates on YAML maps (`map[string]any`) using RFC 7396 JSON Merge
   Patch semantics, not on typed `Overrides` structs. It is **not**
   duplicating the taskset merge logic; it persists patches to
   `dicode.yaml`, which are later loaded and applied through
   `applyOverrides`. The duplication concern does not apply here — the
   two layers serve different stages of the lifecycle.

**Recommendation:** the duplication is **real but small** (~30–50 LOC
across 2–3 helpers, plus 3 reserved-key validation sites). A unification
refactor would tidy `mergeOverrides` + `applyLayer` into a single
`merge(into *Overrides, from *Overrides)` primitive and centralize
reserved-key enforcement, but it would not collapse fundamentally
different code paths — there is no "merge X into Y" function that
duplicates `applyLayer`. Worth doing, but not urgent; estimate
~80 LOC removed, two latent inconsistencies fixed.

## 1. Sites Inventory

### 1.1 Override-application sites (turn `*Overrides` + `*task.Spec` → merged `*task.Spec`)

| # | Site | Function / Line | Layer count | Validates result? | Clone base? |
|---|------|-----------------|-------------|--------------------|-------------|
| A | Taskset resolver — inline entry | `pkg/taskset/resolver.go:166-167` | 3 (defaults + parent-entry + entry) | ❌ no | ✅ (via `applyOverrides` → `copySpec`) |
| B | Taskset resolver — ref-based KindTask | `pkg/taskset/resolver.go:228-229` | 3 (same stack) | ❌ no | ✅ |
| C | Engine — Register validation | `pkg/trigger/engine.go:427` (`validateBeforeRefs`) | 1 (per-edge before override) | ✅ `merged.Validate()` | ✅ |
| D | Engine — preflight dispatch | `pkg/trigger/engine.go:736` (`runPrereqs`) | 1 (per-edge before override) | ✅ `merged.Validate()` | ✅ |
| E | Engine — chain fire | `pkg/trigger/engine.go:1081` (`FireChain`) | 1 (per-edge chain override) | ✅ `merged.Validate()` | ✅ |

All five call `taskset.ApplyOverrides(base, layers...)`. All five
receive a deep copy of `base` via `copySpec`. **Zero divergence in the
merge semantics** — the same `applyLayer` runs in each case.

Divergences across sites A–E:

- **Layer count**: A/B pass three layers (defaults → parent-entry → entry); C/D/E pass one (the per-edge `entry.Overrides`).
- **Post-merge validation**: A/B do **not** call `merged.Validate()`; C/D/E do. The resolver-side omission may be intentional (resolver runs before registry registration, so cross-spec validation happens later in `engine.Register`), but it does mean a malformed override on a taskset entry can produce an invalid spec that is only caught at `engine.Register` time, while a per-edge override is caught at config-load.
- **Failure mode**: D/E log-and-skip a malformed edge; C returns an error to the caller. The semantics match each site's lifecycle (Register can refuse to register; runtime dispatch must degrade gracefully).

### 1.2 Override-merging sites (turn `*Overrides` + `*Overrides` → merged `*Overrides`)

| # | Site | Function / Line | Purpose |
|---|------|-----------------|---------|
| F | `taskset.mergeOverrides` | `pkg/taskset/resolver.go:417` | Combine parent-entry patch with entry's own overrides before passing as a single layer through nested-taskset boundary |
| G | (none in engine) | — | Engine sites pass one override at a time as a single layer to `ApplyOverrides`; they never need to pre-combine two overrides |

Note: `mergeOverrides` (F) is **not** the same code as `applyLayer`. It
operates on `*Overrides → *Overrides` (preserving the patch shape, no
spec involved); `applyLayer` operates on `*task.Spec ← *Overrides`
(mutating a spec). They share some sub-helpers (`mergeEnvEntries`,
`mergeNotify`, `mergeParamOverrides`-vs-`mergeParams`) but the
top-level semantics differ.

### 1.3 Params-merging sites (chain input maps)

| # | Site | Function / Line | Type | Reserved-key enforcement |
|---|------|-----------------|------|----|
| H | Success chain | `pkg/trigger/engine.go:1271` (`buildChainInput`) | `map[string]any` userParams + 5 engine keys | Enforced upstream at config-load via `Spec.validate` |
| I | Failure chain | `pkg/trigger/engine.go:1200-1217` (inline in `fireFailureChain`) | `map[string]any` chainSpec.Params + 5 engine keys | Enforced upstream at `OnFailureChainSpec.Validate` |
| J | if_missing prereq | `pkg/trigger/engine.go:2154` (inline) | `map[string]any` (empty); `Params` passed separately on `RunOptions` | No reserved keys here — empty map |

H and I do **identical work** (overlay user params, stamp 5 engine
keys) on **identical types** — and they're implemented separately. This
is the smallest, most cosmetic duplication.

### 1.4 YAML-merge-patch site (taskset-level persisted overrides)

| # | Site | Function | Type | Notes |
|---|------|----------|------|-------|
| K | `dicode tasks override` / `PATCH /api/tasks/{id}/overrides` | `pkg/config/persist.go:36` (`MergeTaskOverride`) | `map[string]any` JSON Merge Patch (RFC 7396) | Operates on raw YAML, not typed `Overrides` |

This is a **persistence-layer** concern, not an application-layer one.
The patched YAML is reloaded through `config.Load` → taskset resolver →
sites A/B. K is **not** logically duplicate with the taskset merge: K
edits the on-disk patch text; A/B apply that patch to a spec at
load-time.

### 1.5 Total LOC across override-related code

```
pkg/task/overrides.go              195 LOC  (types + per-edge validation)
pkg/taskset/override.go            314 LOC  (applyLayer + helpers + copySpec)
pkg/taskset/resolver.go            ~80 LOC  (mergeOverrides + buildOverrideLayers + mergeParamOverrides)
pkg/config/persist.go              ~140 LOC (MergeTaskOverride + AtomicWriteFile + mergeMap)
pkg/trigger/engine.go              ~50 LOC  (3 ApplyOverrides callsites + buildChainInput + fireFailureChain input merge)
pkg/task/onfailurechain.go         ~50 LOC  (OnFailureChainSpec types + Validate + reservedChainParamKeys)
pkg/task/spec.go                   ~30 LOC  (per-edge override validation in Spec.validate)
                                  ────────
                              ≈ 859 LOC total
```

Of which the **truly merge-implementing** subset is:

- `applyLayer` (pkg/taskset/override.go) — ~50 LOC
- `mergeOverrides` (pkg/taskset/resolver.go) — ~50 LOC
- `MergeTaskOverride` + `mergeMap` (pkg/config/persist.go) — ~80 LOC
- inline failure-chain input merge (pkg/trigger/engine.go) — ~25 LOC
- `buildChainInput` (pkg/trigger/engine.go) — ~20 LOC

≈ **225 LOC** of merge code total.

## 2. Merge Logic Matrix

Per-field behaviour across the merge sites that operate on the same data:

| Field | A/B (resolver) | C/D/E (engine per-edge) | F (`mergeOverrides`) | K (`MergeTaskOverride`) |
|-------|----------------|--------------------------|----------------------|--------------------------|
| `Enabled` | applied (taskset entry-level toggle) | rejected by `validatePerEdgeOverrides` | a-wins-if-b-nil | RFC 7396 (null deletes) |
| `Name` / `Description` | applied (replace) | rejected | b wins if non-empty | RFC 7396 |
| `Trigger` | applied via `applyTriggerPatch` | rejected | b wins if non-nil, else a | RFC 7396 (recursive map merge) |
| `Params` (ParamOverrides) | `mergeParams` (by name; append) | `mergeParams` (by name; append) | `mergeParamOverrides` (by name; b wins) | RFC 7396 (list replaces whole) |
| `Env` | `mergeEnvEntries` (by name; overlay wins) | same | same | RFC 7396 |
| `Net` / `Fs` | full replace | full replace | b wins if non-empty | RFC 7396 |
| `Dicode` | `mergeDicodePerms` (union for Tasks; sticky-true bools) | same | (not merged in F — b wins if non-nil; rare path) | RFC 7396 |
| `Timeout` / `Retry` / `Runtime` | overlay wins on non-zero | overlay wins; `Retry` rejected on per-edge | b wins on non-zero | RFC 7396 |
| `Notify` | `mergeNotify` (non-nil overlay fields win) | same | `mergeNotify(a, b)` | RFC 7396 |
| `Defaults` | applied (legacy) | rejected | b wins if non-nil | RFC 7396 |
| `Entries` | applied | rejected | recursive map merge | RFC 7396 |

**Observation:** A/B/C/D/E all go through `applyLayer`, so the per-field
behaviour in those columns is **literally the same code**. F is a
separate implementation with subtly different semantics (e.g. for
`Params`, F uses `mergeParamOverrides` which does *replace-by-name*
while A/B/C/D/E use `mergeParams` which does *patch-by-name* — they
happen to be equivalent in practice because the `ParamOverride` shape
has only two non-Name fields).

## 3. Reserved-Key Validation Coverage

`reservedChainParamKeys` is checked at **three** sites:

| Site | Where | What it guards |
|------|-------|----------------|
| 1 | `OnFailureChainSpec.Validate` (pkg/task/onfailurechain.go:121) | `OnFailureChainSpec.Params` keys |
| 2 | `Spec.validate` for `trigger.chain.params` (pkg/task/spec.go:733) | `ChainTrigger.Params` keys |
| 3 | `validatePerEdgeOverrides` (pkg/task/overrides.go:190) | `Overrides.Params` names on per-edge sites |

All three call `reservedChainParamKeys[name]`. The error messages
differ:

- Site 1: `"on_failure_chain.params: %q is a reserved key..."`
- Site 2: `"trigger.chain.params: %q is a reserved key..."`
- Site 3: `"%s: params %q is a reserved key..."` where `%s` is the location string

This **is** duplication, but it's tiny (~3 LOC each) and the
site-specific error messages have operator value. **Low-impact.**

### Gap

`taskset.mergeOverrides` (F) does **not** validate reserved keys after
merging. In practice this is fine because the inputs have already been
validated at config-load (sites 1/2/3), but the function does not assert
its invariant. If a future caller passes an unchecked `*Overrides` to
`mergeOverrides`, reserved keys would slip through.

## 4. Cloning Behavior

| Site | Clones base? | How |
|------|--------------|-----|
| A, B (resolver) | ✅ | `applyOverrides` → `copySpec` (pkg/taskset/override.go:269) |
| C, D, E (engine) | ✅ | Same — they all go through `taskset.ApplyOverrides` |

`copySpec` clones every slice field (`Params`, `Permissions.Env`,
`Permissions.FS`, `Permissions.Run`, `Permissions.Net`) and the
pointer fields (`Trigger.Chain`, `Docker`, `Notify`,
`Permissions.Dicode`). It does **not** clone:

- `Trigger.Before` (slice of `BeforeEntry`) — *latent bug*: a layer that
  mutates `Trigger.Before` after `copySpec` would alias the original.
  Currently no `applyLayer` field writes to `Trigger.Before` so this is
  unreachable, but it would become reachable if per-edge overrides
  ever gained a `Before` field.
- `OnFailureChain` (pointer) — same situation.
- `RunInputs` (pointer, see spec.go:474) — same.

**Severity: low.** Pure latent risk; no current callsite exercises it.

## 5. Divergences / Bugs

### 5.1 Resolver omits post-merge validation [latent footgun]

Sites A/B (resolver) do not call `merged.Validate()` after applying
layers; sites C/D/E (engine per-edge) do. **Impact:** a malformed
override on a taskset entry produces an invalid `*task.Spec` that is
only caught when `engine.Register` runs `spec.validate()` separately,
producing a less-specific error message (the operator gets the
"register failed" error rather than "your override is malformed").

**Severity: low.** The error still surfaces, just less helpfully.

### 5.2 `mergeOverrides` (F) silently lacks reserved-key enforcement [latent footgun]

If a caller ever passed two unchecked `*Overrides` to `mergeOverrides`,
reserved-key params could slip through. Today all callers pass values
that went through config-load validation, so this is unreachable. If
the unification refactor merges F and `applyLayer`, this gap should be
explicitly closed.

**Severity: low.**

### 5.3 `copySpec` doesn't clone `Trigger.Before` / `OnFailureChain` / `RunInputs` [latent footgun]

See §4. No callsite currently exercises any of these via `applyLayer`,
but `Overrides` does not have explicit invariants saying "won't ever
write to these fields". If per-edge overrides ever gain a `Before` or
`OnFailureChain` field, the shallow-copy gap would become a real bug.

**Severity: low.**

### 5.4 `buildChainInput` and `fireFailureChain` inline input merge are near-identical

Both build `map[string]any` from user-supplied params + 5 engine keys.
They differ only in `_chain_depth` (0 vs `nextDepth`) and one
special-case "auto-fix" mode default. Could collapse to a single
helper. ~10 LOC duplicate.

**Severity: trivial.**

### 5.5 `mergeParams` (in `applyLayer`) and `mergeParamOverrides` (in `mergeOverrides`) are near-identical

Both walk a list, find by `Name`, replace-or-append. `mergeParams`
patches individual fields (`Default`, `Required`) on `task.Param`;
`mergeParamOverrides` does a full struct copy on `ParamOverride`.
They're operating on *different element types* (`task.Param` vs
`ParamOverride`) so they aren't trivially mergeable, but the pattern
is identical. ~20 LOC duplicate behaviour.

**Severity: trivial.**

## 6. Proposed Unification Architecture

Given the survey shows **less duplication than initially hypothesized**,
the proposed refactor is **conservative**:

### 6.1 What is already unified (no change needed)

- `taskset.ApplyOverrides` is the single entrypoint for
  `Overrides → Spec` application. All five A/B/C/D/E call sites use it.
- `copySpec` is the single deep-copy helper.
- `applyLayer` is the single per-field merge implementation.

### 6.2 What could be unified

**6.2.1** Collapse `mergeOverrides` (F) and `applyLayer` into a single
"merge overrides" primitive parameterised by output type.

Concrete shape:

```go
// pkg/task/overrides.go (or pkg/taskset/override.go)

// Merge produces a new Overrides that is a layered b-on-top-of-a.
// Equivalent to applying a then b on top of an empty *task.Spec —
// but stays in the Overrides domain so callers can pre-combine
// patches before passing them to ApplyOverrides.
func Merge(a, b *Overrides) *Overrides { ... }
```

`mergeOverrides` (F) would become a call to `Merge`. `applyLayer`
would stay; `applyOverrides` would internally use `Merge` to combine
all layers first, then apply once. This is a **non-trivial change** —
it requires `applyLayer` to be re-expressed in terms of merging
`Overrides` into `Overrides`, which then writes to `Spec` — but it
collapses the two `mergeParamOverrides` / `mergeParams` paths
into one.

**Estimated LOC removed:** ~50 (the F implementation, the
mergeParamOverrides helper, and ~half of applyLayer's per-field
boilerplate).
**Estimated LOC added:** ~30 (the new `Merge` function).
**Net:** ~20 LOC removed.

**6.2.2** Centralize reserved-key validation in a single helper.

```go
// pkg/task/overrides.go

func validateReservedChainParams(site string, params map[string]any) error { ... }
func validateReservedChainParamsList(site string, params ParamOverrides) error { ... }
```

The three current sites (`OnFailureChainSpec.Validate`,
`Spec.validate`-trigger.chain, `validatePerEdgeOverrides`) each become
a one-line call. Each site already builds its own site string, so the
error messages stay distinct.

**Estimated LOC removed:** ~15.
**Estimated LOC added:** ~20.
**Net:** ~5 LOC added; primary benefit is enforcement consistency.

**6.2.3** Replace inline `fireFailureChain` map-build with `buildChainInput`.

```go
// before (engine.go:1200-1217 inline)
input := map[string]any{}
for k, v := range chainSpec.Params { input[k] = v }
if targetID == "buildin/auto-fix" { ... }
input["taskID"] = ...

// after
input := buildChainInput(chainSpec.Params, completedTaskID, runID, runStatus, output)
input["_chain_depth"] = nextDepth   // overwrite the 0 default
if targetID == "buildin/auto-fix" {
    if _, ok := input["mode"]; !ok { input["mode"] = "review" }
}
```

The `buildChainInput` would need an optional depth parameter, or its
callers would patch `_chain_depth` after the call.

**Estimated LOC removed:** ~10.
**Estimated LOC added:** ~3.
**Net:** ~7 LOC removed; primary benefit is one canonical chain-input
shape.

**6.2.4** Tighten `copySpec` to clone `Trigger.Before`,
`OnFailureChain`, `RunInputs`.

Defensive — purely closes latent gaps in §5.3.

**Estimated LOC added:** ~10.

### 6.3 What is NOT worth unifying

- **`config.MergeTaskOverride`** (K) operates on raw YAML; it serves a
  different lifecycle stage (persistence). Keep separate.
- **The error messages** at the three reserved-key sites — site-specific
  error context has operator value.

### 6.4 Total estimated impact

| Change | LOC removed | LOC added | Net |
|--------|-------------|-----------|-----|
| 6.2.1 — Merge primitive | ~50 | ~30 | -20 |
| 6.2.2 — Centralize reserved-key | ~15 | ~20 | +5 |
| 6.2.3 — Collapse failure-chain input | ~10 | ~3 | -7 |
| 6.2.4 — Tighten copySpec | 0 | ~10 | +10 |
| **Total** | ~75 | ~63 | **-12 LOC** |

Plus 3 latent footguns closed (§5.1, §5.2, §5.3) and 1 enforcement
consistency improvement (§5.5 still has two distinct functions but
they share a primitive via 6.2.1).

## 7. Recommendation

The duplication the user observed is **real but smaller than expected**.
The 7-PR epic already collapsed the override-application path through
`taskset.ApplyOverrides`; what remains is a separate
`mergeOverrides → mergeOverrides` helper, near-identical chain-input
builders, and three sites checking the same reserved-key set with
different error messages.

A unification refactor would:

- **Remove ~12 net LOC** (the math is small because most "duplication"
  is actually distinct semantics on distinct types — F operates on
  `Overrides`, K operates on YAML maps, H/I operate on chain input
  maps; these don't compress trivially).
- **Close 3 latent footguns** (§5.1, §5.2, §5.3) — none currently
  reachable but all worth tightening.
- **Add one new abstraction** (`Merge` primitive) which makes the
  architecture more legible but adds a layer of indirection.

**Verdict:** worth doing as one focused PR — call it
"refactor(overrides): consolidate merge primitives + reserved-key
validation" — but **not urgent**. The current code is correct, tested,
and well-commented; the refactor improves architectural cleanliness
more than it fixes bugs.

If we proceed: PR scope is small (~5 files touched, ~100 LOC diff
total), no behavioural change, all existing tests should pass
unchanged. Recommended approach is to land the four sub-changes (6.2.1
through 6.2.4) as one commit, since they're tightly coupled.
