# Approval Review: End State, Not Diff

## Problem

The approval gate holds a task pending when its content hash changes and does
not arm its triggers until an operator approves. To approve, the operator wants
to know what they are arming — and `dicode.lock` stores only a hash, so by the
time a task is pending the working tree already holds the new content and there
is no "before" on disk.

The shipped answer (#604 / PR #636) reconstructs the "before" itself: an
in-memory content snapshot per task, redacted before display, diffed and hunked
and rendered. 3,610 lines. Twelve follow-ups are open against it.

Almost none of those follow-ups are implementation defects. They are the cost of
two mistakes: a threat model the feature does not have, and a question the
operator was not asking.

---

## Threat model

**The approval gate is a deploy guard, not an adversarial review surface.**

For a git source, the person whose change lands on the watched ref is a person
whose merge the git host authorised. That access is the trust boundary — with
its own auth, history, attribution, and review tooling. The gate is not the
thing standing between an attacker and execution; it is the thing that stops a
merge from becoming a `cron` run before a human has looked.

This holds under AI authoring because AI authoring is routed through it: an
agent writes on a branch, opens a pull request, and gates on its tests. It does
not merge. The human who merges is the author of record for anything that
reaches the watched ref.

### Stated precondition

**The watched ref cannot be written except through review the git host
enforces.** Branch protection is not incidental context here; it is the
assumption the rest of this document rests on. Where it does not hold — an
unprotected branch a deploy credential can push to directly — the consequence
is that code reaches execution having been read by nobody. dicode can observe
that choice and cannot prevent it, and this design does not pretend otherwise.

We state the precondition rather than verify it. Verification means a per-host
protection API — GitHub's shape is not GitLab's is not Gitea's — inside a
source layer that is deliberately host-agnostic (go-git, no git binary, no
vendor API anywhere today). A check that fails open on every self-hosted remote
is a warning absent exactly where it would matter most.

### What the model deletes

| Machinery | Assumes |
|---|---|
| Byte-level secret redaction | a hostile committer plants secrets to leak them through our renderer |
| `ContentHidden` / `Incomplete` contract | a hostile committer crafts content that renders as an all-clear |
| Streamed fingerprints for capped files | a hostile committer hides a change behind a size cap |
| `securityFieldPattern` text flagging | the reviewer cannot be trusted to notice a permissions change |

These are deleted unconditionally, for every source kind — not because every
author is trusted, but because each is bad machinery on its own terms.
Redaction guards a page that under this design renders no file content at all.
Flagging greps rendered text, so it fires on `// TODO: net: ...` in a comment.
Neither improves if the author is an agent.

---

## The reframe

Under the precondition above, the operator has already seen this change: on the
git host, with line comments, blame, and CI status. By the time dicode pends the
task, the diff has been reviewed and merged. Anything dicode renders is the
second look at something already looked at, and it will always be the worse of
the two.

A diff also answers the wrong question. Changes accumulate across more than one
commit, so "what changed since you last approved" is an n-commit range that
grows with how long you waited. The decision in front of the operator is not
*what moved* — it is **what will run if I arm this.**

So the review surface renders **end state**.

---

## Design

### What the screen shows

**End state — the resolved task.** Runtime and image, triggers with the concrete
cron expression or webhook URL, effective permissions after taskset overrides,
params with defaults, env declarations, timeout. Structured fields, bounded
size, no file content.

**A file inventory.** Names, sizes, per-file hashes — the one code-shaped fact
the spec cannot carry, so a new file is visible without rendering any code.

**A "what moved" strip.** Commit range, commit count since the approved commit,
per-file `new` / `changed` markers, and a link to the host's compare view.

Three properties follow, and they are the point:

**End state needs no baseline.** It renders from the checkout. A local source
with no commits, a source whose history was rewritten, a divergent branch, a
task with no recorded commit yet — every one of them still gets a complete
review surface. The "what moved" strip is decoration: when it cannot be
computed, it is absent, and the screen is *less contextual*, never blank.

**End state is the checkout, so a dirty tree is not a special case.** The daemon
runs the checkout, not the commit. A file present in the task directory but in
no commit is folded into the hash forever and could never appear in a commit
range — under end state it is simply there, because what is shown is what will
run.

**No blob is ever opened.** `DiffTree` yields name-status and blob sizes without
loading content. There is no unbounded-memory diff because nothing is diffed,
and no size cap, threshold, or "too large to display" rendering to design.

### Invariants

**No code bytes render in dicode.** Every deleted subsystem — the caps, the
redaction pass, the truncation banner, the Monaco editors — traces back to
rendering repository content in our own UI. The git host does it better, and
the operator has already been there.

**No secret is dereferenced at render time.** `EnvEntry` supports
`from: env:GH_TOKEN`. End state renders the declaration — `API_KEY ←
env:GH_TOKEN` — and never follows it. Effective permissions still resolve
(policy, not values) and taskset overrides still resolve (config, not values);
only the secret dereference stops at the declaration. This single line replaces
the entire deleted redaction pass.

`pkg/secrets/redactor.go` does not help here and should not be reached for. It
is value-based — `NewRedactor` string-replaces the values resolved for a run in
that run's log output — so it cannot mask a literal written inline in
`task.yaml` (the `- KEY=VALUE` shorthand, a `params[].default`), which was never
in the secrets store. It is also built per run, and a pending task has not run;
constructing one to draw a page would mean decrypting a task's secrets in order
to display it.

### Surfaces

| Surface | Renders |
|---|---|
| `GET /api/tasks/{id}/pending-state` | end state, inventory, moved strip |
| dashboard task detail | all of the above |
| `/approve/{token}` | task name, commit range, link — nothing else |

`/approve/{token}` is session-less by design; the URL travels through Slack,
email, or ntfy, so whatever it renders is visible to everyone in that channel.
It stays minimal for that reason alone, independent of who is trusted to commit.

`pending-diff` is replaced outright rather than aliased. Its only consumer is
the dashboard in this repository, and keeping the word "diff" in the codebase
for a surface that renders no diff is the residue that makes the next reader
reconstruct a deleted mental model.

### Recording the commit

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

**Capture it in `pendingEntry`, not at approve time.** `pendingEntry` already
writes `hash` and `files` in one critical section, with a doc comment explaining
why they cannot disagree on generation; that invariant exists because a race
once let `approve()` promote a snapshot stale relative to its hash. Reading
`HEAD` at approve time reintroduces it.

**Migration is one struct tag.** The v3 MAC covers canonical JSON of
`macPayloadV3{Bootstrapped, Tasks}` (`lock.go`), and `Record` carries no
`omitempty`. Adding `Commit string \`json:"commit,omitempty"\`` leaves existing
records marshalling to identical bytes, so every v3 lock still verifies and no
one is forced to re-approve. An empty commit is never a legitimate value for a
40-hex SHA, so `omitempty`'s usual ambiguity does not arise. Pin it with a
regression test that loads a lock written before the field existed and asserts
`Tampered()` is false.

This works at all because clones are full — `internal/gitops/clone.go:24`, "no
Depth limit… See #175" — so the history is on disk.

---

## What gets deleted

| Location | Removed | Kept |
|---|---|---|
| `pkg/approval/snapshot.go` | all 470 lines — `snapshotDir`, the file/size caps, `statFingerprint` / `contentFingerprint` / `streamFingerprint`, `redactValueLines`, `redactSecrets`, `yamlSecretScalars`, `collectSecretScalars` and their patterns | — |
| `pkg/approval/diff.go` | `securityFieldPattern`, `securityBlockPattern`, `touchesSecurityBlock`, `diffLineIndent`, `snapshotValuesEqual`, `snapshotDisplayText`, `renderedDiffHasContent`, `hunkSides`, `hunked`, `unifiedDiffText`, `resolvedConfigPath`, `maxInlineContentBytes`, `diffContextLines` | replaced by `Gate.State` over the resolved spec plus a `DiffTree` name-status walk |
| `pkg/approval/gate.go` | `approvedFiles`, `approvedResolved`, `takeSnapshot`, `snapshotApproved`, `snapshotApprovedIfMissing`, `recordApprovedResolved`, `resolvedFieldsText`, `redactParamsForDisplay` | `resolvedFieldsOf` and `sanitizePermissions` |
| `pkg/webui/approval.go` | the token page's file-diff template block | the confirm/redeem flow |
| `dc-task-detail.js` | Monaco diff editors, the text fallback renderer, the incomplete banner and its acknowledgement flow | an end-state panel |
| `gate_diff_test.go`, `approval-diff.spec.ts` | the tests for all of the above | the pend → review → approve path |

`sanitizePermissions` and `resolvedFieldsOf` look like display helpers and are
not. They are **content-hash** machinery: `ContentHash` must stay sensitive to
a repointed `params[].default`, which is why the redaction for display lives
outside `resolvedFieldsOf`. Deleting them changes what the gate holds on.

---

## Issue disposition

**Closed as superseded** — #640 (synthetic Monaco line numbers), #641 (key the
diff file list), #642 (persist the snapshot), #643 (redact from the YAML node
tree), #646 (`hash_include` invisible), #651 (flagging is a text match). Each
describes a mechanism this design removes rather than repairs.

**#648** — its premise does not hold. The reconciler's load-failure path only
logs (`reconciler.go:249`), and `Gate.Forget` is wired to `OnUnregister`, which
fires only when `task.yaml` leaves `ScanDir` entirely, so a parse failure evicts
nothing. Folded into #649, with one stake carried over: `Gate.Forget` calls
`lock.Remove` (`gate.go:249`), so a genuine eviction now deletes the recorded
commit and the task returns as brand new.

**#652 — show effective permissions on the review screen.** No longer a
follow-up. Under this design it *is* the review screen, and it is the item that
makes the whole surface worth rendering: knowing what a task can do beats any
rendering of what changed.

**#649 — an unparseable task vanishes from the UI with no error.** Promoted from
an independent bug to a precondition. `LoadKindedDir` failing means the task is
never registered (`reconciler.go:249-254`), so the gate never sees it — and
because the review surface is derived entirely from a *parsed* spec, there is
nothing this design could render in that state even if it did. The byte-diff
design could at least have shown bytes. This one cannot, so the silent-failure
path has to be closed for the surface to be trustworthy.

**#645 — bind approval to the reviewed hash.** Implemented, ready to land.
Correctness rather than protection: the button can otherwise arm a version that
was never on screen, because the task can re-pend between render and click.

**#650 — pending tasks presented as live and healthy.** They render with a green
dot and a "Disable task" tooltip, advertise webhook URLs that 404, and `Run` is
a silent no-op. A convenience feature that hides its own state is worse than no
feature.

**#639 — `/login` accepts any passphrase when none is configured.** Unrelated to
this work. Do not close.

---

## Open questions

1. **Pipelines.** `kind: Pipeline` has no runtime, image, or permissions, so its
   end state is a different shape — presumably its steps and the tasks they
   invoke. Whether that is one renderer with empty sections or two renderers is
   unresolved.
2. **Inventory provenance.** `task.ScanDir` already computes per-file hashes for
   the content hash. The inventory should reuse them rather than walking the
   directory a second time, but the gate does not hold that data today.
