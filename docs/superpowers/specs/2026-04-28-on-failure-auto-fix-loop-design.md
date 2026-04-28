# On-failure auto-fix loop — design

**Status:** draft, awaiting review
**Owner:** TBD
**Epic:** [#207 — landing-page promises](https://github.com/dicode-ayo/dicode-core/issues/207)
**Tracking issue:** [#228](https://github.com/dicode-ayo/dicode-core/issues/228)

## 1. Why

The dicode landing page advertises a continuous AI loop — *create, validate, deploy, monitor, **fix*** — with the headline scenario *"API down → AI diagnoses → auto-fix deployed"* and the guardrail dial *"Require review before deploy, or run fully autonomous."* Today none of the fix-side machinery exists end-to-end:

- An `on_failure_chain` mechanism fires a target task on failure, but the chained task only sees the failed run's `output`. It cannot see the original input that triggered the failure, the source code that produced the failure, or the run logs.
- Run inputs are not persisted (the `runs` table has no `input` column). A failed webhook payload is gone after the run ends, so no agent can replay it once a fix is proposed.
- The dicode SDK exposes `list_tasks` / `run_task` / `kv` / `log` / `output` to tasks but lacks calls for testing a task, toggling dev mode on a source, or committing changes back to a source. These exist as Go-internal APIs and REST endpoints today; an internal agent task cannot reach them.

This spec closes those gaps with the smallest set of additions, reusing existing primitives (dev mode, `buildin/dicodai`, the `temp-cleanup` cron pattern, the buildin TaskSet `overrides:` mechanism that defines `dicodai` itself).

## 2. Scope

In scope:

1. Persisted, encrypted run inputs with parameterizable retention and pluggable storage.
2. Replay primitive that re-fires a failed run with its persisted input.
3. Dev mode extension that lets the engine clone-and-checkout a worktree on a named branch.
4. Chain trigger that can pass parameters to the chained task.
5. SDK additions exposing what auto-fix needs over the unix-socket IPC: `tasks.test`, `sources.set_dev_mode`, `git.commit_push`, plus the run-input plumbing (replay, get_input, list_expired, delete_input, pin/unpin).
6. A buildin `auto-fix` taskset entry (override of `buildin/dicodai`) that consumes the failure context, iterates fix → validate → push, and either merges to main (autonomous) or opens a PR (review).
7. Loop guardrails: per-failure iteration cap, per-iteration timeout, LLM token budget, cooldown, concurrency cap, depth limit, failure-storm circuit breaker, autonomous-mode opt-in protections.

Out of scope (deferred):

- **Extending the buildin MCP task with `write_task_file` / `validate_task` / `commit_task` / `dry_run_task` / `read_task_file`** to close the gap with the documented `dicode-task-dev` skill. Useful for *external* agents (Claude Code, Cursor) but unnecessary for auto-fix because auto-fix is an internal task with direct SDK access. Tracked separately under epic #207.
- Auto-revert on monitoring signals from a *successful* deploy (the LP "When AI is wrong" copy). v1 covers the on-failure path only; revert-on-regression is a follow-up.
- Multi-task fixes (agent edits a dependency task, not the failing task itself). v1 limits the agent to writing inside the failed task's directory.
- Forge integrations beyond GitHub. The PR step is implemented as a buildin `git-pr` task using the `gh` CLI; users replace it with their own task to support GitLab/Gitea/Forgejo.

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
│  (ChaCha20-Poly,│            │ (dicodai override)  │
│   storage task) │            │                    │
└────────┬────────┘            │  uses dicode SDK:   │
         ▲                     │   runs.replay       │
         │ replay              │   runs.get_input    │
         │                     │   tasks.test        │
         │                     │   sources.set_dev_mode│
         │                     │   git.commit_push    │
         │                     │  uses Deno fs:       │
         │                     │   readTextFile       │
         │                     │   writeTextFile      │
         │                     │  uses task tools:    │
         │                     │   buildin/git-pr     │
         │                     └────────────────────┘
         │
┌────────┴────────┐  cron     ┌────────────────────┐
│ run-inputs-     │──────────▶│ storage task        │
│ cleanup task    │  delete   │  put | get | delete │
│ (buildin)       │            └────────────────────┘
└─────────────────┘

┌────────────────────┐
│ dev-worktrees-     │  cron, mirrors temp-cleanup
│ cleanup task       │  removes orphan worktrees
│ (buildin)          │  whose runID is no longer
└────────────────────┘  pinned in `runs`
```

### 3.2 Existing primitives reused (not in scope to change)

- **`pkg/taskset.SetDevMode(enabled, opts)`** — dev-ref substitution + immediate registry re-sync. Used today for human dev workflow ([source.go:121-147](../../pkg/taskset/source.go#L121-L147)).
- **`buildin/dicodai`** — a TaskSet `overrides:` entry defined in [`tasks/buildin/taskset.yaml:32-`](../../tasks/buildin/taskset.yaml#L32) that overrides `ai-agent` with the `dicode-task-dev` + `dicode-basics` skills preloaded.
- **`pkg/secrets/local.go`** ChaCha20-Poly1305 + Argon2id — reused (with a per-purpose Argon2id salt) for run-input encryption. See § 4.1.
- **`pkg/tasktest.Run`** — runs `task.test.{ts,js}` via `pkg/tasktest`. Wrapped by a new `dicode.tasks.test` SDK call.
- **`pkg/source/git`** + go-git — used for clone/worktree/commit/push. Wrapped by a new `dicode.git.commit_push` SDK call. Honours dicode's "no git binary" constraint ([CLAUDE.md key constraints](../../CLAUDE.md)).
- **`temp-cleanup` cron pattern** — replicated for both run-input retention and dev-worktree orphan cleanup.
- **"Delegate I/O to a swappable task" pattern** — same shape as the secret-provider design (separately tracked). Here it is exposed as a config field (`defaults.run_inputs.storage_task`, `params.pr_task`) pointing at any task that satisfies the contract.

### 3.3 What MCP is — and is not — used for here

The buildin MCP task ([`tasks/buildin/mcp/task.ts`](../../tasks/buildin/mcp/task.ts)) is a JSON-RPC wrapper exposed to *external* MCP clients (Claude Code, Cursor). It currently exposes 6 tools, three of which are hints redirecting the client to the REST API (`list_sources`, `switch_dev_mode`, `test_task`).

The auto-fix task is an *internal* dicode task running in the same Deno sandbox the MCP task does, with the same SDK surface available over the unix-socket IPC. It does **not** route through MCP. It calls the dicode SDK directly. The MCP-tool extensions documented in the `dicode-task-dev` skill (`write_task_file`, `validate_task`, `commit_task`, `dry_run_task`, `read_task_file`) are useful *for external agents*, are tracked as a separate child of epic #207, and are not a prerequisite for auto-fix.

## 4. Detailed design

### 4.1 Persisted run inputs

#### 4.1.1 Schema delta on `runs` table

[pkg/db/sqlite.go:69-76](../../pkg/db/sqlite.go#L69-L76):

```sql
ALTER TABLE runs ADD COLUMN input_storage_key TEXT;        -- handle returned by storage task
ALTER TABLE runs ADD COLUMN input_size INTEGER;            -- ciphertext size
ALTER TABLE runs ADD COLUMN input_stored_at INTEGER;       -- unix seconds, for retention
ALTER TABLE runs ADD COLUMN input_pinned INTEGER NOT NULL DEFAULT 0; -- non-zero = referenced by in-flight auto-fix
ALTER TABLE runs ADD COLUMN input_redacted_fields TEXT;    -- JSON array of dotted paths that were redacted
```

This is the **first** use of `ALTER TABLE` in the dicode schema (today's migrations are all `CREATE TABLE IF NOT EXISTS` at init time). Implementation must:

1. Use `ALTER TABLE … ADD COLUMN IF NOT EXISTS` (modernc/sqlite supports this in current versions; verify and fall back to `PRAGMA table_info` introspection if not).
2. Run after the existing `CREATE TABLE IF NOT EXISTS` block in `migrate()`.
3. Be idempotent so a downgrade-then-upgrade cycle works.

This is documented as the introduction of dicode's first migration step. A real migration framework can wait until the second migration arrives.

#### 4.1.2 Encryption

**Cipher: XChaCha20-Poly1305** (matches `pkg/secrets/local.go` for consistency; we derive a separate key, not a separate cipher).

**Key derivation:** Argon2id from the same master key the secrets store uses, but with a **purpose-specific salt** (`"dicode/run-inputs/v1"`) and the same Argon2id parameters as `pkg/secrets`. This means a leak of the secrets-store derived key does not reveal the run-inputs derived key (different salts → different outputs from Argon2id), and vice versa.

**Nonce:** 24-byte random per row (XChaCha20-Poly1305 nonce size).

**AEAD additional data:** `runID || ":" || input_stored_at_string`. Binds the ciphertext to its row identity — a copy-paste of the blob into a different row's storage key fails decryption.

**On-disk blob layout** (handed to the storage task as base64):

```
[24B nonce][N bytes ciphertext][16B Poly1305 tag]
```

**Master key rotation** is out of scope here. When `pkg/secrets` adds rotation, the same mechanism extends naturally (re-derive both keys from the new master).

#### 4.1.3 Redaction policy

The engine assembles a structured `PersistedInput`:

```go
type PersistedInput struct {
    Source      string                 `json:"source"`        // webhook | cron | manual | chain | daemon | replay
    Method      string                 `json:"method,omitempty"`        // webhook only
    Path        string                 `json:"path,omitempty"`          // webhook only
    Headers     map[string]string      `json:"headers,omitempty"`       // webhook only, post-redaction
    Query       map[string]string      `json:"query,omitempty"`         // webhook only, post-redaction
    Body        json.RawMessage        `json:"body,omitempty"`          // webhook only, see body policy below
    BodyKind    string                 `json:"body_kind,omitempty"`     // "json" | "form" | "binary" | "text" | "omitted"
    BodyHash    string                 `json:"body_hash,omitempty"`     // sha256 hex; present when body is omitted/binary
    Params      map[string]any         `json:"params,omitempty"`        // post-redaction (recursive)
    RedactedFields []string            `json:"redacted_fields,omitempty"` // dotted paths that were redacted
}
```

**Redaction is name-based, applied recursively to map keys.** A built-in deny-list (Go constant in `pkg/registry/inputs.go`) of case-insensitive matches:

```
authorization, cookie, set-cookie,
x-hub-signature, x-hub-signature-256,
x-dicode-signature, x-dicode-timestamp,
x-slack-signature, x-line-signature,
password, passphrase, api_key, apikey, api-key,
secret, token, bearer
```

Match rule: a field's name is redacted if it equals (case-insensitive) any item in the deny-list, or if it contains the substring `"signature"`, `"token"`, `"secret"`, `"password"`, or `"key"` after lowercasing. Substring matching catches `MY_SLACK_TOKEN` and `gh-secret-X`; over-redaction (e.g. a legitimate field named `tokens_per_minute`) is the safe failure mode.

**Per-bucket redaction:**

- **Headers:** lowercase the header name, apply the match rule. Redacted values are replaced with `"<redacted>"`. Header names recorded in `RedactedFields` as `headers.<name>`.
- **Query:** same rule. `RedactedFields` entries as `query.<name>`.
- **Params:** recursive walk over `map[string]any` and nested maps; lists are walked positionally. `RedactedFields` entries as dotted paths (`params.user.token`, `params.items[3].secret`).
- **Body:**
  - `application/json` → walk like Params, store post-redaction as JSON.
  - `application/x-www-form-urlencoded` → parse, treat as `map[string][]string`, walk, re-encode.
  - `multipart/form-data` → store metadata (field names, file presence, sizes) but not values; `BodyKind = "omitted"`, `BodyHash` set.
  - Any other content type (binary, plain text, XML, …) → `BodyKind = "binary"` or `"text"`, `BodyHash` set, body itself omitted unless `persist_inputs.body_full_textual: true` per-task. This is conservative-by-default.

**Per-task / global controls:**

```yaml
# dicode.yaml
defaults:
  run_inputs:
    enabled: true             # default true; set false to disable persistence globally
    retention: 30d            # global default
    storage_task: local-storage
    body_full_textual: false  # default false; allow plain-text/XML body persistence

# task.yaml — per-task overrides
run_inputs:
  enabled: false              # opt this task out
  retention: 7d
  body_full_textual: true     # this task's bodies are non-sensitive (e.g. internal cron heartbeats)
```

**Auto-fix-specific opt-out:** distinct from `run_inputs.enabled`, a task can persist input but withhold it from the auto-fix agent's prompt:

```yaml
# task.yaml
auto_fix:
  include_input: false  # input is persisted (replay still works for humans), but auto-fix sees only logs+output
```

#### 4.1.4 Storage task contract

3 ops, no `list` (core tracks `runID → storage_key` in the `runs` table):

```yaml
# tasks/buildin/local-storage/task.yaml — default backend
apiVersion: dicode/v1
kind: Task
name: "Local Storage (run inputs)"
runtime: deno
trigger:
  manual: true              # only invoked by core via run_task
params:
  op:    { type: string, required: true }   # put | get | delete
  key:   { type: string, required: true }
  value: { type: string, default: "" }      # base64(blob); put only
permissions:
  fs:
    - path: "${DATADIR}/run-inputs"
      permission: rw
timeout: 30s
notify:
  on_success: false
  on_failure: true
```

Returns `{ ok: true, value?: "<base64>" }` on success, `{ ok: false, error: "..." }` on failure. Core retries transient failures (network, S3 throttling); storage task itself is stateless.

Note: `${DATADIR}` is a new template var introduced as part of this work — exposes the daemon's data directory (the same one used by `pkg/db` for SQLite, by `pkg/source/git` for clones, etc.). Added to [pkg/task/template.go](../../pkg/task/template.go) alongside `TASK_DIR` / `HOME` / `TASK_SET_DIR`.

#### 4.1.5 Read path

Used by `dicode.runs.replay`, the auto-fix driver's prompt builder (`dicode.runs.get_input`, internal-only), and the cleanup task.

1. Look up `input_storage_key` for `runID` in `runs` table.
2. Call configured storage task with `op: get, key: <stored_key>`.
3. Base64-decode, split nonce/ciphertext/tag, XChaCha20-Poly1305 decrypt with AEAD-AD `runID || ":" || input_stored_at`.
4. Return as structured `PersistedInput`. The decrypted struct includes `RedactedFields` so callers can surface the list.

If the input has been GC'd or never stored, return a typed `ErrInputUnavailable`. Callers must handle this gracefully — typically by aborting the loop with a clear message.

#### 4.1.6 Pinning + crash-recovery

`runs.input_pinned` is set when the auto-fix driver starts (it pins the failed run's input); cleared when the driver exits (success or failure). The driver MUST `defer cleanup()` to ensure unpinning on panic / context-cancel.

If the engine crashes mid-fix, stale pins survive in the database. **Engine startup**:

1. Mark all runs with `input_pinned = 1` whose `parent_run_id` IS NULL or whose triggering auto-fix run is no longer in `running` status as pinned-but-orphaned.
2. Sweep: clear the pin so the next retention cycle can collect them.

This is a single SQL update at startup, not a separate task.

### 4.2 Retention via cleanup task

A new buildin, modeled on [`buildin/temp-cleanup`](../../tasks/buildin/temp-cleanup/task.yaml):

```yaml
# tasks/buildin/run-inputs-cleanup/task.yaml
apiVersion: dicode/v1
kind: Task
name: "Run-input retention sweep"
runtime: deno
trigger:
  cron: "17 * * * *"   # hourly
permissions:
  dicode:
    runs_list_expired: true      # new SDK call
    runs_delete_input: true      # new SDK call
timeout: 120s
notify:
  on_success: false
  on_failure: true
```

Logic: `dicode.runs.list_expired({ exclude_pinned: true })` returns runIDs whose `input_stored_at + retention < now`. For each, call `dicode.runs.delete_input(runID)` — core looks up the storage key, calls the configured storage task's `delete` op, clears the row's input columns. Pinned rows are skipped.

A second cleanup buildin handles dev-mode worktrees (analogous and independent):

```yaml
# tasks/buildin/dev-worktrees-cleanup/task.yaml
apiVersion: dicode/v1
kind: Task
name: "Dev-worktree orphan sweep"
runtime: deno
trigger:
  cron: "*/15 * * * *"
permissions:
  fs:
    - path: "${DATADIR}/dev-worktrees"
      permission: rw
  run: ["git"]   # only `git worktree prune/remove`; documented narrow purpose
  dicode:
    list_runs: true
timeout: 60s
notify:
  on_failure: true
```

Logic: list directory entries under `${DATADIR}/dev-worktrees/<source>/<runID>`, cross-check against running auto-fix runs (`dicode.list_runs({ status: "running", task_id_prefix: "auto-fix" })`); any worktree dir whose `<runID>` is not in the running set is removed via `git worktree remove --force` + branch retained on disk.

(`run: ["git"]` here is a deliberate narrow exception — only the cleanup task uses git binary, and only for worktree pruning. The auto-fix task itself uses `dicode.git.commit_push`, which goes through go-git in core. See § 4.6.4 for rationale.)

### 4.3 Replay primitive

**SDK / REST:**

```ts
// dicode SDK (internal use by tasks)
dicode.runs.replay(runID: string, taskName?: string): Promise<{ run_id: string }>

// REST mirror
POST /api/runs/:id/replay  with { "task_name"?: "..." }
```

Behavior:

1. Resolve original run; require non-empty `input_storage_key`.
2. Decrypt input via the read path.
3. Resolve target task: `task_name` if provided, else original run's task.
4. Fire as a *new* run with `parent_run_id = original.id`, `triggerSource = "replay"`. The new run's input is itself persisted (same retention rules) — recursive replay works.
5. **Replay-triggered runs do NOT fire `on_failure_chain` if they fail.** This prevents the agent's validation step from re-triggering itself. The engine checks `triggerSource == "replay"` in `FireChain` and skips chain firing.
6. **Cooldown does NOT apply to replay.** The cooldown is on chain-firing, not on running the task. Replay is human/agent-initiated; if a user asks for a replay, they get one.
7. Return the new run ID synchronously; the run executes asynchronously.

`task_name` is optional and is the only knob. Branch is not a parameter — the live source's resolution decides what code runs (with or without dev mode).

**Replay-fidelity limitation (documented, not solved in v1):** the input was redacted at store-time. Authorization headers, signatures, etc. are gone. A replay is therefore an *internal* invocation by the agent, not a faithful re-impersonation of the original sender. If the failing task's logic depends on the redacted fields (e.g., HMAC-validates the body), replay will not exercise that codepath — and worse, may *succeed* for the wrong reason. The auto-fix prompt explicitly surfaces `redacted_fields` so the agent can reason about the gap. Operators with sensitive auth flows should set `auto_fix.include_input: false` and let the agent work from logs+output only.

MCP exposure: a `replay_run` MCP tool may be added later to the buildin MCP task for external agents. Out of scope here.

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
2. Compute worktree path: `${DATADIR}/dev-worktrees/<sourceName>/<branch-sanitized>/`.
3. If worktree doesn't exist: use go-git to add a worktree on `<branch>`. If branch doesn't exist locally: create from `Base` (default = the source's tracked branch); if it doesn't exist remotely either, that's fine — push will create it.
4. Set `s.devRootPath = <path>/<root entry yaml>`, `s.resolver.SetDevMode(true)`, trigger immediate sync.

Engine behavior when `enabled=false` *and* the source was previously in worktree-mode:

1. Disable dev-ref substitution (existing behavior).
2. Use go-git to remove the worktree.
3. The branch ref itself is *retained* (commits made by the agent live on disk and are pushed to remote on the way out).

Concurrency: at most one dev-mode-with-branch session per source at a time. A second `SetDevMode(enabled=true, Branch=...)` on a source that's already in worktree-mode returns `ErrDevModeBusy`. Auto-fix engine serialises via the per-task `max_concurrent` guard (§ 4.7).

Crash safety: see the `dev-worktrees-cleanup` buildin in § 4.2.

**SDK exposure** (new):

```ts
dicode.sources.set_dev_mode(name: string, opts: {
    enabled: boolean,
    local_path?: string,
    branch?: string,
    base?: string,
}): Promise<{ ok: true }>
```

Wraps the existing `SourceManager.SetDevMode` Go call, exposed via IPC. Authorisation: a task needs `permissions.dicode.sources_set_dev_mode: true` to call it (new permission flag).

REST mirror: existing `PATCH /api/sources/{name}/dev` body extended:

```json
{ "enabled": true, "branch": "fix/abc-123", "base": "main" }
```

MCP: existing `switch_dev_mode` tool's argument schema extends to accept `branch` + `base`.

### 4.5 Chain trigger with parameter passing

Today `defaults.on_failure_chain` is a string (task ID). Extended to accept a structured form:

```yaml
defaults:
  on_failure_chain:
    task: auto-fix
    params:
      mode: review            # review | autonomous (autonomous REJECTED here, see below)
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

**Config-load validation** (hard errors, not warnings):

1. Reserved-key collision: if `params` contains any of `taskID`, `runID`, `status`, `output`, `_chain_depth`, **fail config load**. These keys are populated by the engine and cannot be user-overridden.
2. **`mode: autonomous` at `defaults.on_failure_chain.params` is rejected** with a config-load error directing the user to opt in per-task. Rationale: a `defaults`-level `autonomous` silently applies to every new task added later, which is exactly the surprise the LP "guardrails built in" copy promises against. Per-task `on_failure_chain.params.mode: autonomous` is the only valid path.
3. **Branch protection prerequisite** for autonomous mode: documented (not enforced) requirement that the source's tracked branch be branch-protected on the forge — the agent shouldn't be the only review for direct-to-main pushes. Doc'd in `dicode-task-dev.md` and the auto-fix README.

Chain payload merge in [pkg/trigger/engine.go:670-677](../../pkg/trigger/engine.go#L670-L677):

```go
input := map[string]any{
    "taskID": completedTaskID,
    "runID":  runID,
    "status": runStatus,
    "output": output,
    "_chain_depth": parentChainDepth + 1,
}
for k, v := range chain.Params {
    input[k] = v   // engine reserved keys are guaranteed by config-load validation
                   // not to appear in chain.Params, so no collision possible at runtime
}
```

The auto-fix task reads its `mode` (and other guardrails) from `params` like any normal task.

User composition: this lets users define **two `task.yaml` entries** that wrap `auto-fix` with different param defaults — same pattern as `tasks/examples/ai-agent-{openai,ollama,groq}`. Their `dicode.yaml` then references whichever wrapper they want.

### 4.6 Auto-fix task

**Defined as a TaskSet override entry** in [`tasks/buildin/taskset.yaml`](../../tasks/buildin/taskset.yaml), mirroring the existing `dicodai` entry's shape (`ref:` → `./ai-agent/task.yaml`, with `overrides:` setting flat `params: key: value` defaults plus per-section overrides for `trigger`, `env`, `net`, `permissions`):

```yaml
# tasks/buildin/taskset.yaml — additional entry (sketch — exact override syntax
# follows the live dicodai entry; see lines 37-77 of taskset.yaml today)
auto-fix:
  ref:
    path: ./ai-agent/task.yaml
  overrides:
    name: "Auto-fix on failure"
    description: |
      Diagnoses a failed task run, edits source on a worktree, validates via
      task tests + replay, and either merges to main (autonomous) or opens a
      PR (review). Fired by setting `on_failure_chain: auto-fix`.
    trigger:
      manual: true              # fired by on_failure_chain dispatcher, not user
    params:
      # Override ai-agent's existing params with auto-fix defaults:
      skills: "dicode-task-dev,dicode-basics,dicode-auto-fix"
      prompt: "(set from chain input by the auto-fix skill prompt)"
      system_prompt: "(replaced by the dicode-auto-fix skill at load time)"
      max_tool_iterations: 30   # higher than ai-agent's 10 — fix loops chatter more
    timeout: 1800s
    notify:
      on_success: true
      on_failure: true
    # NEW SDK permission flags added in child issue (2):
    permissions_dicode:
      runs_replay: true
      runs_get_input: true
      runs_pin_input: true
      runs_unpin_input: true
      sources_set_dev_mode: true
      tasks_test: true
      git_commit_push: true
      list_tasks: true
      get_runs: true
      tasks:
        - git-pr                  # static literal; users override to point elsewhere
    permissions_fs:
      - path: "${DATADIR}/dev-worktrees"
        permission: rw
```

(Exact override merging semantics follow the existing `Resolver` in [pkg/taskset/resolver.go](../../pkg/taskset/resolver.go); implementation must verify whether overrides extend `permissions.dicode.{tasks,*flags}` and `permissions.fs` correctly, and add the merge if not. This is a small clarification, not a redesign.)

**Auto-fix-specific config** (`mode`, `max_iterations`, `max_iteration_seconds`, `max_tokens`, `branch_prefix`, `pr_task`, `base_branch`) does NOT live in `params:` declarations — it arrives via the **chain trigger params merge** described in § 4.5. The engine fires `auto-fix` with an input map of `{ taskID, runID, status, output, _chain_depth, mode, max_iterations, ... }`. The `dicode-auto-fix` skill prompt instructs the agent to read these from the input map.

This avoids redeclaring schema in an override (which the current TaskSet override mechanism doesn't natively support — overrides set values for existing params, not new param shapes).

The agent loop is implemented as a new buildin skill (`dicode-auto-fix`, a markdown file under `tasks/skills/`) loaded into the `ai-agent` system prompt via the override's `skills` param. The skill prescribes the workflow:

1. Read failure context: `taskID`, `runID`, `status`, `output`, plus persisted input via `dicode.runs.get_input(runID)` (returns `{ input, redacted_fields }`). If `auto_fix.include_input: false` on the failing task, the agent gets only logs+output and is told the input is intentionally withheld.
2. `dicode.runs.pin_input(runID)` — keep the input alive for the duration of the loop.
3. `defer dicode.runs.unpin_input(runID)` semantics — the task's wrapper sets up cleanup that runs on success, failure, or timeout.
4. Generate fix branch name (review: `${branch_prefix}${runID}`; autonomous: `base_branch || sourceTrackedBranch`).
5. `dicode.sources.set_dev_mode(<source>, { enabled: true, branch: <fixBranch>, base: <base> })`.
6. Iterate (capped at `max_iterations`, each iteration capped at `max_iteration_seconds`):
   - Read failing task's source files via `Deno.readTextFile` from the worktree path.
   - Edit via `Deno.writeTextFile` to the same worktree path. (Permissions: `fs.rw: ${DATADIR}/dev-worktrees`.) The agent's edit scope is enforced by the path it writes to — cross-task edits are explicitly out of scope and the loop's prompt forbids them.
   - Validate by inline YAML/schema parse.
   - Test via `dicode.tasks.test(<failingTaskID>)` (runs `task.test.{ts,js}` if present; if absent, the agent is instructed to write one as part of the fix).
   - Replay via `dicode.runs.replay(<failedRunID>)`. Wait for the new run to finish; check status.
   - If both green → exit loop.
7. On success: `dicode.git.commit_push(<source>, <message>, branch=<fixBranch>)`. Engine validates the branch matches the `branch_prefix` allow-list (or is the source's tracked branch in autonomous mode), refuses force-push.
8. If `mode == "review"`: invoke the PR task via `dicode.run_task(params.pr_task, { source_id, branch: fixBranch, base: base, title, body })`.
9. `dicode.sources.set_dev_mode(<source>, { enabled: false })` — engine removes the worktree; branch retained.
10. Unpin the failed run's input.

Loop terminator conditions:

- **Success:** as above.
- **Iteration cap:** push the partial work to `${branch_prefix}wip-${runID}`, open a "needs human" PR, exit. (Review mode only; autonomous mode aborts without pushing partials to main.)
- **Token budget:** same as iteration cap.
- **Hard error:** infrastructure error (decrypt fail, push fail, dev-mode error). Driver disables dev mode, unpins, exits with failure.
- **Timeout (1800s):** runtime kills the task; `defer` cleanup runs (unpins, attempts to disable dev mode).

#### 4.6.1 Why no shell-out to `git`

dicode's "no git binary" key constraint ([CLAUDE.md](../../CLAUDE.md)) says the daemon uses go-git for all git operations. The auto-fix task respects this by routing commit/push through a new `dicode.git.commit_push` SDK call that the Go core implements via go-git.

The single deliberate exception is the `dev-worktrees-cleanup` task (§ 4.2), which uses `git worktree prune` because go-git's worktree support is more limited and cleanup is a sysadmin operation rather than user-facing. This is documented at the task and limited to that one task's permissions.

#### 4.6.2 git_commit_push contract

```ts
dicode.git.commit_push(sourceID: string, opts: {
    message: string,
    branch?: string,        // default: source's currently-active dev mode branch (if any)
    files?: string[],       // default: all tracked changes in the worktree
    allow_main: boolean,    // default false; must be true for autonomous mode
}): Promise<{ commit: string, pushed: boolean }>
```

Engine enforces:

- The branch must match `${branch_prefix}*` patterns (read from per-task config) OR equal the source's tracked branch when `allow_main=true`.
- Never `--force`. A diverged branch causes the call to error out; the driver's recovery is to abandon the loop with a structured error.
- Push uses the source's existing auth (env-injected forge token, same as reconciler pulls).

### 4.7 Loop guardrails (engine-level)

| Guard | Default | Where enforced | Override |
|---|---|---|---|
| Max fix iterations per run | `5` | auto-fix driver | `params.max_iterations` |
| Per-iteration timeout | `300s` | auto-fix driver | `params.max_iteration_seconds` |
| LLM token budget per run | `50_000` | auto-fix driver | `params.max_tokens` |
| Cooldown after auto-fix runs (per failing task) | `10m` | trigger engine | `defaults.on_failure_chain.cooldown`; per-task override |
| Concurrent auto-fixes per failing task | `1` | trigger engine | `defaults.on_failure_chain.max_concurrent` |
| Concurrent auto-fixes globally | `3` | trigger engine | `defaults.on_failure_chain.max_concurrent_global` |
| Chain depth | max `2` | trigger engine; reserved key `_chain_depth` | `defaults.on_failure_chain.max_depth` |
| Failure storm circuit breaker | trip if > `10` chain fires within `1m`; suppress for `30m`; emit notification | trigger engine, **scope = per-source** (failures in one user's source don't suppress another's) | `defaults.on_failure_chain.storm.{rate, suppress, scope}` |
| Replay → `on_failure_chain` | suppressed (`triggerSource == "replay"` skips FireChain) | trigger engine | not configurable in v1 |
| Cooldown → replay | not enforced (replay is human/agent-initiated) | trigger engine | not configurable in v1 |
| Push refspec scoping | `${branch_prefix}*` allowlist + tracked branch when `allow_main=true` | git_commit_push | per-task `branch_prefix` |

Cooldown semantics: if task X fails at T0 and auto-fix fires, a second failure of X at T0+5m does **not** fire auto-fix. The on-failure notification still fires per the existing `on_failure` flag.

Config-load warnings sink: zap log at WARN level + a `webui_warnings` table row (new, single-purpose) so the WebUI can surface "your config has X issues" on startup. Schema for that table is out of scope — TODO is filed; the warning content is logged for now.

### 4.8 git-pr task (review mode)

```yaml
# tasks/buildin/git-pr/task.yaml — reference implementation
apiVersion: dicode/v1
kind: Task
name: "Open Pull Request (GitHub)"
runtime: deno
trigger:
  manual: true
params:
  source_id: { type: string, required: true }
  branch:    { type: string, required: true }
  base:      { type: string, default: "main" }
  title:     { type: string, required: true }
  body:      { type: string, default: "" }
permissions:
  run: ["gh"]
  env:
    - name: GH_TOKEN
      from: GH_TOKEN_AUTOFIX     # documented: provision a fine-grained PAT scoped
                                  # to {contents:write, pull_requests:write} on the
                                  # specific repo; do NOT reuse an admin token.
  fs:
    - path: "${DATADIR}/dev-worktrees"
      permission: r
timeout: 60s
```

Returns `{ ok: true, url: "..." }` or `{ ok: false, error: "..." }`. Replaceable by users with their own task targeting GitLab/Gitea/Forgejo/Bitbucket.

**Security note on `permissions.run: [gh]`:** the dicode `run` permission is binary-name-scoped, not subcommand-scoped. With `gh` allowed, the task can in principle invoke `gh repo delete`, `gh auth login`, `gh secret set`, etc. The defenses are layered:

1. `GH_TOKEN_AUTOFIX` should be a fine-grained PAT — even if the agent prompts `gh repo delete`, the PAT lacks the scope.
2. The agent's prompt does not include `gh` as a tool; only the `git-pr` task does. The agent invokes `git-pr` via `dicode.run_task("git-pr", ...)` — it cannot reach into git-pr's runtime.
3. Documented in onboarding as a prerequisite: "auto-fix requires a scoped PAT, not your default `gh` auth".

A future hardening (out of scope here): wrap `gh` invocation in a vendored shim that whitelists subcommands. Tracked under epic #207.

## 5. Data flow walkthrough — review mode

A webhook task `process-payment` fails with a 500 from a downstream API.

1. **t=0:** webhook arrives. Engine: build `PersistedInput` (with redaction applied to headers/query/body) → encrypt with XChaCha20-Poly1305 → call `local-storage` task `op=put, key=run-inputs/<runID>` → store `input_storage_key + size + stored_at + redacted_fields` on the new run row.
2. **t=0+1s:** task runs, fails. `FireChain` checks `defaults.on_failure_chain` — set to `{ task: auto-fix, params: { mode: review } }`.
3. Engine fires `auto-fix` with input `{ taskID: "process-payment", runID: "<id>", status: "failure", output: <500 body>, mode: "review", _chain_depth: 1 }`.
4. **t=0+2s:** `auto-fix` driver pins input via `dicode.runs.pin_input("<id>")`, picks branch `fix/<runID>`, calls `dicode.sources.set_dev_mode("user-tasks", { enabled: true, branch: "fix/<runID>", base: "main" })`.
5. Engine: go-git `Worktree.Checkout` on `${DATADIR}/dev-worktrees/user-tasks/fix-<runID>/` (created from main since branch didn't exist), resolver swaps source root, registry reloads tasks from worktree.
6. **t=0+5s:** driver builds prompt with failure context (`{ logs, output, input, redacted_fields: ["headers.authorization", "headers.x-stripe-signature"] }`) plus reads task source via `Deno.readTextFile`. Hands off to the underlying `ai-agent` runtime. Agent reads logs, sees "500 from upstream", proposes adding a retry-with-backoff. Calls `Deno.writeTextFile` to the worktree's `process-payment/task.ts`.
7. Agent calls `dicode.tasks.test("process-payment")` — sees no test; writes `task.test.ts` with a regression test for the 500 case using a mocked fetch; reruns — passes.
8. Agent calls `dicode.runs.replay("<id>")` — engine decrypts the original webhook body (sans Stripe signature), fires a new run on the worktree code with retry. The replay run does NOT fire `on_failure_chain` (we suppressed it for `triggerSource == "replay"`). Replay run passes (no auth check exercised because of redaction; agent was warned via `redacted_fields`).
9. Agent calls `dicode.git.commit_push("user-tasks", { message: "auto-fix: process-payment retry on 5xx", branch: "fix/<runID>" })`. Engine validates `fix/<runID>` matches `${branch_prefix}*`, runs go-git `Add` + `Commit` + `Push` (no `--force`).
10. Driver invokes the PR task via `dicode.run_task("git-pr", { source_id, branch: "fix/<runID>", base: "main", title, body })`. `gh pr create` returns URL.
11. Driver calls `dicode.sources.set_dev_mode("user-tasks", { enabled: false })`. Engine removes worktree, source resumes pulling main.
12. Driver unpins via `dicode.runs.unpin_input("<id>")`, returns `{ ok: true, pr_url: "..." }`.
13. Operator gets notification "auto-fix opened PR <url> for process-payment runID=...". Reviews, sees the PR mentions "redacted_fields contained Stripe signature — please verify the retry logic doesn't bypass signature validation" in the auto-generated body. Reviews carefully and merges. Reconciler picks up the change on the next poll.

## 6. Decomposition into child issues

Filed under epic [#207](https://github.com/dicode-ayo/dicode-core/issues/207); tracked from [#228](https://github.com/dicode-ayo/dicode-core/issues/228):

1. **Run-input persistence + storage task contract + retention sweep** — schema delta with first-`ALTER TABLE` migration step, XChaCha20-Poly1305 + Argon2id (purpose-specific salt), redaction policy with explicit per-bucket rules, `local-storage` buildin, `run-inputs-cleanup` buildin, the new `${DATADIR}` template var, startup-time stale-pin sweep. SDK additions: `runs.list_expired`, `runs.delete_input`, `runs.pin_input`, `runs.unpin_input`, `runs.get_input` (internal-only).

2. **Auto-fix SDK surface** — `dicode.runs.replay`, `dicode.tasks.test`, `dicode.sources.set_dev_mode` (with branch+base support), `dicode.git.commit_push` (go-git, refspec scoping, no `--force`). REST mirrors. Permissions for each (`runs_replay`, `tasks_test`, `sources_set_dev_mode`, `git_commit_push`). Depends on (1) for replay's input read.

3. **Dev mode `branch` lifecycle + `on_failure_chain` parameter passing** — `SetDevMode` opts struct with `Branch`/`Base`, go-git worktree create/remove, dev-worktrees-cleanup buildin, `on_failure_chain` structured form with reserved-key + autonomous-at-defaults config-load errors. Independent of (1) and (2) — usable by humans without auto-fix.

4. **Auto-fix taskset override + git-pr buildin + engine guardrails + auto-fix skill** — add `auto-fix` to `tasks/buildin/taskset.yaml` as a `dicodai` override, `git-pr` reference impl, the engine-side guardrails (cooldown, concurrency, chain depth, storm circuit breaker, replay→chain suppression, push refspec scoping), the `dicode-auto-fix` skill markdown. Depends on (1), (2), (3).

Each child issue is independently mergeable: (1) ships persistence with no consumers; (2) ships replay+SDK surfaces usable from any task and from REST; (3) ships dev-mode-on-a-branch usable by humans; (4) finally wires the loop.

Recommend shipping in this order so v0.2.0 can include (1)+(2)+(3) — replay-from-WebUI is itself a useful debugging tool — and the auto-fix loop arrives in v0.3.0 once each foundation block is hardened.

## 7. Open questions / explicitly deferred

- **Auto-revert on monitoring signals from a successful deploy.** The LP "When AI is wrong" copy implies that a *successful* deploy that subsequently breaks production is also auto-fixed. v1 covers the on-failure path only; revert-on-regression is a follow-up under the same epic.
- **Multi-task fixes.** v1 restricts the agent to writing inside the failed task's directory. A failure rooted in a *dependency* task is detectable by the agent (it can read other tasks' source) but the fix would have to be outside its write scope. v1 punts this with a clear error from the prompt and the worktree's path-scoped permissions.
- **Forge auth UX.** `git-pr` ships with `GH_TOKEN_AUTOFIX` env. A first-launch onboarding step that prompts for a fine-grained PAT (similar to the OpenAI key prompt) is desirable but out of scope here.
- **WebUI surfacing.** The fix worktree's branch and the in-flight auto-fix's progress should appear in the WebUI runs view. Concrete UI design is deferred.
- **Cost telemetry.** Token spend per auto-fix should be aggregated and surfaced. Deferred — first ship the loop, then add accounting.
- **Subcommand-scoped `gh` shim.** Wrapping `gh` invocation to whitelist `pr create` / `pr view` / `pr list` only. Hardening, not v1.
- **MCP-tool extensions** (`write_task_file`, `validate_task`, `commit_task`, `dry_run_task`, `read_task_file`) for external agents. Tracked separately under epic #207.
- **Migration framework.** This spec's `ALTER TABLE` is dicode's first schema migration. A real migration framework (versioned, ordered, tested) can wait until the second migration arrives.

## 8. Security considerations

- Run-input encryption uses an Argon2id sub-key with a purpose-specific salt, so a leak of one derived key does not compromise the other.
- AEAD additional data binds each ciphertext to its `runID + stored_at`. Splicing across rows fails decryption.
- Header/query/body redaction is name-based deny-list with substring matching on `signature`, `token`, `secret`, `password`, `key`. Over-redaction is the safe failure mode. Users with sensitive non-standard field names set `auto_fix.include_input: false` per task.
- Multipart and binary bodies are stored as `BodyKind + BodyHash`, not contents.
- The agent runs with a curated SDK permission set (`runs_replay`, `tasks_test`, `git_commit_push`, etc.). It cannot escalate to filesystem or network access beyond what the override declares. Worktree path-scoping prevents cross-task edits.
- **Autonomous mode requires per-task opt-in.** A `defaults.on_failure_chain.params.mode: autonomous` is a config-load error. Users must explicitly opt each task in by setting `on_failure_chain.params.mode: autonomous` in that task's `task.yaml`. Documented in onboarding: pair with branch protection on the source's tracked branch — the agent should never be the only review for direct-to-main pushes.
- `git-pr` uses `permissions.run: [gh]`. Rationale + threat model documented in § 4.8. Recommend a fine-grained PAT.
- `dev-worktrees-cleanup` uses `permissions.run: [git]` for `git worktree prune` only. Documented narrow exception to the "no git binary" constraint, isolated to one task.
- Replay-fidelity: a replay validates against post-redaction input, not the original sender's request. The agent prompt explicitly surfaces `redacted_fields`. PRs include a "redacted fields were [...]" line in their body so reviewers can sanity-check the agent's reasoning.
- Stale pin recovery on engine startup prevents indefinite `input_pinned = 1` lingering from a crashed driver.

## 9. Landing-page mapping

| LP claim | Spec section |
|---|---|
| "API down → AI diagnoses → auto-fix deployed" | § 5 (full walkthrough) |
| "Every step is a git commit you can review" | § 4.6 step 7, § 4.8 |
| "Require review before deploy, or run fully autonomous" | § 4.6 `params.mode`; § 4.5 autonomous-at-defaults guard; § 8 prerequisites |
| "Auto-revert on failure" | Deferred — § 7 |
| "When AI is wrong → the monitor catches it → git revert is one command away" | Partial — § 4.6 iteration-cap WIP-PR fallback; revert deferred § 7 |
| "Continuous loop — create, validate, deploy, monitor, **fix**" | This spec covers the *fix* arc; the rest already exist. |
