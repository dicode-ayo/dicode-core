# Upgrade notes

Behaviour changes that a release's `CHANGELOG.md` entry does not make obvious. `CHANGELOG.md` is generated from commit subjects; this file carries the "your setup may stop working" detail that belongs with a version but has no place in a one-line commit summary.

Newest release first.

## Unreleased

### `DICODE_DATA_DIR` now outranks `data_dir` in dicode.yaml

The CLI, the daemon and the first-run wizard each resolved the data directory from their own inputs, and they disagreed whenever both `DICODE_DATA_DIR` and a `data_dir` in `dicode.yaml` were set: the CLI took the environment variable, the daemon took the config. The CLI then dialed a socket no daemon was listening on and started a second daemon, which unlinked the live socket and rebound it — two daemons over one data directory.

One resolver now answers for all three, and the order is the one the CLI and the wizard already used: `DICODE_DATA_DIR`, then `data_dir`, then `$HOME/.dicode`.

**If you set both to different paths, the daemon moves.** It will read and write the directory named by `DICODE_DATA_DIR`, so its SQLite database, sources and run logs appear empty — the old ones are still at the `data_dir` path, untouched. Pick one:

- drop `DICODE_DATA_DIR` from the daemon's environment to keep using `data_dir`, or
- point both at the same directory.

Setting only one of the two is unaffected, and so is the Docker image, where `ENV DICODE_DATA_DIR=/data` and the generated config already name the same directory.

`${DATADIR}`, `database.path` and the AI scratch directory now follow `DICODE_DATA_DIR` as well; previously only `data_dir` moved them.

### Task subprocesses run in a process group of their own

Deno and Python task subprocesses are now started as process-group leaders, and the graceful stop after a run posts its result signals the whole group. A Python task runs as `uv run python`, so the process the daemon holds is a wrapper: signalling it alone left the interpreter running, and SIGKILL — which no wrapper can forward — orphaned it outright.

The group also means task subprocesses are no longer in the terminal's foreground group, so a Ctrl-C on a foreground `dicode daemon` no longer reaches them directly. Shutdown never relied on that: the run context's cancel kills them.

Per-child resource metrics (`/api/metrics`, `dicode status`) now sum the whole group, so a Python task's memory and CPU are reported instead of `uv`'s. Expect the numbers to rise for Python workloads — that is the task's real footprint, which was previously invisible.

### A second daemon no longer takes over a live control socket

Starting a daemon against a data directory that already has one running now fails with `control: a daemon is already listening on <path>` instead of unlinking the socket and rebinding it. Unlinking never disconnected the daemon behind it — it just left two processes serving one directory, with the CLI reaching whichever bound last.

A restart that races its own not-yet-exited predecessor will now fail rather than take over; retry once the old process has gone.

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

#### Allowlisting an internal git host

If you self-host git on a private network, list the specific hosts and CIDRs you trust:

```yaml
source_security:
  allow_internal_hosts:
    - git.corp.internal   # authorises ssh:// and git@host:path
    - 10.0.0.0/8          # ALSO required for http/https
```

Absent config means the guard stays fully closed, so an upgrade changes nothing until you opt in.

**An entry's kind determines its reach**, because the two guard layers match on different values:

- A **hostname** entry authorises `ssh://` and SCP-shorthand remotes. Those are checked only against the literal host string in the URL.
- `http`/`https` remotes are *additionally* re-checked at connection time against the **resolved IP**. For those you must also list the target's IP or CIDR.

A hostname entry alone never authorises the address it resolves to. That is deliberate: otherwise allowlisting a name would become a DNS-rebind bypass.

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

To find what is being held, `dicode task pending` lists each gate-held task with its short content hash, and `dicode list` marks them in an `APPROVAL` column.

If you script around `dicode`, an edit-then-run flow that used to succeed silently now needs an approve step between the two.
