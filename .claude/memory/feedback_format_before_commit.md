---
name: Always run formatter before committing
description: Run go fmt (or project formatter) before every commit — lint failures in CI are caused by unformatted code
type: feedback
---

Always format code before committing.

**Why:** Lint/format checks fail in CI when code is committed without formatting, causing broken pipelines.

**How to apply:** Before any `git commit`, run `make fmt` (or `make lint` which includes fmt + vet) to ensure all changed files are properly formatted. Do not skip this even for small changes.
