---
name: Update agent skill when relevant capabilities change
description: When adding or changing any feature the internal AI agent uses to build tasks, update pkg/agent/skill.md to reflect it
type: feedback
---

Always update `pkg/agent/skill.md` when work touches anything the internal AI agent needs to know to build tasks correctly.

**Why:** The skill file is loaded as the agent's system prompt. If it's out of date the agent will produce incorrect task.yaml or task.js, miss new fields, or fail to follow new patterns (e.g. webhook_secret was added but the skill had no mention of it until manually requested).

**How to apply:** After any PR that adds or changes:
- A new trigger type or trigger field (e.g. `webhook_secret`, daemon, chain options)
- A new JS global or change to an existing one
- A new task.yaml field (env, params, timeout, runtime options)
- A new security requirement that affects how tasks are written
- A new pattern or convention visible in task.js / task.test.js

...update the relevant section of `pkg/agent/skill.md` and commit it in the same PR or as an immediate follow-up. Include a worked example if the feature has non-obvious usage.
