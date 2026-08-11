---
status: accepted
---

# The review surface renders declarations, never resolved secrets or code

Two invariants govern everything the operator sees when approving a task.

**No secret is dereferenced at render time.** An env entry that reads a value from
elsewhere renders as the declaration — the exposed name and where it comes from — and the
reference is never followed. Effective permissions still resolve, and taskset overrides
still resolve; those are policy and configuration, not values. Only the secret lookup
stops.

**No code bytes render in dicode.** The git host renders code better, and under ADR-0002
the operator has already been there.

## Why this instead of masking

Masking was the obvious alternative and it is what the previous design did: render
everything, then scrub it. That approach leaked twice — once through a parameter default,
once through an inline environment shorthand — because scrubbing is driven by patterns
over content the task author writes, and the author chooses the content.

Not resolving is checkable in one sentence. Masking is auditable only per surface, and the
audit has to be redone every time a surface is added.

## Do not reach for the run-log redactor

The existing redactor cannot serve this surface, and a future reader will reasonably assume
it can.

It is **value-based** — it string-replaces the specific secret values resolved for a run in
that run's log output — so it cannot mask a literal the author typed directly into a task's
configuration, because that string was never in the secrets store. That is precisely the
gap that leaked. It is also built **per run**, and a pending task has not run; constructing
one to draw a review page would mean decrypting a task's secrets in order to display it.

## Consequences

The session-less approval link renders only the task name, the commit range, and a link.
Whatever that URL shows is visible to everyone in the channel it travelled through, which
is a disclosure question independent of who is trusted to commit — so it stays minimal even
though the authenticated dashboard shows full end state.

Separately: the helpers that compute resolved fields and sanitise permissions for the
**content hash** are not display helpers, despite looking like them. The hash must stay
sensitive to a repointed parameter default, which is why display-time redaction was never
folded into them. Removing them changes what the gate holds on.
