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

Everything below is done by calling a tool. Prose in this skill that names a
tool means *call that tool* — you cannot run code, read a file, or reach the
dicode SDK any other way.

1. **Read context.**
   - Read `taskID`, `runID`, `status`, `output`, and `mode` from the input map.
   - `dicode_get_run_input(run_id)` returns `{ input, redacted_fields }`.
     If `auto_fix.include_input: false` was set, you get only the failure logs
     and output; respect that and reason from logs alone.
   - Note the redacted-field names; if the failure depends on a redacted field
     (a signature, a token), say so plainly in the PR body — reviewers need that signal.
2. **Pin the input.**
   - `dicode_pin_run_input(run_id)` keeps the blob alive while you work.
   - Call `dicode_unpin_run_input(run_id)` before you finish, on every path —
     including the one where you give up.
3. **Open a fix branch.**
   - In `mode: review`: use `${branch_prefix}${runID}` (default `fix/<runID>`).
   - In `mode: autonomous`: use the source's tracked branch (`base_branch`).
   - `dicode_set_dev_mode(source, enabled=true, branch=<fixBranch>, base=<base>)`
     clones the source and returns `clone_path`. Keep that value: it is the
     directory every later step refers to, and nothing else tells you where the
     clone landed.
4. **Iterate** (cap each iteration at `max_iteration_seconds`, default 300s):
   - Edit **only inside the failing task's directory** under `clone_path`.
     Cross-task edits are out of scope and will land you in trouble.
   - Test: `dicode_test_task(<failingTaskID>)`. If no test exists, write one.
   - Replay: `dicode_replay_run(<failedRunID>)` with the original failed run
     ID (the engine looks it up by lineage; the replayer accepts because your
     parent_run_id is that run). It returns a new run id — poll it with
     `dicode_get_runs` until that run reaches a terminal status.
   - If both green → exit loop.
5. **Commit + push.**
   - `dicode_git_commit_push(source_id, message=…, branch=<fixBranch>)`. The
     branch prefix is fixed by the agent's configuration, not by you: a push to
     a branch outside it is refused.
6. **Open the PR (review mode only).**
   - `task_buildin_git-pr` with `source_id`, `branch`, `base`, `title`, `body`,
     and `clone_path` — the value step 3 returned.
   - Pass `clone_path` explicitly — the legacy first-readDir fallback in
     `git-pr` is brittle when prior runs left orphan clone directories.
   - Body must mention any redacted_fields you saw — reviewers need that signal.
7. **Disable dev mode.**
   - `dicode_set_dev_mode(source, enabled=false)` — the engine removes the local
     clone; the remote branch is retained.
8. **Unpin input** (step 2's cleanup, on every exit path).

## Token / iteration budget

- `max_tool_iterations: 30` (overrides ai-agent's default 10) — fix loops
  chatter more than chat sessions.
- `max_tokens: 50_000` per run — keep prompts focused; avoid re-reading
  files you already have in working memory.

## Hard rules

- **A tool you were not given does not exist.** Your tool list is the whole of
  what you can do. If a step here needs something absent from it — reading or
  writing a file among them — stop and report that plainly. Never describe an
  edit you did not make.
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
