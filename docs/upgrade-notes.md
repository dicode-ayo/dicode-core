# Upgrade notes

Behaviour changes that a release's `CHANGELOG.md` entry does not make obvious. `CHANGELOG.md` is generated from commit subjects; this file carries the "your setup may stop working" detail that belongs with a version but has no place in a one-line commit summary.

Newest release first.

## 0.4.1

### Git remotes on internal hosts are refused

A git source pointing at a loopback, private, link-local, or otherwise internal address is now rejected before any clone or pull. A self-hosted git server on an RFC 1918 address (`10.x`, `192.168.x`, …), on `localhost`, or on a `*.internal` / `*.local` name will stop syncing, logging:

```
host "..." is a private or internal address; refusing to contact it
```

This closes an SSRF hole. Two guard layers now cover every scheme:

- `http` / `https` — rejected at dial time, on the *resolved* connection IP, so a hostname that resolves to an internal address is caught too (DNS rebind).
- `ssh` and SCP shorthand (`git@host:path`) — rejected on the *literal* host in the URL. This is the only guard these schemes get; a hostname that passes the literal check but resolves to a blocked address is not caught.

`http`/`https` sources on internal addresses have already been failing since the dial-time guard landed. What changes in 0.4.1 is that `ssh` and SCP-shorthand sources — previously unguarded at the clone path — now fail the same way.

**There is currently no allowlist or opt-out.** If you depend on an internal git host, do not upgrade past 0.4.0 until an operator-controlled allowlist ships. Progress is tracked in [issue #537](https://github.com/dicode-ayo/dicode-core/issues/537).

An `HTTPS_PROXY` / `HTTP_PROXY` host is exempt from the dial-time check, so an egress proxy on a private address keeps working.

### `dicode.run_task` blocks through a suspending child

`WaitRun` previously treated `suspended` as terminal: a parent whose child called `dicode.suspend()` got `{status: "suspended"}` back immediately, contradicting the documented "blocks until terminal" contract.

It now follows the resume chain and returns only when the child reaches a genuinely terminal state. The wait is bounded by the **calling task's own timeout**, not by the child's resume deadline, so a parent cannot hang indefinitely on a child nobody resumes — it fails on its own timeout instead.

A task that relied on observing `suspended` as a return value must now either set a timeout it is willing to wait for, or not wait on a child that suspends.

### Replaying a suspended run returns 409

`POST /api/runs/{runID}/replay` on a run in `suspended` status now returns **409 Conflict** and fires nothing. Previously it spawned a duplicate execution while the original run still held a live resume token.

The Web UI already hid the replay button for suspended runs, so this only affects direct API callers.

### Edited tasks require re-approval

The trust-on-change approval gate now re-hashes a task's directory at fire time. Editing a task's script (`task.js` / `task.ts` / `task.py`) — not just its `task.yaml` — correctly re-arms the gate, and the task is held pending until approved:

```
dicode task approve <task-id>
```

Previously a script-only edit left the resolved spec byte-identical, so the gate never re-armed and the edited code ran under the prior approval. Tasks from a source marked `trust: always`, and builtin tasks, remain exempt.

If you script around `dicode`, an edit-then-run flow that used to succeed silently now needs an approve step between the two.
