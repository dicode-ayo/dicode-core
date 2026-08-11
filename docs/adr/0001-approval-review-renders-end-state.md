---
status: accepted
---

# The approval review surface renders end state, not a diff

An operator approving a pending task is deciding **what will run if I arm this**, not
**what changed since I last looked**. We render the resolved task — runtime, triggers,
effective permissions, params, env declarations, timeout — plus a file inventory, and
reduce "what changed" to a decoration strip carrying the commit range and a link to the
git host's compare view.

## Considered options

We shipped the diff first (#604 / PR #636): per-task in-memory content snapshots,
redacted, diffed, hunked, rendered. 3,610 lines and twelve follow-ups. A second design
kept the diff but sourced it from git rather than from snapshots, which fixed the
follow-ups without changing the question being answered.

End state won on three counts the diff could not reach. It needs no baseline, so a local
source, a rewritten history, or a divergent branch still produces a complete screen — the
diff produces a blank one. It *is* the checkout, so a file committed nowhere is simply
present rather than being a case the diff can structurally never show. And it opens no
blob, so there is no unbounded-memory diff, no size cap, and no "too large to display"
state to design around.

It also survives accumulation. Changes pile up across many commits between approvals, so
"what changed" is an n-commit range that grows with how long the operator waited, while
"what will run" is the same size every time.

## Consequences

The diff a reviewer actually reads lives on the git host, where it has line comments,
blame, and CI status. This is a deliberate cession, and it depends on ADR-0002.

End state is derived entirely from a **parsed** spec, so a task whose configuration fails
to parse has nothing to render at all. The diff could at least have shown bytes. This
makes surfacing load failures in the UI a precondition of the review surface rather than
an independent nicety.

Implementation tracked as epic #667.
