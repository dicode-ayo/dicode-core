# On-failure auto-fix loop — design

**Status:** draft, awaiting review
**Owner:** TBD
**Epic:** [#207 — landing-page promises](https://github.com/dicode-ayo/dicode-core/issues/207)

## 1. Why

The dicode landing page advertises a continuous AI loop — *create, validate, deploy, monitor, **fix*** — with the headline scenario *"API down → AI diagnoses → auto-fix deployed"* and the guardrail dial *"Require review before deploy, or run fully autonomous."* Today none of the fix-side machinery exists end-to-end:

- An `on_failure_chain` mechanism fires a target task on failure, but the chained task only sees the failed run's `output`. It cannot see the original input that triggered the failure, the source code that produced the failure, or the run logs.
- Run inputs are not persisted (the `runs` table has no `input` column). A failed webhook payload is gone after the run ends, so no agent can replay it once a fix is proposed.
- `commit_task` and dev-mode workflow tools exist for *humans* iterating in the WebUI dev panel, but they are not wired to a chain trigger and there is no headless agent loop that replicates the dev workflow on a failure.

This spec closes those gaps with the smallest set of additions, reusing existing primitives (dev mode, MCP dev tools, `buildin/dicodai`, the `temp-cleanup` cron pattern, the `from: task:` extensibility shape used for secret providers).

## 2. Scope

In scope:

1. Persisted, encrypted run inputs with parameterizable retention and pluggable storage.
2. Replay primitive that re-fires a failed run with its persisted input.
3. Dev mode extension that lets the engine clone-and-checkout a worktree on a named branch, so an agent can iterate in isolation without touching the production source.
4. Chain trigger that can pass parameters to the chained task.
5. A buildin `auto-fix` task (preset of `buildin/dicodai`) that consumes the failure context, iterates fix → validate → push, and either merges to main (autonomous) or opens a PR (review).
6. Loop guardrails: per-failure iteration cap, token budget, cooldown, concurrency cap, depth limit, failure-storm circuit breaker.

Out of scope (deferred):

- Multi-task fixes (agent edits a dependency task, not the failing task itself). v1 limits the agent to writing inside the failed task's directory.
- Forge integrations beyond GitHub. The PR step is implemented as a buildin `git-pr` task using the `gh` CLI; users replace it with their own task to support GitLab/Gitea/Forgejo.
- Auto-revert on monitoring signals from a *successful* deploy (the LP "When AI is wrong" copy). v1 covers the on-failure path only; revert is a follow-up.

## 3. Architecture

### 3.1 Component map

```
┌─────────────────┐  failure   ┌────────────────────┐
│  trigger engine │──────────▶│ on_failure_chain    │
│  (pkg/trigger)  │            │ (params merge)     │
└────────┬────────┘            └─────────┬──────────┘
         │ persist input                 │ fires
         ▼                               ▼
┌─────────────────┐            ┌────────────────────┐
│ run-input store │            │ buildin/auto-fix    │
│  (AES-GCM core, │            │ (dicodai preset)   │
│   storage task) │            │                    │
└────────┬────────┘            │  uses MCP tools:   │
         ▲                     │   switch_dev_mode  │
         │ replay              │   write_task_file  │
         │                     │   validate_task    │
         │                     │   test_task        │
         │                     │   dry_run_task     │
         │                     │   replay_run       │
         │                     │   commit_task      │
         │                     │  uses task tools:  │
         │                     │   buildin/git-pr   │
         │                     └────────────────────┘
         │
┌────────┴────────┐
│ run-inputs-     │  cron     ┌────────────────────┐
│ cleanup task    │──────────▶│ storage task        │
│ (buildin)       │  delete   │  put | get | delete │
└─────────────────┘            └────────────────────┘
```

### 3.2 Existing primitives reused (not in scope to change)

- `pkg/taskset.SetDevMode(enabled, opts)` — dev-ref substitution + immediate registry re-sync. Used today for human dev workflow.
- `buildin/dicodai` — `ai-agent` preset preloaded with `dicode-task-dev` + `dicode-basics` skills.
- MCP tools `validate_task`, `test_task`, `dry_run_task`, `commit_task`, `write_task_file`, `switch_dev_mode` — the agent's existing dev surface.
- `pkg/secrets` AES master key — reused (via HKDF sub-key) for run-input encryption.
- `temp-cleanup` cron pattern — replicated for run-input retention.
- "Delegate I/O to a swappable task" pattern — same shape as the secret-provider design (§ runner-internal). Here it is exposed as a config field (`defaults.run_inputs.storage_task`, `params.pr_task`) pointing at any task that satisfies the contract, not as a `from: task:` env-injection prefix.

## 4. Detailed design

### 4.1 Persisted run inputs

**Schema delta on `runs` table** ([pkg/db/sqlite.go:69-76](../../pkg/db/sqlite.go#L69-L76)):

```sql
ALTER TABLE runs ADD COLUMN input_storage_key TEXT;        -- handle returned by storage task
ALTER TABLE runs ADD COLUMN input_size INTEGER;            -- ciphertext size, for diagnostics + GC
ALTER TABLE runs ADD COLUMN input_stored_at INTEGER;       -- unix seconds
ALTER TABLE runs ADD COLUMN input_pinned INTEGER NOT NULL DEFAULT 0; -- 1 = in-flight auto-fix references this; do not GC
```

No plaintext column. The storage task holds the only copy of the encrypted blob.

**Encryption.** AES-256-GCM. Key derived from `pkg/secrets` master key via HKDF with context string `"dicode/run-inputs/v1"`. Per-row 12-byte random nonce. On-disk blob layout:

```
[12B nonce][N bytes ciphertext][16B GCM tag]
```

**Write path.** When a run starts, the engine collects the structured input it would normally pass to the runtime:

```go
type PersistedInput struct {
    Source  string                 `json:"source"`  // webhook | cron | manual | chain | daemon
    Headers map[string]string      `json:"headers,omitempty"` // webhook only, post-redaction
    Body    json.RawMessage        `json:"body,omitempty"`    // webhook only, post-redaction
    Query   map[string]string      `json:"query,omitempty"`   // webhook only
    Params  map[string]any         `json:"params,omitempty"`  // manual / SDK / chain
}
```

Before encryption the engine walks the JSON and zeroes out values for keys matching a built-in deny-list (case-insensitive substring match):

```
authorization, cookie, set-cookie,
x-*-signature*, x-*-token*, x-*-key*, x-api-key,
password, passphrase, api_key, apikey, token, secret
```

The deny-list is a Go constant in `pkg/registry/inputs.go`. Users who need stricter redaction set `persist_inputs: false` per-task to disable persistence entirely. Users who need looser redaction must accept the deny-list as is — there is no allow-override in v1, on the grounds that an "allow this header that looks like a secret" knob is a footgun.

**Storage task contract** (3 ops, no `list`):

```yaml
# tasks/buildin/local-storage/task.yaml — default backend
apiVersion: dicode/v1
kind: Task
name: "Local Storage (run inputs)"
runtime: deno
params:
  op:    { type: string, required: true }   # put | get | delete
  key:   { type: string, required: true }   # opaque, format chosen by core
  value: { type: string, default: "" }      # base64(blob); put only
permissions:
  fs:
    - path: "${DICODE_DATA_DIR}/run-inputs"
      permission: rw
timeout: 30s
notify:
  on_success: false
  on_failure: true
```

Returns `{ ok: true, value?: "<base64>" }` on success, `{ ok: false, error: "..." }` on failure. Core is responsible for retry on transient failure; storage task itself is stateless across calls.

**Configuration:**

```yaml
# dicode.yaml
defaults:
  run_inputs:
    enabled: true            # default: true; set false to disable persistence globally
    retention: 30d           # global default; per-task override via task.yaml run_inputs.retention
    storage_task: local-storage   # any task ID implementing the contract
```

Per-task override:

```yaml
# task.yaml
run_inputs:
  enabled: false             # opt this task out (e.g. tasks handling Stripe webhooks)
  retention: 7d              # tighter retention than global
```

**Read path** (used by `replay_run` and the auto-fix agent's prompt builder):

1. Look up `input_storage_key` for `runID` in `runs` table.
2. Call configured storage task with `op: get, key: <stored_key>`.
3. Base64-decode, split nonce/ciphertext/tag, AES-GCM decrypt.
4. Return as structured `PersistedInput`.

If the input has been GC'd or never stored, return a typed `ErrInputUnavailable`. Callers (the auto-fix agent and `replay_run`) must handle this gracefully — typically by aborting the loop with a clear message rather than retrying.

### 4.2 Retention via cleanup task

A new buildin, modeled exactly on [`buildin/temp-cleanup`](../../tasks/buildin/temp-cleanup/task.yaml):

```yaml
# tasks/buildin/run-inputs-cleanup/task.yaml
apiVersion: dicode/v1
kind: Task
name: "Run-input retention sweep"
runtime: deno
trigger:
  cron: "17 * * * *"   # hourly, off the hour
permissions:
  dicode:
    runs_list_expired: true      # new SDK call
    runs_delete_input: true      # new SDK call
timeout: 120s
notify:
  on_success: false
  on_failure: true
```

Logic: `dicode.runs.list_expired({exclude_pinned: true})` returns runIDs whose `input_stored_at + retention < now`. For each, call `dicode.runs.delete_input(runID)` — core looks up the storage key, calls the configured storage task's `delete` op, clears the row's input columns. Pinned rows (referenced by an in-flight auto-fix run) are skipped; the auto-fix run is responsible for unpinning when it terminates.

### 4.3 Replay primitive

**SDK / MCP tool:**

```ts
replay_run(runID: string, task_name?: string): { run_id: string }
```

Behavior:

1. Resolve original run; require non-empty `input_storage_key`.
2. Decrypt input via the read path.
3. Resolve target task: `task_name` if provided, else original run's task.
4. Fire as a *new* run with `parent_run_id = original.id`, `triggerSource = "replay"`. The new run's input is itself persisted (same retention rules) so a replay-of-a-replay works.
5. Return the new run ID synchronously; the run executes asynchronously.

`task_name` is the only knob and is optional. The branch is *not* a parameter — the live source's resolution (with or without dev mode) decides what code runs. This is intentional: replay always runs *the currently resolved version of the task*, which is exactly what the auto-fix loop wants (it has dev mode pointing at the fix worktree).

REST mirror: `POST /api/runs/:id/replay` with `{ "task_name": "..." }`.

### 4.4 Dev mode with branch parameter

Today: `Source.SetDevMode(ctx, enabled bool, localPath string)` ([pkg/taskset/source.go:124](../../pkg/taskset/source.go#L124)).

Extended:

```go
func (s *Source) SetDevMode(ctx context.Context, enabled bool, opts DevModeOpts) error

type DevModeOpts struct {
    LocalPath string  // existing: point at user's local checkout
    Branch    string  // new: engine clones a worktree on this branch
    Base      string  // new: branch to fork from when Branch doesn't exist (default: source's tracked branch)
}
```

Engine behavior when `enabled=true, Branch != ""`:

1. Resolve the source's git URL (must be a git source; error if local-only).
2. Compute worktree path: `<data_dir>/dev-worktrees/<source>/<branch>`.
3. If worktree doesn't exist: `git worktree add <path> <branch>`. If branch doesn't exist on remote, create it from `Base` (default = the source's tracked branch).
4. Set `s.devRootPath = <path>/<root entry yaml>`, `s.resolver.SetDevMode(true)`, trigger immediate sync.

Engine behavior when `enabled=false` *and* the source was previously in worktree-mode:

1. Disable dev-ref substitution (existing behavior).
2. Run `git worktree prune` and `git worktree remove --force <path>` on the worktree dir.
3. The branch ref itself is *retained* (commits made by the agent live on disk and on remote after push).

Concurrency: at most one dev-mode-with-branch session per source at a time. A second `SetDevMode(enabled=true, Branch=...)` on a source that's already in worktree-mode returns `ErrDevModeBusy`. The auto-fix engine serialises via the per-task `max_concurrent` guard (§ 4.6).

REST mirror: `PATCH /sources/{name}/dev` body extended:

```json
{ "enabled": true, "branch": "fix/abc-123", "base": "main" }
```

MCP tool `switch_dev_mode` arguments extended likewise.

### 4.5 Chain trigger with parameter passing

Today `defaults.on_failure_chain` is a string (task ID). Extended to accept a structured form:

```yaml
defaults:
  on_failure_chain:
    task: auto-fix
    params:
      mode: review
      max_iterations: 5
```

Backwards-compatibility: bare string remains valid, equivalent to `{ task: <string>, params: {} }`.

Same shape on per-task override:

```yaml
# task.yaml
on_failure_chain:
  task: auto-fix
  params:
    mode: autonomous   # this task is non-critical, fix and ship without review
```

Chain payload merge in [pkg/trigger/engine.go:670-677](../../pkg/trigger/engine.go#L670-L677):

```go
input := map[string]any{
    "taskID": completedTaskID,
    "runID":  runID,
    "status": runStatus,
    "output": output,
}
for k, v := range chain.Params {
    if _, exists := input[k]; !exists {  // never overwrite the four reserved keys
        input[k] = v
    }
}
```

Reserved keys (`taskID`, `runID`, `status`, `output`) are not overridable by user params. A param named the same is silently ignored — and a validation step at config-load time emits a warning.

The auto-fix task reads its `mode` (and other guardrails) from `params` like any normal task.

Note on user composition: this lets users define **two `task.yaml` entries** that wrap `auto-fix` with different param defaults — same pattern as `tasks/examples/ai-agent-{openai,ollama,groq}`. Their `dicode.yaml` then references whichever wrapper they want as the chain target. The buildin ships the canonical `auto-fix` task; user-authored `auto-fix-strict`, `auto-fix-conservative`, etc. are taskset-level overrides.

### 4.6 Auto-fix task

**File:** `tasks/buildin/auto-fix/task.yaml`. Preset of `buildin/dicodai`. Trigger is `chain` (no webhook).

```yaml
apiVersion: dicode/v1
kind: Task
name: "Auto-fix on failure"
description: |
  Diagnoses a failed task run, edits source on a worktree, validates via
  task tests + replay, and either merges to main (autonomous) or opens a
  PR (review). Wired by setting on_failure_chain to this task ID.
extends: dicodai
trigger:
  chain:
    on: failure                 # accepts taskID/runID/status/output + merged params
params:
  mode:
    type: string
    default: "review"           # review | autonomous
  max_iterations:
    type: number
    default: 5
  max_tokens:
    type: number
    default: 50000              # passed to the underlying ai-agent's response_max_tokens × budget
  branch_prefix:
    type: string
    default: "fix/"
  pr_task:
    type: string
    default: "git-pr"           # buildin task implementing the PR contract; replace for non-GitHub forges
  base_branch:
    type: string
    default: ""                 # empty = source's tracked branch
permissions:
  dicode:
    list_tasks: true
    get_runs: true
    runs_get_input: true
    runs_replay: true
    sources_set_dev_mode: true
    tasks:
      - "validate_task"
      - "test_task"
      - "dry_run_task"
      - "commit_task"
      - "write_task_file"
      - "${param.pr_task}"      # the PR task is callable as a tool
timeout: 1800s                  # 30 min hard cap
notify:
  on_success: true              # operator wants to know when an auto-fix lands
  on_failure: true
```

The task's `task.ts` is a small driver that:

1. Reads chain payload: `{ taskID, runID, status, output, mode, ... }`.
2. Pins the failed run's input (`dicode.runs.pin_input(runID)`).
3. Generates fix branch name: autonomous mode → `base_branch || sourceTrackedBranch`; review mode → `${branch_prefix}${runID}`.
4. Calls `switch_dev_mode(source=<failed task's source>, enabled=true, branch=<fixBranch>, base=<base>)`.
5. Builds the agent prompt:
   - Failure context: task ID, run ID, status, exit code, last 200 log lines, persisted input (from `runs_get_input` — internal SDK only, not exposed as MCP tool).
   - Source context: failing task's `task.yaml` and source files (read by the agent via `read_task_file` if it exists, else inlined into prompt).
   - Instructions: follow the `dicode-task-dev` workflow, validate via `task.test.*` + `replay_run(runID)`, commit when both green.
6. Invokes the underlying `dicodai` agent loop (this is the existing `ai-agent` machinery — no new code).
7. Loop terminator conditions:
   - **Success:** validate + test + dry_run + replay all green. Agent calls `commit_task`. Driver pushes worktree. If `mode == "review"`, driver invokes `pr_task` with branch + body. Driver disables dev mode.
   - **Iteration cap:** `max_iterations` reached without success. Driver leaves the worktree branch on disk, opens a PR titled `[auto-fix WIP] runID=...` with a "needs human" body, disables dev mode.
   - **Token budget:** same as iteration cap.
   - **Hard error:** infrastructure error (decrypt fail, push fail, dev-mode error). Driver disables dev mode (cleanup), unpins input, exits with failure — which itself triggers `on_failure` notification but **not** another chain (chain depth check, § 4.7).
8. Unpins the failed run's input on exit (success or failure).

### 4.7 Loop guardrails (engine-level)

| Guard | Default | Where enforced | Override |
|---|---|---|---|
| Max fix iterations per run | `5` | auto-fix task driver | `params.max_iterations` |
| LLM token budget per run | `50_000` | auto-fix task driver | `params.max_tokens` |
| Cooldown after auto-fix runs (per failing task) | `10m` | trigger engine | `defaults.on_failure_chain.cooldown`; per-task override |
| Concurrent auto-fixes per failing task | `1` | trigger engine | `defaults.on_failure_chain.max_concurrent` |
| Concurrent auto-fixes globally | `3` | trigger engine | `defaults.on_failure_chain.max_concurrent_global` |
| Chain depth | max `2` | trigger engine — input carries `_chain_depth`, increments on each chain fire, refuse to fire if ≥ limit | `defaults.on_failure_chain.max_depth` |
| Failure storm circuit breaker | trip if > `10` chain fires within `1m`; suppress for `30m`; emit notification | trigger engine | `defaults.on_failure_chain.storm.{rate, suppress}` |

Cooldown semantics: if task X fails at T0 and auto-fix fires, a second failure of X at T0+5m does **not** fire auto-fix. The notification still fires per the existing `on_failure` flag. The cooldown's purpose is to prevent fix-loops where the auto-fix push triggers the same task and it fails again instantly.

The chain-depth check is recorded on the chained run's persisted input (alongside the user params), so a chain of `A → auto-fix → A → auto-fix` is detected by the second auto-fix invocation, which sees `_chain_depth = 2` and refuses. Reserved key `_chain_depth` (underscore prefix) avoids user-param collision.

### 4.8 Git-PR task (review mode only)

```yaml
# tasks/buildin/git-pr/task.yaml — reference implementation
apiVersion: dicode/v1
kind: Task
name: "Open Pull Request"
runtime: deno
params:
  source_id: { type: string, required: true }
  branch:    { type: string, required: true }
  base:      { type: string, default: "main" }
  title:     { type: string, required: true }
  body:      { type: string, default: "" }
permissions:
  run: ["gh"]                   # shells to gh CLI
  env:
    - GH_TOKEN
  fs:
    - path: "${DICODE_DATA_DIR}/dev-worktrees"
      permission: r              # read worktree path to resolve cwd
timeout: 60s
```

Returns `{ ok: true, url: "..." }` or `{ ok: false, error: "..." }`. Replaceable by users with their own task targeting GitLab/Gitea/Forgejo/Bitbucket.

## 5. Data flow walkthrough — review mode

A webhook task `process-payment` fails with a 500 from a downstream API.

1. **t=0:** webhook arrives. Engine: persist input → AES-GCM blob → call `local-storage` task `op=put, key=run-inputs/<runID>` → store key + size + timestamp on the new run row.
2. **t=0+1s:** task runs, fails. `FireChain` checks `defaults.on_failure_chain` — set to `{ task: auto-fix, params: { mode: review } }`.
3. Engine fires `auto-fix` with input `{ taskID: "process-payment", runID: "<id>", status: "failure", output: <500 body>, mode: "review", _chain_depth: 1 }`.
4. **t=0+2s:** `auto-fix` driver pins input, picks branch `fix/<runID>`, calls `switch_dev_mode("user-tasks", enabled=true, branch="fix/<runID>", base="main")`.
5. Engine: `git worktree add /var/lib/dicode/dev-worktrees/user-tasks/fix-<runID> fix/<runID>` (created from main since branch didn't exist), resolver swaps source root, registry reloads tasks from worktree.
6. **t=0+5s:** driver builds prompt with failure context, hands off to `dicodai`. Agent reads logs, sees "500 from upstream", proposes adding a retry-with-backoff. Calls `write_task_file(process-payment/task.ts, ...)` which writes to the worktree.
7. Agent calls `validate_task("process-payment")` — passes.
8. Agent calls `test_task("process-payment")` — sees no test; writes `task.test.ts` with a regression test for the 500 case using a mocked fetch; reruns — passes.
9. Agent calls `replay_run("<id>")` — engine decrypts the original webhook body, fires a new run on the worktree code, this time with retry — passes.
10. Agent calls `commit_task("process-payment", "user-tasks")` — engine commits the `process-payment/` directory on the worktree branch and pushes `origin/fix/<runID>`.
11. Driver invokes `pr_task` (`buildin/git-pr`) with `branch=fix/<runID>, base=main, title="auto-fix: process-payment retry on 5xx", body=<failure summary + change rationale>`. `gh pr create` returns URL.
12. Driver calls `switch_dev_mode("user-tasks", enabled=false)`. Engine removes worktree, source resumes pulling main.
13. Driver unpins the failed run's input, returns `{ ok: true, pr_url: "..." }`.
14. Operator gets notification "auto-fix opened PR <url> for process-payment runID=...". Reviews and merges. Reconciler picks up the change on the next poll.

## 6. Decomposition into child issues

Filed under epic [#207](https://github.com/dicode-ayo/dicode-core/issues/207):

1. **Run-input persistence + storage task contract** — schema, encryption, deny-list redaction, `local-storage` buildin, retention sweep buildin. SDK additions: `runs.list_expired`, `runs.delete_input`, `runs.pin_input`, `runs.unpin_input`, `runs.get_input` (internal).
2. **Replay primitive** — `replay_run(runID, task_name?)` IPC + REST + MCP. Depends on (1).
3. **Dev mode with branch + chain params** — extend `SetDevMode` to support `Branch` + worktree lifecycle; extend `on_failure_chain` to accept structured `{ task, params }` form. Independent of (1) and (2).
4. **Auto-fix buildin + git-pr buildin + guardrails** — the agent driver, the PR helper, the engine-side guardrails (cooldown, concurrency, chain depth, storm circuit breaker). Depends on (1), (2), (3).

Each child issue is independently mergeable: (1) ships persistence with no consumers; (2) ships replay usable from the WebUI; (3) ships dev-mode-on-a-branch usable by humans; (4) finally wires the loop. Recommend shipping in this order so v0.2.0 can include (1)+(2)+(3) and the auto-fix loop arrives in v0.3.0 once each foundation block is hardened.

## 7. Open questions / explicitly deferred

- **Auto-revert on monitoring signals from a successful deploy.** The LP "When AI is wrong" copy implies that a *successful* deploy that subsequently breaks production is also auto-fixed. v1 covers the on-failure path only; revert-on-regression is a follow-up under the same epic.
- **Multi-task fixes.** v1 restricts the agent to writing inside the failed task's directory. A failure rooted in a *dependency* task is detectable by the agent (it can read other tasks' source) but the fix would have to be outside its write scope. v1 punts this with a clear error from `write_task_file` if the path escapes the failing task's dir.
- **Forge auth UX.** `git-pr` ships with `GH_TOKEN` env. A first-launch onboarding step that prompts for the GH token (similar to the OpenAI key prompt) is desirable but out of scope here.
- **WebUI surfacing.** The fix worktree's branch and the in-flight auto-fix's progress should appear in the WebUI runs view. Concrete UI design is deferred to a follow-up site/webui PR.
- **Cost telemetry.** Token spend per auto-fix should be aggregated and surfaced. Deferred — first ship the loop, then add accounting.

## 8. Security considerations

- Run-input encryption uses an HKDF sub-key, so a leak of the run-input key does not compromise the secrets table's key (and vice versa).
- Header redaction is deny-list only. A user storing webhook bodies that themselves carry credentials in non-standard fields (e.g. JSON `{"my_secret": ...}`) gets no redaction — the deny-list scans key names, not value entropy. Documented as a limitation; users with sensitive payloads set `persist_inputs: false`.
- The agent runs with `dicode.list_tasks` and access to a curated tool list. It cannot escalate to filesystem or network access beyond what its underlying `dicodai` task already has. The fix worktree is sandboxed by the runtime's existing permission model.
- Autonomous mode pushes directly to the source's tracked branch. Users opting in (`mode: autonomous`) accept that an LLM can land code on main without human review. The default in the buildin's `task.yaml` is `review`, so the only path to autonomous is explicit user opt-in via `params.mode`.
- The `git-pr` task uses `permissions.run: [gh]`, which is a deliberate widening of the runtime's default deny-all `run` policy. Documented in the task description.

## 9. Landing-page mapping

| LP claim | Spec section |
|---|---|
| "API down → AI diagnoses → auto-fix deployed" | § 5 (full walkthrough) |
| "Every step is a git commit you can review" | § 4.6 step 7 (commit_task push), § 4.8 (PR step) |
| "Require review before deploy, or run fully autonomous" | § 4.6 `params.mode` |
| "Auto-revert on failure" | Deferred — § 7 |
| "When AI is wrong → the monitor catches it → git revert is one command away" | § 4.6 step 7c (iteration cap → WIP PR with "needs human" body); revert deferred § 7 |
| "Continuous loop — create, validate, deploy, monitor, **fix**" | This spec covers the *fix* arc; the rest already exist. |
