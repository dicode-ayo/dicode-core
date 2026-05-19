# PipelineTask: a first-class task kind for orchestrated pipelines

**Status:** draft — pending implementation plan
**Date:** 2026-05-18
**Scope:** New `kind: PipelineTask` in `dicode-core`. Hard cut: removes `trigger.before:` from `kind: Task`. Migrates `buildin/relay-server` + cloudflared docs example.

## Goal

Introduce `kind: PipelineTask` as a first-class task kind that unifies what `trigger.before:` (PRs #311 / #312 / #331) and daemon-with-preflight orchestration do today. Pipelines become the unit; `kind: Task` goes back to "executable task with one body."

The current `trigger.before:` mechanism, while functional, is a bolt-on with accumulated uncomfortable edges:
- `runPrereqs` has a `spec.Trigger.Daemon &&` gate around `daemonRegistered()` (silently-dropped-one-shots bug from PR #331).
- `fireAsync` has a `runOneShotPreflight` branch added in #312.
- `validateBeforeRefs` rejects `${input.params.X}` on every before-edge because daemon preflight can't carry params.
- `dispatchPipelineStage` threads `parentRunID` "for one-shots only; daemons can't carry this link at preflight time."
- Cycle detection runs across all task before-graphs at registration.

`kind: PipelineTask` makes these problems vanish by collapsing the bolt-on into a dedicated type with its own validation, dispatcher, and lifecycle.

## Non-goals (deferred to small follow-ups)

- `subtype: parallel` (the schema reserves the field; v1 validator rejects anything other than `sequential`).
- `if` / `else` / `when` / `continue_on_failure` / fan-in / fan-out grammar.
- Duplicate-task-id-in-one-pipeline disambiguation via explicit `StageRef.id`.
- Backwards-compat shim for `trigger.before:` — there is none.

## Motivation by example

### Today (`kind: Task` + `trigger.before:`)

```yaml
# tasks/buildin/relay-server/task.yaml
apiVersion: dicode/v1
kind: Task
name: "Relay Server"
runtime: deno
trigger:
  daemon: true
  restart: always
  before:
    - task: buildin/template
      overrides:
        params: { template_path: "${TASK_DIR}/relay.yaml" }
        env:    [ ... Doppler-fed OAuth env ... ]
        fs:     [ ... ]
    - task: buildin/write-local
      overrides:
        params: { content: "${input.output}", path: "${DATADIR}/relay/relay.yaml", mode: "0600" }
        fs:     [ ... ]
permissions:
  net: ["*"]
  fs:  [ ... ]
  env: [ DICODE_DATADIR, DICODE_VERSION ]
# (relay's signing-key bootstrap + startServer() in task.ts)
```

The daemon body, the preflight pipeline, and the per-edge overrides all live in one spec, glued together by `trigger.before:`. Operators read one file but the engine treats it as two distinct lifecycles.

### After (`kind: PipelineTask`)

```yaml
# tasks/buildin/relay-server/pipeline.yaml
apiVersion: dicode/v1
kind: PipelineTask
name: "Relay Server"
subtype: sequential
stages:
  - task: buildin/template
    overrides:
      params: { template_path: "${TASK_DIR}/relay.yaml" }
      env:    [ ... Doppler-fed OAuth env ... ]
      fs:     [ { path: "${TASK_DIR}/relay.yaml", permission: r } ]
  - task: buildin/write-local
    overrides:
      params: { content: "${input.output}", path: "${DATADIR}/relay/relay.yaml", mode: "0600" }
      fs:     [ { path: "${DATADIR}/relay", permission: rw } ]
  - task: buildin/relay-server-body   # kind: Task, trigger.daemon: true — its own file
```

```yaml
# tasks/buildin/relay-server-body/task.yaml
apiVersion: dicode/v1
kind: Task
name: "Relay Server (daemon body)"
runtime: deno
trigger:
  daemon: true
  restart: always
permissions:
  net: ["*"]
  fs:  [ { path: "${DATADIR}/relay", permission: rw } ]
  env: [ DICODE_DATADIR, DICODE_VERSION ]
# task.ts: signing-key bootstrap + startServer({configPath: …/relay/relay.yaml})
```

Two files. `relay-server-body` becomes a clean `kind: Task` (still a daemon, just standalone-runnable). The PipelineTask is the orchestration concern; the Task is the runtime concern. Each can be reasoned about independently.

## Design

### Spec schema

`pkg/task/pipeline.go` (new):

```go
// PipelineTask is the spec for kind: PipelineTask.
type PipelineTask struct {
    APIVersion  string          `yaml:"apiVersion"`             // "dicode/v1"
    Kind        string          `yaml:"kind"`                   // "PipelineTask"
    Name        string          `yaml:"name"`
    Description string          `yaml:"description,omitempty"`
    Subtype     string          `yaml:"subtype"`                // "sequential" (v1) | "parallel" (v2+)
    Trigger     PipelineTrigger `yaml:"trigger,omitempty"`      // how the pipeline is fired
    Stages      []Stage         `yaml:"stages"`
    Timeout     time.Duration   `yaml:"timeout,omitempty"`      // overall pipeline timeout
}

// PipelineTrigger is the subset of trigger shapes valid on a pipeline.
// Notably no Daemon: pipelines are daemon-shaped iff their terminal stage
// is a kind: Task with trigger.daemon: true.
type PipelineTrigger struct {
    Manual        bool          `yaml:"manual,omitempty"`
    Cron          string        `yaml:"cron,omitempty"`
    Webhook       string        `yaml:"webhook,omitempty"`
    WebhookSecret string        `yaml:"webhook_secret,omitempty"`
    WebhookAuth   bool          `yaml:"auth,omitempty"`
    Chain         *ChainTrigger `yaml:"chain,omitempty"`
}

// Stage is one entry in a PipelineTask.Stages list.
type Stage struct {
    Task      string     `yaml:"task"`                // task ID to run as this stage
    Overrides *Overrides `yaml:"overrides,omitempty"` // same Overrides type used by trigger.before today
}
```

Notes:
- `Subtype` is a required field in v1. Only `"sequential"` is accepted; anything else is a load-time error referencing the follow-up issue for parallel mode (filed at spec approval).
- `Trigger` is a subset of `kind: Task`'s — no `daemon` here. The daemon-shaped lifetime comes from the terminal Stage when it's a `kind: Task` with `trigger.daemon: true`.
- `Stage` is structurally identical to today's `BeforeEntry`. No `id:` field at v1; duplicate task IDs in one pipeline are a load-time error (deferred follow-up for explicit IDs).
- `Overrides` continues to use the existing type (covers params, env, fs, net, timeout, dicode, runtime). A new "PipelineTask stage" override-validator mode adds `Trigger` to the allowlist (see "Stage trigger overrides" below).

### Stage trigger overrides

Per the design discussion, a PipelineTask can override the trigger of its stages. This handles the case where a `kind: Task` defaults to `trigger.manual: true` but a PipelineTask needs to fire it programmatically.

```yaml
stages:
  - task: my-manual-only-task
    overrides:
      trigger:
        manual: false   # disable the manual-only constraint for this firing
```

`Overrides.Trigger` already exists today via `TriggerPatch` (used by per-edge overrides) but was rejected by `validatePerEdgeOverrides`. PipelineTask stages relax that restriction — trigger overrides ARE allowed at the stage level. (Concretely: the `validatePerEdgeOverrides` allowlist gains a "PipelineTask stage" mode that includes Trigger.)

### Run lifecycle

A PipelineTask's run row is created in `runs` the same way a Task's is. Specifically:

| Field | Value |
|---|---|
| `id` | new UUID (PipelineTask's parent run ID) |
| `task_id` | PipelineTask's task ID |
| `kind` | `"pipeline"` (new column; see schema delta below) |
| `parent_run_id` | the caller's run ID (e.g., chain caller) or NULL for direct fires |
| `trigger_source` | manual / cron / webhook / chain — whichever fired the pipeline |
| `status` | `running` → `success` / `failure` (terminal) |
| `start_time` | when the pipeline began |
| `end_time` | when the terminal stage completed (or NULL while running) |
| `return_value` | structured: terminal stage's return for sequential pipelines |

Each Stage fires as a separate run with:

| Field | Value |
|---|---|
| `id` | new UUID per stage run |
| `task_id` | the stage's `task:` (e.g., `buildin/template`) |
| `kind` | `"task"` (the stage IS a kind: Task) |
| `parent_run_id` | PipelineTask's run ID |
| `trigger_source` | `"pipeline-stage"` (new value; or reuse `preflight` post-rename) |

This means: the runs table sees N+1 rows per pipeline fire (parent + N stages). The presentation-layer filter (`WHERE parent_run_id IS NULL`) hides the children from the default WebUI runs list.

### Schema delta

`runs` table gains a `kind TEXT NOT NULL DEFAULT 'task'` column. Migration backfills `kind = 'task'` for existing rows. New PipelineTask runs get `kind = 'pipeline'`.

`trigger_source` accepts a new value: `'pipeline-stage'` (replacing the existing `'preflight'` which goes away with `trigger.before:`). Backfill: `UPDATE runs SET trigger_source = 'pipeline-stage' WHERE trigger_source = 'preflight'`.

### Status semantics

PipelineTask run status is computed from stage runs:

- **All stages reached `success` AND terminal stage isn't a daemon** → pipeline `success`, `end_time` set to terminal stage's `end_time`.
- **All stages reached `success` AND terminal stage IS a daemon** → pipeline `running` for as long as the daemon's run is `running`. When the daemon's run terminates, the pipeline's run terminates with the daemon's status (`success` / `failure` / `cancelled`).
- **Any stage reaches `failure` / `timeout` / `cancelled`** → pipeline `failure`, `end_time` set immediately, `fail_reason` = `"stage N (<task-id>): <error>"`, subsequent stages NOT fired.

Per the design discussion (user Q1): the failure semantics are uniform `failed` regardless of whether the failing stage was a daemon or a one-shot.

### `${input.*}` interpolation across stages

Stages use the same `${input.*}` grammar shipped in PR #310 + PR #330:
- `${input.output}` — previous stage's return value (string only).
- `${input.output.<field>}` — previous stage's return value field.
- `${input.params.<name>}` — previous stage's input params (not currently populated for sequential pipelines; reserved).

First stage receives no `input` (any `${input.*}` reference is rejected at load time, same as today's `before[0]` rule).

The PipelineTask's OWN return value (consumed by chain downstreams or by an outer pipeline that wraps this one):

- **Sequential pipelines** (v1): the terminal stage's return value. Same shape as today.
- **Parallel pipelines** (v2 follow-up): an object map keyed by stage's task ID — `{stage-id: {status, output, run_id}, ...}`. Downstreams pick via `${input.output.<stage-id>}`.

### Validation

`pkg/task/pipeline.go` `Validate()`:

1. `apiVersion: dicode/v1` and `kind: PipelineTask` required.
2. `name` required, must be unique across the task registry.
3. `subtype: sequential` required (v1). Anything else: `"pipeline subtype '%s' not implemented in v1; see the parallel-pipeline follow-up issue"` (issue number filled in once filed during spec approval).
4. `len(stages) >= 1` required.
5. Each `Stage.Task` must reference an existing task at registration time (`engine.Register` checks).
6. Cycle detection: build the pipeline-stage graph (PipelineTask → Stage.Task references) and run DFS three-colour to reject cycles. Same algorithm as PR #331's `detectBeforeCycle` but operating on the pipeline graph.
7. Per-stage `overrides` go through `validatePerEdgeOverrides` with a new "PipelineTask stage" allowlist mode that includes `Trigger` (for the trigger-override case).
8. `${input.*}` references on stage[0] are rejected at load time. `${input.params.X}` rejected on every stage (always-nil for v1; same restriction as PR #330 added for before-edges).
9. Stage tasks must NOT themselves be PipelineTasks (v1) — pipelines-of-pipelines is the parallel-mode follow-up. Sequential-of-sequential is technically possible but the use case is unclear; reject at v1 to keep the surface minimal. (Easy to relax later.)

Wait — point 9 contradicts the fan-out design where parallel is a nested pipeline. The right rule:

9. Sequential pipelines can reference any task EXCEPT a sequential PipelineTask (avoids deep linear nesting with unclear value at v1). Parallel pipelines (v2) can be stages in sequential pipelines (the fan-out case). Cross-subtype nesting rules belong in the parallel follow-up spec.

Concretely v1: a sequential PipelineTask's stages must be `kind: Task` references. Period. Parallel follow-up loosens this.

### Engine dispatch

`pkg/trigger/pipeline_runner.go` (new):

```go
// PipelineRunner orchestrates the execution of a kind: PipelineTask.
// One PipelineRunner per pipeline fire. Replaces:
//   - runPrereqs (pkg/trigger/engine.go)
//   - dispatchPipelineStage
//   - propagateBeforeRerun
//   - finalizePreflightFailure
//   - runOneShotPreflight branch in fireAsync
type PipelineRunner struct {
    engine     *Engine
    spec       *task.PipelineTask
    parentRunID string
    runID      string  // PipelineTask's own run ID
}

func (p *PipelineRunner) Run(ctx context.Context) error {
    // 1. Create parent run row (kind=pipeline).
    // 2. For each stage (sequential):
    //    a. Resolve ${input.*} against previous stage's output.
    //    b. Apply overrides.
    //    c. Fire stage via existing fireAsync (parent_run_id = p.runID).
    //    d. Wait for stage completion via WaitRun.
    //    e. On failure: mark pipeline failed with stage info; return.
    //    f. On success: capture return value for next stage's input.
    // 3. Mark pipeline succeeded with terminal stage's return.
    // 4. If terminal stage is a daemon: subscribe to its state transitions
    //    and update pipeline's run status accordingly until daemon
    //    terminates.
}
```

`fireAsync` in engine.go gains a top-level branch:

```go
func (e *Engine) fireAsync(ctx context.Context, spec interface{}, opts pkgruntime.RunOptions, source string) (string, error) {
    switch s := spec.(type) {
    case *task.Spec:
        // existing kind: Task dispatch path
    case *task.PipelineTask:
        runner := &PipelineRunner{...}
        return runner.RunAsync(ctx)
    }
}
```

(Adapt to the actual `Spec` interface shape — likely a `Kind() string` method on a base interface.)

### Re-run propagation

Today's `propagateBeforeRerun` (PR #331) handles "operator re-fires stage N of a daemon's preflight → re-fire descendants → restart daemon." With PipelineTask:

- Operator manually re-fires a stage's underlying `kind: Task` (e.g., `dicode run buildin/template ...`).
- The engine observes that this task is a stage in pipeline P, currently in `running` state.
- The engine kicks off a "re-run from stage N" pass: re-fires stages [N..terminal] sequentially with fresh `${input.*}` values.
- If the terminal stage is a daemon, the daemon restart happens at the end (same as today).

This logic lives in `PipelineRunner.HandleStageRerun(taskID, runID)` — clean to test in isolation, no daemon-state-machine couplings sprinkled across engine.go.

### What gets deleted

PR-line / lines-of-code reductions (approximate):

| Today | LOC | Status |
|---|---|---|
| `Spec.Trigger.Before []BeforeEntry` field + UnmarshalYAML + MarshalYAML | ~60 | DELETE |
| `runPrereqs` | ~80 | DELETE (replaced by PipelineRunner) |
| `dispatchPipelineStage` | ~80 | DELETE |
| `propagateBeforeRerun` | ~120 | DELETE (replaced by PipelineRunner.HandleStageRerun) |
| `finalizePreflightFailure` | ~70 | DELETE |
| `runOneShotPreflight` branch in `fireAsync` | ~30 | DELETE |
| `validateBeforeRefs` cycle detection + before[0] rejection + ${input.params} rejection | ~60 | DELETE (moves to pipeline validation) |
| `daemonRegistered`-only-for-daemons gate in `runPrereqs` (PR #331's silent-drop fix) | ~5 | MOOT |
| Daemon state's `restartGate` / `propagateBeforeRerun` plumbing | ~40 | SIMPLIFIED |

Total deleted: **~545 LOC**.

### What gets added

| Item | LOC (est.) |
|---|---|
| `pkg/task/pipeline.go` (spec types + YAML decoder + Validate) | ~250 |
| `pkg/trigger/pipeline_runner.go` (PipelineRunner + HandleStageRerun) | ~350 |
| Schema migration for `runs.kind` column + backfill | ~30 |
| `pkg/trigger/engine.go` kind-switch in fireAsync | ~30 |
| WebUI `parent_run_id IS NULL` default filter + drill-in view | ~80 |
| Unit tests for PipelineTask spec + PipelineRunner | ~400 |
| New e2e: `pkg/trigger/e2e_pipeline_task_test.go` | ~250 |

Total added: **~1390 LOC**.

**Net: ~+845 LOC short-term.** But complexity-density goes down because the new code is one cohesive unit instead of branches spread across `fireAsync`, `runPrereqs`, `dispatchPipelineStage`, etc.

### Relationship to `trigger.chain`

PipelineTask does NOT replace `trigger.chain`. They model orthogonal orchestration concerns and continue to coexist:

| Concern | Pipeline | Chain |
|---|---|---|
| Coupling direction | Pipeline declares the sequence; stages are unaware they're in one | Downstream B declares dependency on upstream A; A is unaware |
| Style | Procedural / imperative composition | Event-driven / observer |
| Discoverability | Read one pipeline spec → see the whole flow | Scan task graph for `chain.from: A`; fan-out is implicit |
| Coordination | Single team owns the pipeline file | Team Y reacts to Team X's task without modifying X |
| Cardinality | One pipeline, N stages | One source, M downstream subscribers (natural fan-out) |
| Failure semantics | Stage failure short-circuits the pipeline | `on_failure_chain` lets a separate task react to failure |

#### Cases where chain stays the right tool

1. **Decoupled observability / auditing** — task A runs; audit-task B chains from it. Adding B doesn't require modifying A or every pipeline that includes A. Pipeline-only forces audit into every consumer's stages.
2. **`on_failure_chain`** — A fails → remediation B runs. Pipelines can't model "run this only if the pipeline failed" without adding a separate grammar (deferred). Chain already does this.
3. **Cross-team task coordination** — team X owns task A; team Y wants to fire B when A completes. Chain lets Y add a subscriber without coordinating with X.
4. **Many-to-one event aggregation** — task C reacts when any of {A, B, D} completes. Chain handles this naturally (C has `chain.from` on each).
5. **Webhook-driven fan-out** — webhook fires A; B, C, D each chain from A. With chain, each is independent. With pipelines, you'd wrap B/C/D in a parallel pipeline (deferred) and route the webhook to the wrapper.

#### How they compose

- **Chain can trigger a pipeline.** `kind: PipelineTask, trigger.chain.from: <some-task>` — the pipeline fires when the upstream task completes. The pipeline's first stage receives the upstream's return via the existing `${input.*}` grammar (chain.params, just like today for `kind: Task`).
- **Pipeline can be a chain source.** Another task's `trigger.chain.from: <pipeline-id>` fires when the PipelineTask's parent run terminates. Chain consumer subscribes to the overall pipeline outcome, not individual stages. (Resolves open implementation question #2.)
- **Stage failure can fire `on_failure_chain` independently.** A stage IS a `kind: Task`; if that task has its own `on_failure_chain` configured, the chain fires when the stage fails. The pipeline's overall short-circuit does not suppress the stage's own configured chain.
- **No "pipeline as chain source for individual stage completion."** Chain consumers can only subscribe to the pipeline's overall outcome, not to "stage 2 of pipeline X." This keeps the chain-trigger surface narrow; observers needing per-stage events can chain from the underlying `kind: Task` directly.

### Migration

Two consumer migrations in the same PR (or a follow-up PR if the diff is too big):

1. **`tasks/buildin/relay-server/`**:
   - Rename `task.yaml` → `pipeline.yaml` (kind: PipelineTask).
   - Move `task.ts` + signing-key bootstrap to a new `tasks/buildin/relay-server-body/task.yaml` + `task.ts`.
   - `relay-server-body` is `kind: Task, trigger.daemon: true` — standalone-runnable.
   - The PipelineTask references it as its terminal stage.

2. **`docs/examples/cloudflare-tunnel.md`**:
   - Update the YAML examples to use `kind: PipelineTask`.
   - Same shape as relay-server: pipeline with template + write-local + (cloudflared docker daemon as terminal stage).

### `kind: Task` survives unchanged for non-pipeline users

These tasks stay `kind: Task` and have NO migration:
- All standalone manual / cron / webhook / chain tasks (the majority of buildins).
- `buildin/relay-client` (no preflight, just `trigger.daemon: true`).
- `buildin/template`, `buildin/write-local`, `buildin/local-storage`, etc. (library tasks used AS stages).
- All user task.yaml files that don't currently use `trigger.before`.

The hard cut affects EXACTLY:
- `buildin/relay-server` (1 task — migration in this epic).
- `docs/examples/cloudflare-tunnel.md` (docs — migration in this epic).

That's it. No third-party operator task.yaml in the wild has `trigger.before:` because the feature shipped less than 2 weeks ago and the buildins are the only documented consumers.

## Test strategy

### Unit tests
- `pkg/task/pipeline_test.go` — Spec parsing, validation rules (subtype check, cycle detection, stage[0] `${input.*}` rejection, sequential-only-stages, duplicate-task-id rejection).
- `pkg/trigger/pipeline_runner_test.go` — Sequential dispatch, `${input.*}` flow, failure short-circuit, re-run propagation, daemon-terminal-stage lifecycle.

### Integration tests
- `pkg/trigger/engine_pipeline_test.go` — Engine dispatch via `fireAsync`'s kind-switch, parent_run_id linkage in `runs`, status transitions.

### E2E test
- `pkg/trigger/e2e_pipeline_task_test.go` — Real Deno upstream + downstream + daemon stand-in. Mirrors `e2e_oneshot_preflight_test.go` from PR #312/#331.

### Backwards-compat tests
- `pkg/task/spec_test.go` — A spec with `trigger.before:` should now FAIL at load time with a clear error pointing operators at the migration doc.

## Implementation phases

The work is structured to land in N PRs, each independently mergeable and reviewable:

### PR1 — Schema + spec types (foundation)
- `pkg/task/pipeline.go` with spec types, YAML decoder, Validate.
- Unit tests for the spec layer only (no engine integration).
- Schema migration for `runs.kind` column.
- Net diff: ~300 LOC. Zero behavior change for existing tasks.

### PR2 — PipelineRunner + engine dispatch
- `pkg/trigger/pipeline_runner.go`.
- `fireAsync` kind-switch.
- Stage runs link via `parent_run_id`.
- No re-run propagation yet (placeholder that fires the full pipeline on stage re-run; refined in PR3).
- E2E test for sequential pipeline with real Deno stages.

### PR3 — Re-run propagation + daemon-terminal-stage lifecycle
- `PipelineRunner.HandleStageRerun`.
- Daemon-terminal lifetime subscription.
- Mid-pipeline rerun e2e test.

### PR4 — WebUI default filter + drill-in view
- Runs-list query gains `WHERE parent_run_id IS NULL` for default view.
- "Show children" toggle + drill-in component.
- Updates to dc-task-detail and related components.

### PR5 — Hard cut + buildin migration
- Delete `Spec.Trigger.Before` field and all engine code that depends on it.
- Delete `runPrereqs`, `dispatchPipelineStage`, `propagateBeforeRerun`, `finalizePreflightFailure`, `runOneShotPreflight`.
- Migrate `tasks/buildin/relay-server/` to `pipeline.yaml` + `relay-server-body/task.yaml`.
- Update `docs/examples/cloudflare-tunnel.md`.
- Add load-time error for legacy `trigger.before:` referencing the migration doc.
- Net diff: heavy deletions + 2 task migrations.

### PR6 — Docs (dicode-core)
- `docs/concepts/task-format.md` — new top-level "Pipelines" section, remove the "Preflight pipelines via trigger.before" section (per hard cut).
- `docs/examples/cloudflare-tunnel.md` — already done in PR5.
- Short migration guide for any operator with a `trigger.before:` task.yaml in flight: how to convert to `kind: PipelineTask` + separate daemon-body Task.

### PR7 — dicode-site sync (dicode-ayo/dicode-site)
Lands after PR6 merges. Pattern mirrors site#58 (the preflight-pipelines epic's site sync):

- `docs-src/concepts/triggers.md` / `tasks.md` — pull the new "Pipelines" section from dicode-core's task-format.md. Remove the "Preflight pipelines via trigger.before" subsection.
- `docs-src/concepts/` — add a new top-level concept page if the Pipelines section is large enough to warrant standalone navigation. Decide during the sync; bias toward keeping it inline unless the doc grows past ~200 lines.
- `site/src/components/` — audit the landing-page components for any copy that promises the OLD shape ("preflight before daemons", `trigger.before`). Update if found; skip if the landing's existing narrative is general enough.
- `docs/examples/cloudflare-tunnel.md` — pull updated example from dicode-core.
- Build + `npm run build` verification per the existing dicode-site pipeline.

PR7 cannot land before PR6 (cross-repo dep). File it as a follow-up issue with `blocked: waiting on dicode-core PR6` until PR6 merges.

## Risks

### Schema migration on existing databases
`runs.kind` column needs a clean ALTER TABLE migration. Existing rows backfilled to `kind = 'task'`. Migration is one-way (no rollback path needed) since the column has a sensible default.

### Behavior change for daemon restart on terminal stage crash
Today: daemon-with-preflight crashes → daemon-state transitions to `crashed` (per PR #329). Tomorrow: PipelineTask's terminal-stage daemon crashes → PipelineTask's run goes to `failure`. Operators looking at the WebUI see "PipelineTask failed" rather than "daemon crashed." Semantically equivalent; visually different. Mitigated by the drill-in view (operator clicks the failed PipelineTask, sees the daemon stage's run with `crashed` status).

### Engine code review surface
PR2 (PipelineRunner) is the largest engine change since PR #311. Needs careful review for:
- Goroutine lifecycle (PipelineRunner is one goroutine per fire).
- Context propagation + shutdown handling (same patterns as PR #311's `dispatchPipelineStage`).
- Concurrent fires of the same pipeline (no coalescing — each fire gets its own runner; matches one-shot semantics from #312).

### Documentation churn
The doc reshape (PR6) follows recently-shipped docs from PRs #311 / #327 / #336 / #337 etc. Doc reviewers will see a lot of churn in `task-format.md`. Mitigated by structuring the new section clearly and removing the old one in a single commit (visible diff = signal-rich).

## Open implementation questions

To resolve during planning:

1. **Stage's `task:` field — can it be a fully-qualified path (e.g., `buildin/template`) or just a task name?**
   Today's `BeforeEntry.Task` accepts the fully-qualified registry ID. PipelineTask should match.

2. **`return_value` storage for the parent run.**
   Sequential pipeline's parent run's `return_value` = terminal stage's `return_value`. Implementation: PipelineRunner reads the terminal stage's run row at completion and copies the value up. Same persistence semantics (`run_result.enabled` on the terminal stage propagates).

3. **`run_inputs.enabled` for the parent PipelineTask run.**
   Parent run has no params of its own (Trigger.Manual fire-time params? Cron schedule? Webhook payload?). Probably reuse the trigger's payload (manual params, webhook body) as the parent's `run_inputs`. Stage runs have their own inputs as today.

4. **Timeout enforcement.**
   `PipelineTask.Timeout` is the wall-clock budget for the entire pipeline. Each stage has its own `overrides.timeout`. When the pipeline timeout fires mid-stage: cancel the current stage, mark pipeline `timeout`. Implementation: PipelineRunner runs under a derived context with deadline.

## Out of scope (separate follow-up issues to file post-spec-approval)

- `subtype: parallel` — fan-out grammar (likely renames / supersedes #315).
- `if` / `when` / `continue_on_failure` per stage.
- Stage-level `id:` field for duplicate-task-id disambiguation.
- Pipeline-of-pipeline nesting at v1 (the fan-out case unlocks this).
- Webhook payload as `${input.*}` source on first stage.
- Chain-trigger-fires-pipeline + chain-input-as-first-stage-input.

## Spec ownership

The spec lives at `docs/superpowers/specs/2026-05-18-pipeline-task-design.md`. The implementation plan will be written by the writing-plans skill after spec approval.
