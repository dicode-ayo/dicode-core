---
name: Use TypeScript for Deno task scripts
description: Always create task.ts (not task.js) for Deno runtime tasks
type: feedback
---

Always use `task.ts` when creating Deno task scripts, not `task.js`.

**Why:** User preference — they always work with TypeScript.

**How to apply:** Any time a new Deno task script is created, name it `task.ts`. Only use `.js` if the user explicitly asks for it.
