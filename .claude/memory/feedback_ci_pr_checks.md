---
name: Always check CI and update PR after commits
description: After every commit or push, check CI status and PR description; also update docs/ files to reflect completed features
type: feedback
---

After any commit to a feature branch, always:
1. Check CI task status (e.g. `gh pr checks`) and fix any failures before moving on
2. Review and update the PR description to reflect the latest state of the work
3. Update relevant docs/ files: `docs/current-state.md`, `docs/implementation-plan.md`, and `docs/README.md` if the feature is documented there

**Why:** This was a repeated pattern in the webhook UI feature — lint CI failed after the work was "done", and the PR description was outdated. Keeping docs and PR descriptions current prevents confusion about what's actually shipped.

**How to apply:** Treat CI checks and doc updates as part of the definition of done for any feature work, not as optional follow-up. Run `gh pr checks` after pushing and don't declare the task complete until CI is green and docs are updated.
