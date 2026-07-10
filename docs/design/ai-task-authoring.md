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
| `buildin/ai-agent-claude-cli` (Claude Pro/Max, `CLAUDE_CODE_OAUTH_TOKEN`) | ✅ | ✅ | **Ungoverned.** `claude -p` runs with its full default tools (Read/Write/Edit/Bash) — the task passes no `--allowedTools`/`--permission-mode`, so as a subprocess it has host-wide fs/bash as the daemon user. A privilege hole; **must be restricted + routed through MCP** (#560). |

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
