---
status: accepted
---

# The trust boundary is the merge, and branch protection is a stated precondition

The approval gate is a **deploy guard**, not an adversarial review surface. Whatever
reaches a watched ref was merged by a person the git host authorised, with that host's
auth, attribution, and review tooling behind it. The gate exists to stop a merge from
becoming a scheduled run before a human has looked — not to stand between an attacker and
execution.

This holds for AI-authored tasks because AI authoring is routed through the same door: an
agent writes on a branch, opens a pull request, and gates on its tests; a human merges.
The agent must not hold a credential that can merge its own pull request or write the
watched ref, or the boundary becomes a token the agent has and the sentence "a human
merged it" stops meaning anything.

## The precondition

**The watched ref cannot be written except through review the git host enforces.** Where
that does not hold, code reaches execution having been read by nobody. dicode can observe
that choice and cannot prevent it.

We state this rather than verify it. Verification means a per-host branch-protection API —
GitHub's shape is not GitLab's is not Gitea's — inside a source layer that is deliberately
host-agnostic and today carries no vendor dependency at all. Such a check fails open on
every self-hosted remote, which is a warning absent exactly where it matters most. Saying
nothing was the third option and the worst: an unstated precondition makes the argument
for ADR-0001 unfalsifiable, and that argument is what justifies deleting roughly 2,500
lines.

## Consequences

Every piece of machinery that assumed a hostile committer comes out, unconditionally, for
every source kind — byte-level secret redaction, the hidden-content and incomplete-diff
contract, streamed fingerprints for size-capped files, and text-matched flagging of
security-relevant fields. They are removed not because every author is trusted, but
because each was bad on its own terms: the redaction guarded a page that now renders no
file content, and the flagging grepped rendered text closely enough to fire on comments.

Local sources have no host and no merge, so this argument does not cover them. They remain
the operator's responsibility. They still get a full review surface, because ADR-0001's end
state needs no baseline and no commit.
