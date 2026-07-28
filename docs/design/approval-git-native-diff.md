# Approval Review: Minimal Git-Native Diff

## Problem

The approval gate holds a task pending when its content hash changes and does
not arm its triggers until an operator approves. To approve, the operator wants
to see what changed — and `dicode.lock` stores only a hash, so by the time a
task is pending the working tree already holds the new content and there is no
"before" on disk.

The shipped answer (#604 / PR #636) reconstructs the "before" itself: an
in-memory content snapshot per task, redacted before display, diffed and hunked
and rendered. 3,610 lines. Twelve follow-ups are open against it.

Almost none of those follow-ups are implementation defects. They are the cost of
a threat model the feature does not actually have.

---

## Threat model

**The approval gate is a deploy guard for a trusted author, not an adversarial
review surface.**

For a git source, the person who changed the task is a person with write access
to the repository. That access is the trust boundary, and it is enforced by the
git host — with its own auth, history, attribution, and whatever branch
protection the operator configured. The gate is not the thing standing between
an attacker and execution; it is the thing that stops a `git push` from becoming
a `cron` run before a human has looked.

Everything expensive in the shipped surface follows from assuming otherwise:

| Machinery | Assumes |
|---|---|
| Byte-level secret redaction | a hostile committer plants secrets to leak them through our renderer |
| `ContentHidden` / `Incomplete` contract | a hostile committer crafts content that renders as an all-clear |
| Streamed fingerprints for capped files | a hostile committer hides a change behind a size cap |
| `securityFieldPattern` text flagging | the reviewer cannot be trusted to notice a permissions change |

Against a trusted author each of those is machinery guarding a door that is not
the way in. The review surface's real job is: **remind me what I changed since I
last approved this.**

---

## Design

Record the commit at pend time, alongside the hash that already governs
execution:

```yaml
tasks:
  repo/deploy:
    hash: <content hash>
    commit: <40-hex sha>
    approved_by: manual
    approved_at: 2026-07-28T...
```

Render the diff with `go-git`: `DiffTree` between the approved commit's tree and
the current one, filtered to the task's directory prefix plus any resolved
`hash_include` paths. `Change.Patch()` already produces a hunked unified diff
with real line numbers, so we render its output directly.

This works because clones are full — `internal/gitops/clone.go:24`, "no Depth
limit… See #175" — so the history is on disk.

**Capture the commit in `pendingEntry`, not at approve time.** `pendingEntry`
already writes `hash` and `files` in one critical section, with a doc comment
explaining why they cannot disagree on generation; that invariant exists because
a race once let `approve()` promote a snapshot stale relative to its hash.
Reading `HEAD` at approve time reintroduces it.

**When the objects are not local, say so and move on.** No fetch ladder, no
retrieval hints, no background repair. Rewritten history, a divergent branch
switch, a source that is not git, a task with no recorded commit yet — all
render the same one-line statement that a diff is not available, and approval
proceeds normally. The operator reviews at the source. This is the honest
behaviour for a convenience feature, and it is one branch instead of a
subsystem.

**No redaction, no flagging, no completeness contract.** The diff shows the
commit range. If it cannot, it says so.

---

## What survives the reframing

Three things, none of which is about threat models.

**1. The `/approve/{token}` page renders no file content.** That URL arms a task
and travels through Slack, email, or ntfy. Whatever it displays is visible to
everyone with access to that channel — a disclosure question independent of who
is trusted to commit. It shows the task, the commit range, and a link. This is
why the redaction pass can be deleted rather than fixed: nothing on that page
renders repo bytes.

**2. Approval binds to the hash that was displayed (#645).** Correctness, not
protection: today the button can arm a version that was never on screen, because
the task can re-pend between the render and the click. Already implemented.

**3. Pending tasks must look pending (#650).** They currently render with a
green dot and a "Disable task" tooltip, advertise webhook URLs that 404, and
`Run` is a silent no-op. A convenience feature that hides its own state is worse
than no feature.

---

## What gets deleted

| Location | Removed | Kept |
|---|---|---|
| `pkg/approval/snapshot.go` | all 470 lines — `snapshotDir`, the file/size caps, `statFingerprint` / `contentFingerprint` / `streamFingerprint`, `redactValueLines`, `redactSecrets`, `yamlSecretScalars`, `collectSecretScalars` and their patterns | — |
| `pkg/approval/diff.go` | `securityFieldPattern`, `securityBlockPattern`, `touchesSecurityBlock`, `diffLineIndent`, `snapshotValuesEqual`, `snapshotDisplayText`, `renderedDiffHasContent`, `hunkSides`, `hunked`, `unifiedDiffText`, `resolvedConfigPath`, `maxInlineContentBytes`, `diffContextLines` | a much smaller `Diff` / `FileDiff`, `Gate.Diff` rewritten against `DiffTree` |
| `pkg/approval/gate.go` | `approvedFiles`, `approvedResolved`, `takeSnapshot`, `snapshotApproved`, `snapshotApprovedIfMissing`, `recordApprovedResolved`, `resolvedFieldsText`, `redactParamsForDisplay` | `resolvedFieldsOf` and `sanitizePermissions` — both are **content-hash** machinery, not display |
| `pkg/webui/approval.go` | the token page's file-diff template block | the confirm/redeem flow |
| `dc-task-detail.js` | Monaco diff editors, the text fallback renderer, the incomplete banner and its acknowledgement flow | a plain diff panel |
| `gate_diff_test.go`, `approval-diff.spec.ts` | the tests for all of the above | the pend → review → approve path |

Roughly 2,500 of #636's 3,610 lines.

Note `sanitizePermissions` and `resolvedFieldsOf` specifically: they look like
display helpers and are not. `ContentHash` must stay sensitive to a repointed
`params[].default`, which is why the redaction for display lives outside
`resolvedFieldsOf`. Deleting them changes what the gate holds on.

---

## Issue disposition

**Close — the mechanism they describe no longer exists**

- #640 synthetic Monaco line numbers — `Change.Patch()` carries real ones
- #641 key the diff file list — the panel that needed keying is gone
- #642 persist the snapshot — the baseline is a commit, not a snapshot
- #643 redact from the YAML node tree — nothing renders repo bytes to a
  session-less viewer
- #646 `hash_include` changes invisible — folded into the diff scope
- #651 security flagging is a text match — flagging removed

**Keep**

- #645 bind approval to the reviewed hash — implemented, ready to land
- #649 an unparseable task vanishes from the UI with no error — real bug,
  independent of rendering
- #650 pending tasks presented as live and healthy — see above
- #652 show effective permissions on the review screen — the highest-value
  remaining item under this framing: knowing what a task *can do* beats a
  prettier rendering of what changed
- #639 `/login` accepts any passphrase when none is configured — unrelated to
  this work, do not close

**Merge into #649**

- #648 a parse error destroys the baseline and reports it as a routine restart —
  the underlying event is #649's eviction. Under this design the harm changes
  shape: `Gate.Forget` calls `lock.Remove` (`gate.go:443`), so an eviction
  deletes the commit record and the task returns as brand new. One issue should
  cover "a parse failure evicts the task and erases its approval record."

---

## Open questions

1. **Migration.** Existing lock records have a hash and no commit. The v3 MAC
   covers canonical JSON of `map[string]Record` (`lock.go:181`) and `Record` has
   no `omitempty`, so adding fields naively makes every existing lock load as
   *tampered* — all records discarded, mass forced re-approval. Needs
   `omitempty` or a version bump, deliberately.
2. **Dirty working trees.** The daemon runs the checkout, not the commit, so a
   file present in the task dir but in no commit is folded into the hash forever
   and never appears in any commit range. Cheap guard: compare the task-dir hash
   computed from the recorded commit's tree against the pending hash, and state
   that the diff is partial when they differ. Worth it, or over-protection?
3. **Local sources.** They have no commits, so they always render "no diff
   available". Is that acceptable, or do they keep a directory snapshot? Note
   the tension: *"git already guards it"* argues for relaxing gating on **git**
   sources, but local folders — where AI-authored tasks land — are exactly where
   git guards nothing.
4. **Unbounded blob diffs.** `Change.Patch()` loads full blobs and diffs them in
   memory with no cap; `maxSnapshotFileBytes` went away with the snapshot. A
   large file in a task dir becomes an unbounded-memory diff.
