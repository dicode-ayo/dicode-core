# Design — AI task authoring

**Status:** adopted direction (2026-07-10). Rescopes epic #288; informs #217, #213, #120.
Grounded in a five-map code audit of `main`; see the disconnects table for evidence.

This document is the reference the AI-authoring issues point at. Update it when the
direction changes — do not let the issue bodies drift independently.

## Problem

The flagship promise is *"describe a task in English, get a working, deployed task."*
Today that path does not exist end-to-end. The audit found the "AI path" is **three
disconnected systems** with dead wiring between them.

## Current state (audited)

| System | What it is | Status |
|---|---|---|
| **A — Chat** | `dicode ai` / `POST /api/ai/chat` fire `cfg.AI.Task` (default `buildin/dicodai`, an OpenAI override of `ai-agent`) via the trigger engine and return a reply. | Works. Free text only — the user pastes code back into the editor by hand. |
| **B — Auto-fix** | `on_failure_chain` dispatch with storm/depth guards, per-run dev-mode clones, the capability-gated agent SDK (`sources.set_dev_mode`, `tasks.test`, `runs.replay`, `git.commit_push`), `buildin/git-pr` landing, `mode=review` default. | Shipped end-to-end, but nothing sets `on_failure_chain: auto-fix`, so it never fires. |
| **C — Authoring sessions** | `pkg/webui/authoring_service.go` + `authoring_db.go` + the IPC/CLI `task create\|edit\|save\|cancel` verbs: a session state machine. | Hollow. `CreateTask` writes a hardcoded hello-world; `Edit` opens a DB row; `Save` flips a flag. No AI, no sandbox. |

### The disconnects

| Dead wiring | Evidence |
|---|---|
| Prompt captured at the CLI flag → IPC request → then **discarded**; `EditTask` has no prompt parameter. | `authoring.go:75`, `control.go:411`, `cmd/dicode/task.go:158` ("not wired yet") |
| `ai.create_task` defaults to `buildin/task-create`, which **resolves to nothing** (no dir, no taskset entry) — and no runtime code reads the field. A test pins the phantom as correct. | `config.go:571`, `config_test.go:689` |
| `SandboxPath` is plumbed through DB/struct/IPC/REST but **assigned nowhere**. The working clone mechanism (B) is never called from authoring. | grep: 0 writes |
| `${DATADIR}/ai-tasks` is never created, so a fresh-install `task create` 404s. | `config.go:490` |
| `dicode-task-dev.md` skill names **seven MCP tools that do not exist**; the real server exposes six different ones. | skill vs `mcp/task.ts:84` |

**Key realization:** everything the authoring loop needs already exists in **System B**.
Auto-fix already does prompt → dev-clone → edit → test → replay → commit → PR, with
permissions, guardrails, and a review default. Authoring is that same loop with a
different entry point and skill. The work is **alignment, not construction.**

## Decision

**Converge authoring onto the auto-fix substrate, and model the authoring session as a
suspendable task run.**

1. **One agent-runner.** `handleAI` (`control.go:775`) already fires an agent task by id,
   waits, and extracts `{reply, session_id}`. That is the canonical "run an agent turn"
   primitive; `ai_chat`, `handleAI`, and authoring all route through it (the explicit seam
   extraction is Phase 2). No new dispatch idiom.
2. **`task-create` is a taskset override of `ai-agent`**, cloning `auto-fix`'s proven
   permission set (`sources_set_dev_mode`, `tasks_test`, `tasks: [git-pr]`, dev-clones
   `fs`), differing only by skill + system prompt. `dicodai` and `auto-fix` already prove
   the pattern.
3. **The authoring session *is* a suspended run.** The loop is human-in-the-loop — the
   agent needs clarification ("which Slack channel? what time?") before it can finish.
   Rather than the bespoke `authoring_sessions` state machine, the `task-create` agent
   `dicode.suspend({schema})` when it needs input (JSON-Schema form, #514), and `resume`
   feeds the validated answer back (auto-dispatch, #515). The suspended run's own state —
   its dev-clone, its conversation, its pending form — *is* the session. This is why
   suspend/resume was built first, and it lets us likely **delete** System C's session
   bookkeeping rather than wire it up.
4. **Review-before-deploy is already solved.** The #392 trust-on-change approval gate holds
   every changed task (AI-authored included) pending operator approval. Save just lands the
   task; the gate reviews it. No bespoke AI review queue — #213 collapses to the autonomy
   dial.
5. **Agents reach dicode capabilities through one governed surface: MCP.** Every backend
   (`ai-agent`, `ai-agent-claude-cli`, future) authors by calling dicode's MCP tools, on the
   same skills + prompts — not each model's raw tools and not the SDK. **This only closes
   the privilege hole if MCP carries the capability model** (see #560): (a) restrict the raw
   model tools so the agent can't bypass MCP (`claude --allowedTools` = the dicode tools
   only, `--permission-mode`, confined cwd); (b) add **per-agent capability scoping** to
   `/mcp` — today it is a single daemon-wide Bearer key with no per-caller scoping, so a
   naive "give it MCP" would hand every agent full access; (c) grow the MCP surface to the
   governed authoring tools (write-into-clone, test, commit/PR), lifting existing REST
   (`/api/sources/{name}/commit-push`, `/api/tasks/{id}/test`) into it. The capability-gated
   IPC control plane stays the enforcement point; MCP is its model-agnostic front door.

### Rejected alternatives

- **Finish #288 as originally specced** (custom `.sessions/` sandboxes, rsync save, bespoke
  review queue): builds a second sandbox/apply/review mechanism next to three shipped ones.
- **A `dicode.ai` SDK primitive** (provider calls in Go): breaks the "an agent is a task
  with declared permissions" security model the capability-gated SDK and provider-swap
  presets depend on; moves the control plane out of Go where the guardrails live.

## Backend matrix

The authoring loop needs a **tool-using** agent (write files, call `tasks.test`, `git-pr`).
Both backends are tool-capable; they differ in **how governed** those tools are.

| Backend | Chat | Authoring | Governance today |
|---|---|---|---|
| `buildin/ai-agent` (OpenAI-compatible, `OPENAI_API_KEY`) | ✅ | ✅ | Governed — edits go through the capability-gated dicode SDK (dev-clone, `tasks.test`, `git-pr`). Drives `auto-fix` today. |
| `buildin/ai-agent-claude-cli` (Claude Pro/Max, `CLAUDE_CODE_OAUTH_TOKEN`) | ✅ | ✅ | **Partially governed.** The task always passes `--disallowedTools` denying Claude's built-in Read/Write/Edit/Bash/NotebookEdit/WebFetch/WebSearch/Glob/Grep/Task/KillShell tools (fail-closed, regardless of MCP wiring), and `--allowedTools mcp__dicode` when MCP is wired. Still tracked as partially-governed: the MCP surface itself only covers the tools it exposes today, so growing it to the full governed authoring toolset (write-into-clone, test, commit/PR) is open work (#560). |

The end state (decision 5): both backends run the **same governed MCP path** on the same
prompts. Claude-via-subscription becomes a first-class authoring backend *once* its raw
tools are restricted and MCP is capability-scoped — not before.

## Phased plan

Phase 0 is **fork-agnostic** — needed regardless of the rescope. The Option-2 / suspend
divergence bites at Phase 1.

| Phase | Goal | Closes |
|---|---|---|
| **0 — Align** | `task create --ai "<prompt>"` returns a real AI turn. Ship the `task-create` override; thread the prompt through `handleAI`-style dispatch; `mkdir ai-tasks` at startup; fix the phantom-pinning test; rewrite the `dicode-task-dev` skill to the real tool surface; doc fixes. | #288 phantom bug |
| **1 — Land the loop** | Prompt → generated task (dev-clone) → suspend/resume for clarification → test → apply/PR → approval gate → deployed. Sessions become suspended runs. | #288 · #213 (delta = dial) |
| **2 — One seam + validator** | Extract the single agent-runner across chat/handleAI/authoring; field-keyed `Spec.Validate` + a `dicode validate` verb; mocked-LLM CI coverage. | #217 (validator) |
| **3 — Surface + autonomy** | `dc-ai-edit-panel` with diff apply, `dc-task-create` entry, streaming events; optional `ai.deploy_mode` dial + opt-in `on_failure_chain: auto-fix`. | #120 · #213 residual |

## Verified: clone survival across suspend

The load-bearing question for the session-as-run model — *does a suspended run keep its
dev-mode clone?* — was checked against the code (2026-07-10):

- The **run row survives** suspension: the registry stale-run sweep deliberately skips
  suspended runs (`registry.go:28`, `TestCleanupStaleRuns_SkipsSuspended`). ✅
- **Suspending does not tear down the clone**: `SetDevMode(false)` only fires from the
  task-delete flow, not from the suspend path. ✅
- **But `dev-clones-cleanup` would reap it.** Its `collectActiveRunIDs` protects only clones
  whose run is `Status === "running"` (`task.ts:29`) and sweeps every 15 min. A *suspended*
  run isn't "running", so its clone is classed an orphan and deleted within ≤15 minutes. ❌

**Phase-1 prerequisite (one line):** extend `collectActiveRunIDs` to protect non-terminal
runs (`running` **or** `suspended`), matching the registry's already-correct behavior.
Without it, a user who pauses to answer the agent's clarifying form loses the sandbox.

## AI interactions as suspend/resume conversations

Every AI interaction — chat *and* authoring — becomes **one conversation = one suspend/resume
loop**. The agent produces a turn, `suspend`s with a JSON-Schema form for the next message, and
`resume` feeds the reply back in. This unifies chat and authoring onto a single model and gives
combined logs, one UI surface, and run-carried state instead of KV `session_id` threading.

**Verified readiness (2026-07-11):**
- **Unbounded loop ✅** — resume *preserves* chain depth (`resume.go:20`), so a chat never hits
  the `maxSuccessChainDepth`/`MaxDepth` ceilings. No turn cap.
- **"One run" ❌ not native** — resume does not re-invoke the same run; it **spawns a new
  continuation run** (`ParentRunID` = the suspended run, `resume.go:150`). So a conversation is a
  *chain of runs* linked by `parent_run_id`, and logs key by `run_id` — scattered, not combined.
  This is the foundational prerequisite (#569): collapse a run's descendant tree under its root
  (extends the existing `run_group`, #114/#116) so the conversation reads as one grouped unit in
  UI and CI. General infra — also helps pipelines/subtasks/auto-fix.

**Two states, decoupled — this is the key design call:**

| Concern | Store | Model | Consumer |
|---|---|---|---|
| Agent's LLM context (full-fidelity messages, intra-turn tool calls) | `resume_state`, offloaded when big | **cumulative** (self-contained per turn) | the model, on resume |
| Conversation record (per-turn input/output, timeline) | the **run tree** (#569) | linked (`parent_run_id` chain) | humans — UI, CI, audit |

Cumulative state is simplest (read one self-contained blob on resume, no reconstruction), and
because the run tree independently holds the per-turn record, the cumulative state stays *purely*
the model's view — it never doubles as the audit log. The trade-off: cumulative grows every turn
and is re-persisted per suspend, so **big-state offload is a required companion, not optional** —
store a structured `{store, key}` reference in `resume_state`, offload the blob to
`buildin/blob-storage` / `buildin/local-storage`, rehydrate on resume via the storage task's `get`
(never a raw fetchable URL — SSRF/coupling), GC keyed to the root run on conversation end.

**Ending a conversation:** the run ends when the handler **returns instead of suspending**,
triggered by (a) an explicit end-marker in the resume input → `success` (primary), (b) idle TTL —
the authoring purge loop already does this — or (c) agent goal-completion. Plus a cumulative
turn/token backstop against runaway chats. No new terminal status needed; map a clean close to
`success`, reserve `cancelled` for a genuine abort.

**Migration scope:** migrating `ai-agent` migrates the fleet (`dicodai`/`auto-fix`/`task-create`
are overrides of it). The **drivers** — `/api/ai/chat`, the WebUI chat panel, `dicode ai` — switch
from fire-a-run-per-turn to drive suspend→resume. Phase order: **#569 run-tree** →
**state offload** → **migrate `ai-agent` + drivers**.

## Verified convergence design (2026-07-12)

A code-grounded design pass on *"one selectable ai-task runs the whole loop/create logic;
the user swaps model/provider; prompts + skills are preserved"*. The finding: this is
**assembly of existing parts**, and the shared code is one ~120-line module. Every claim
below was checked against `main`.

### The seam — a shared turn module, not a mega-task

Provider is a dial by **task id + params + yaml overrides**, not a `provider:` param on a
single task. A single task carrying every backend would need the **union** of their
permissions (`net:` for the OpenAI family vs `run:["claude"]` + fs + env for the CLI
family) — a least-privilege violation. So:

- **`tasks/buildin/ai-agent-core/chat.ts`** — the shared turn envelope extracted from the
  claude-cli slice that already ships (`decideEntryMode`/`isChatEnd`/`chatSchema`/
  `steps.turn`, plus the skill-name validation duplicated across both tasks today). Exposes
  one seam:

  ```
  runTurn({ message, systemPrompt, skills, params, state }) → { reply, state }   // state provider-opaque
  ```

- **`ai-agent/task.ts`** = OpenAI `runTurn` (its existing tool loop), `state = {messages,
  summary}` carried in `resume_state` — the KV `chat:<id>` threading is retired.
- **`ai-agent-claude-cli/task.ts`** = `runTurn` = the already-factored `runClaudeTurn`,
  `state = {claudeSessionId, chatId}`.
- Provider/persona stays a **yaml override** (`dicodai`/`auto-fix`/`task-create`), sharing
  the skills library, not copied prompts.

**Extensibility:** a `codex`/`gemini` CLI backend = one new task dir + one `taskset.yaml`
line (1 existing file changed). A local vLLM / any OpenAI-compat = 0 code, 1 yaml stanza.

### Dedup — reuse, do not rebuild

- **Big-state offload (#570) reuses `registry.InputStore`.** Go already has the exact
  mechanism: marshal → AES-encrypt (`InputCrypto`) → delegate `{op,key,value}` to a
  config-dialed storage task (`run_inputs.storage_task`, default `buildin/local-storage`),
  reference on the runs row, GC by a cleanup buildin. #570 is a threshold-check on
  `len(resume_state)` at the suspend write + an InputStore sibling (`prefix: resume-state/`,
  keyed by **root run id** for conversation-scoped GC). The `{store,key}` reference is
  **internal to Go, invisible to tasks** (and encrypted) — not a task-side protocol. The
  8 MiB IPC frame cap makes this non-optional, not a nicety.
- **Suspend/end notification reuses the `approvalNotifier` pattern.** The WebUI already
  broadcasts `run:finished` with `Status:"suspended"` off `runFinishedHook`. Missing is
  only a `notify_task` fire — a ~50-line `suspendNotifier` mirroring
  `pkg/daemon/approval_notify.go` on the same hook (suspend → the agent awaits input;
  conversation-end → `root_run_id != run_id`). Not in the agent tasks: they'd each need
  `dicode.tasks` grants and it would miss non-AI suspending tasks.
- **Provider dial, run grouping, dispatch** — reuse `cfg.AI.Task`/params, `root_run_id`
  (#569), `handleAI`. No Go provider interface, no prompt-template engine, no second
  notification or storage system.

### Corrections that shape the order

- **The shared skills are factually wrong**, so "skills preserved across providers" is
  false *in content* today: `dicode-task-dev.md` names seven MCP tools that do not exist;
  `dicode-basics.md` carries SDK-surface wording wrong on the MCP path. Fixing skill truth
  is a prerequisite, not polish.
- **Keep `ai-agent` on the capability-gated SDK until per-agent MCP scoping (#560) lands.**
  Converging it onto the current single-bearer MCP surface would be a governance
  *downgrade*. The seam is indifferent — tool dispatch lives inside each `runTurn`.
- **`task.Hash` digests only a task's own dir**, so a shared module outside it doesn't bust
  importers' content hashes — an edited `chat.ts` would bypass the #392 approval gate for
  *user-source* importers (moot for gate-exempt buildins). Follow-up: an optional
  `hash_include:` in task.yaml folded into `Hash()`.

### Ordered plan

| # | Step | Reuses |
|---|---|---|
| 1 | **#566** — protect `suspended` dev-clones (one line) | — |
| 2 | **Skill-truth fix** — real MCP tool surface in the shared `.md`s | `tasks/skills/` |
| 3 | **#570** — `resume_state` offload in Go | `InputStore`/`InputCrypto`, `local-storage`, `root_run_id` |
| 4 | **Seam + #571a** — extract `ai-agent-core/chat.ts`; migrate `ai-agent` onto it | claude-cli envelope, #569, #570 |
| 5 | **#571b** — drivers (`handleAI` / `/api/ai/chat` / `dicode ai`) drive suspend→resume | shipped CLI resume loop |
| 6 | **Suspend-notify** — `suspendNotifier` on `runFinishedHook` | `approval_notify` pattern, `buildin/notify` |
| 7 | **`task-create` + prompt-threading** — kills #288 | `auto-fix` override, `handleAI` |
| 8 | ~~Follow-up — `hash_include` for shared-module hash coverage~~ — done (#585) | — |

## Open questions

- **Single-session-per-source (#283)** becomes "one active suspended authoring run per
  source" — the concurrency rule must carry over.
- **Provider posture — decided:** the flagship authoring backend is **Claude via
  subscription** (`ai-agent-claude-cli`), reaching dicode through the MCP surface. The MCP
  mount is now correct (`--strict-mcp-config --mcp-config`; the old reliance on auto-loading
  `<cwd>/.claude/mcp.json` was a no-op). Remaining sub-question: an acceptable default when
  no `CLAUDE_CODE_OAUTH_TOKEN` is configured. Tool-restriction + per-agent MCP scoping stay
  in #560.
- **Auto-wire `on_failure_chain: auto-fix`** as a default vs. keep opt-in — carries
  `git_commit_push` blast radius.
