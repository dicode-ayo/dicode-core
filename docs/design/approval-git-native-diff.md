# Git-Native Approval Diff

## Problem

The trust-on-change approval gate holds a task pending when its content hash
changes and does not arm its triggers until an operator approves. To approve
responsibly the operator needs to see *what changed* — and `dicode.lock` stores
only a hash, never file bytes, so by the time a task is pending the working tree
already holds the new content and there is no "before" on disk.

The shipped answer (#604 / PR #636) reconstructs the "before" itself:
`pkg/approval.Gate` keeps in-memory content snapshots per task, refreshed on
every approve and on every already-approved re-admit, and `Gate.Diff` renders
them. Everything downstream follows from that choice:

- the baseline is **in-memory only**, so a daemon restart loses it — and
  `git pull` → restart → review is the ordinary workflow (#642);
- the snapshot holds **raw file bytes**, which must then be redacted before
  reaching the session-less `/approve/{token}` page, and the redaction is a line
  pattern that multiline scalars and `- KEY=VALUE` env shorthand walk straight
  past (#643);
- a task that fails to parse drops out of the snapshot, destroying the baseline
  and reporting it as a routine restart (#648);
- security relevance is decided by **grepping the rendered diff text**, so it
  fires on `// TODO: net: ...` in a comment (#651);
- change sites the directory snapshot cannot see — taskset overrides,
  `hash_include` targets — need a synthetic `(resolved config)` entry to be
  visible at all (#646).

Twelve follow-ups are open against that surface. Most are not defects in the
implementation; they are consequences of maintaining a private, unversioned,
in-memory copy of history in a system whose sources are **already git
repositories with full history on disk**.

This document proposes deriving the baseline from git instead.

---

## Design

Three independent pieces, each replacing one job the snapshot currently does.

### 1. The baseline is a commit

`dicode.lock` records, per task:

```yaml
tasks:
  repo/deploy:
    hash: <content hash>          # unchanged — governs execution
    commit: <40-hex sha>          # new — what the approved content was
    branch: main                  # new — retrieval hint, see §3
    approved_by: manual
    approved_at: 2026-07-28T...
```

The diff is then `DiffTree(approvedCommit.Tree(), currentCommit.Tree())` scoped
to the task's directory prefix. `go-git/v5` v5.19.1 has the whole surface
already: `Tree.Diff` / `DiffTreeWithOptions`, `Change.Patch()`,
`Changes.Patch()`, `Patch.String()`, `ResolveRevision`, `CommitObject`.

This works because **clones are full**. `internal/gitops/clone.go`:

> Clones are full (no Depth limit) so that go-git's PullContext can always
> compute a merge base when the remote advances. See #175.

so the history needed to diff is, in the ordinary case, already on disk.

**The commit is captured at pend time, not at approve time.** `pendingEntry`
already carries `hash` and `files` written together in one critical section,
with a doc comment explaining why they can never disagree on generation — that
invariant exists because an earlier race let `approve()` promote a snapshot
stale relative to the hash it was matched against. Reading `HEAD` at approve
time reintroduces exactly that race: a pull can land between admit and approve,
and the recorded commit would not correspond to the hashed content.

### 2. Resolved settings are compared structurally, not textually

The content hash folds in more than the task directory: the resolved
permissions, runtime, params, timeout and trigger, which taskset overrides can
rewrite from outside the directory entirely, and `hash_include` targets that
live at other paths.

These get their own comparison, on **parsed values** rather than rendered text.
`resolvedSecurityFields` already enumerates the fields; `approvedResolved`
already stores the resolved form — in memory. Persist it alongside the lock
record and compare field by field.

This subsumes three follow-ups at once: it is how override changes become
visible (#646), and comparing parsed values rather than grepping rendered text
is the actual fix for security flagging firing on comments (#651).

### 3. Retrieval ladder

Diffing is a purely local computation. `Repository.CommitObject(h)` is
`object.GetCommit(r.Storer, h)` — the local object storer, no network. Git's
wire protocol has no diff operation at all (`upload-pack` advertises refs,
negotiates want/have, ships a packfile), which is why GitHub and GitLab expose
compare as a REST API: their servers compute it out of band.

So having both objects locally is a hard precondition, and when it fails:

1. **Object is local** — the common case. No network.
2. **Fetch the exact SHA** (`<sha>:refs/dicode/baseline/<task>`). Cheapest, but
   go-git returns `ErrExactSHA1NotSupported` unless the server advertises
   `allow-reachable-sha1-in-want` or `allow-tip-sha1-in-want`
   (`remote.go:1190`). Stock git has `uploadpack.allowReachableSHA1InWant` off
   by default, so this cannot be the only rung.
3. **Fetch the recorded branch.** Always servable, costs more. This is what the
   `branch` field in the lock record is for — it is a retrieval hint, not part
   of the commit's identity.
4. **Honest failure** — `Incomplete` + a reason naming the real cause,
   one-click approval withheld.

Constraints on the fetch:

- **Fetch at pend time, not at review time.** The reconciler already does
  network I/O; the approve screen must not. This also means the session-less
  `/approve/{token}` page never triggers an outbound fetch, which would
  otherwise be a request an unauthenticated caller can cause.
- **Route through `internal/gitops`, never go-git directly.**
  `ValidateRemoteHost` is the only guard `ssh://` and SCP-shorthand remotes
  get; its own comment calls failing open there "a real, exploitable SSRF
  bypass".
- **Auth must reach it.** The fetch needs the source's credentials
  (`gitops.HTTPAuth(tokenEnv)`), and `pkg/approval` currently knows nothing
  about sources. This wants an injected `fetchBaseline` callback — the same
  decoupling the gate already uses for `arm` and `hashFn`. It is the first time
  the gate gets credentialed network access.
- **Pin what you fetch** into `refs/dicode/baseline/...` so it is not an
  unreferenced object subject to gc.
- **Back off.** A task pending across reconcile cycles must not fetch every
  30s; cache the failure per `(task, sha)`.

### 4. Approval stays bound to the content hash

The gate protects **what the daemon executes**, which is the working tree, not
the commit. `FireGuard` re-hashes the on-disk directory at fire time precisely
because "the runtime imports task files fresh on every run". A commit-range
review plus a content-hash-bound approve is coherent; a commit-range review
alone is not.

So the commit is for *display*; the hash remains the thing approval binds to
(#645). Nothing in this design changes that.

### 5. Local sources are ungated by default

Local folder sources have no commits, so none of the above applies to them.
They become ungated **by default policy**, not by deleting the gate's local
path: per-source trust in `dicode.yaml` already expresses both answers, and an
operator who wants their local folder gated keeps that option.

Accepted residual risk: for local sources, `FireGuard`'s fire-time re-hash
becomes the only thing between generated code and a scheduled run holding
credentials. This matters because local folders are where AI-authored tasks
land.

### 6. What the unauthenticated approve page renders

The `/approve/{token}` page has no session — the token is the only boundary. It
renders the commit range, the **structured resolved-settings diff** (safe to
redact properly, because it operates on parsed values rather than grepping
text), and a deep link to the host's compare view where one exists. Task file
content requires a session.

This is what actually closes #643: the session-less page stops rendering task
bytes at all, rather than rendering them through a redaction pass that keeps
being incomplete.

---

## What this removes

| Follow-up | Effect |
|---|---|
| #642 persist the snapshot | **Gone.** The baseline is a commit; objects are on disk. |
| #643 redact from the YAML node tree | **Gone.** The session-less page renders no file bytes. |
| #640 synthetic Monaco line numbers | **Gone** with the hunked-snapshot rendering. |
| #641 key the diff file list | **Gone** with the same. |
| #646 `hash_include` invisible | **Fixed** by §2. |
| #651 flagging is a text match | **Fixed** by §2. |
| #648 parse error destroys the baseline | **Fixed** — a parse failure no longer erases anything, because the baseline is not derived from parsing the current tree. |
| #645 bind approval to the reviewed hash | **Unaffected** — still required (§4). |
| #649 unparseable task vanishes from the UI | **Unaffected.** |
| #650 pending tasks presented as live | **Unaffected.** |
| #652 effective permissions not shown | **Unaffected.** |
| #639 `/login` passphrase | **Unrelated.** |

Deleted machinery: the per-task content snapshot maps, the streamed
fingerprints for capped/binary files, the byte-level redaction pass, the
`(resolved config)` synthetic entry (superseded by a real structured
comparison), and the `HasBaseline` in-memory caveat.

---

## What this does not solve

- **Working tree vs. HEAD.** The daemon runs the checkout, not the commit. A
  dirty clone, a partial pull, or an operator editing the checkout means the
  reviewed commit is not the executed artifact. Mitigated, not eliminated, by
  §4.
- **Rewritten history.** A force-push or rebase that orphans the approved
  commit, followed by remote gc, leaves the SHA a permanently dangling
  identifier. This is the terminal case of the ladder in §3.
- **Divergent branch switch.** Verified experimentally against the real
  clone/pull options: repointing a `SingleBranch` clone at another branch fails
  the pull with `object not found`, which `IsReclonableError` matches, so
  `CloneOrPull` wipes the directory and re-clones. If the approved commit is not
  an ancestor of the new tip it is absent locally — recoverable via rung 3,
  since the commit is still on the remote.
- **First review after adoption.** A task approved before this lands has a hash
  but no commit. Its first review under the new scheme has no baseline commit
  and must degrade honestly rather than badge every file as added.
- **Dir-less / inline taskset tasks.** No directory, no commit range.

## Deliberately not adopted

**Linking out to the host's compare view as the review surface.** The GitHub
compare API returns per-file patches, but: the changed-file list appears only on
the first page and is capped at 300 files, large diffs time out with a 5xx, and
there is no path/subdirectory parameter on either the API or the web URL — so a
task's file can be genuinely absent from the response in a large compare.
Self-hosted Gitea, bare repos over SSH and `file://` remotes have no web UI at
all. Computing the diff locally is strictly more capable and has no auth, rate
limit, or network dependency at review time. A deep link remains as a
convenience, not as the review surface.

---

## Open questions

1. What does a review render when the task changed but **no commit changed** —
   a dirty working tree, or a taskset override in a different repo? The
   structured settings diff covers the override case; the dirty-tree case has
   no "before" commit to name.
2. Migration: do existing lock records get a commit backfilled at first
   re-approval, or does the daemon attempt to resolve one at startup?
3. Does the deep link belong on the session-less page at all, given it leaks
   the repo URL and commit SHA to whoever holds the token?
4. Should the structured settings diff be persisted in `dicode.lock` (visible,
   diffable, human-editable) or in the database (larger, not hand-edited)?
