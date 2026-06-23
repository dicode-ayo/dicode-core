---
name: dicode-auto-fix
description: Mandatory workflow for the on-failure auto-fix loop. Diagnose a failed run, edit source on a fix branch, validate via tests + replay, push, optionally PR.
---

# dicode auto-fix loop

You are running inside the `auto-fix` task, fired by another task's failure
via `on_failure_chain`. Your job is to diagnose the failure, write a
narrowly-scoped fix to the failing task's source, validate it with tests
and replay, and either push to main (autonomous) or open a PR (review).

## Iteration loop (cap: `max_iterations`, default 5)

1. **Read context.**
   - Read `taskID`, `runID`, `status`, `output`, and `mode` from the input map.
   - Call `dicode.runs.get_input(runID)` — receives `{ input, redacted_fields }`.
     If `auto_fix.include_input: false` was set, you get only the failure logs
     and output; respect that and reason from logs alone.
   - Note the redacted-field names; if the failure depends on a redacted field
     (a signature, a token), say so plainly in the PR body — reviewers need that signal.
2. **Pin the input.**
   - Call `dicode.runs.pin_input(runID)` — keeps the blob alive while you work.
   - Set up `defer cleanup()`: in JavaScript terms, register a try/finally so
     `dicode.runs.unpin_input(runID)` runs on success, error, or timeout.
3. **Open a fix branch.**
   - In `mode: review`: use `${branch_prefix}${runID}` (default `fix/<runID>`).
   - In `mode: autonomous`: use the source's tracked branch (`base_branch`).
   - Call `dicode.sources.set_dev_mode(<source>, { enabled: true, branch: <fixBranch>, base: <base>, run_id: <runID> })`.
4. **Iterate** (cap each iteration at `max_iteration_seconds`, default 300s):
   - Read failing-task source files via `Deno.readTextFile` from `${DATADIR}/dev-clones/<source>/<runID>/`.
   - Edit via `Deno.writeTextFile` — **only inside the failing task's directory.**
     Do NOT write outside the failed task's directory; cross-task edits are out
     of scope and will land you in trouble.
   - Validate inline: re-parse `task.yaml`, re-typecheck `task.ts`.
   - Test: `dicode.tasks.test(<failingTaskID>)`. If no test exists, write one.
   - Replay: `dicode.runs.replay(<failedRunID>)` with the original failed run
     ID (the engine looks it up by lineage; the replayer accepts because your
     parent_run_id is that run). Wait for terminal status.
   - If both green → exit loop.
5. **Commit + push.**
   - `dicode.git.commit_push(<source>, { message, branch: <fixBranch> })`.
6. **Open the PR (review mode only).**
   - `dicode.run_task("git-pr", { source_id, branch: fixBranch, base, title, body, clone_path: <path you cloned into> })`.
   - Pass `clone_path` explicitly — the legacy first-readDir fallback in
     `git-pr` is brittle when prior runs left orphan clone directories.
   - Body must mention any redacted_fields you saw — reviewers need that signal.
7. **Disable dev mode.**
   - `dicode.sources.set_dev_mode(<source>, { enabled: false, run_id: <runID> })` — engine
     removes the local clone; the remote branch is retained.
8. **Unpin input** (the deferred cleanup also handles the timeout/panic case).

## Token / iteration budget

- `max_tool_iterations: 30` (overrides ai-agent's default 10) — fix loops
  chatter more than chat sessions.
- `max_tokens: 50_000` per run — keep prompts focused; avoid re-reading
  files you already have in working memory.

## Hard rules

- **One task, one fix.** Do not edit dependency tasks, secrets, or anything
  outside the failing task's directory.
- **No `--force` push.** The engine refuses it; your call will error and you
  must abandon the iteration.
- **Branch protection.** Autonomous mode pushes directly to the source's
  tracked branch; this assumes branch protection is configured on the forge.
  Review mode is the safe default.
- **Stop on storm.** The engine's circuit breaker will refuse new fires from
  a flapping source; if you see `storm circuit breaker tripped` errors,
  exit cleanly and let humans triage.
