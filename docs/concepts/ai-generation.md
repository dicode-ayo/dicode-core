# AI Generation

Describe what you want automated in plain language. Dicode calls Claude to generate `task.yaml`, `task.js`, and `task.test.js`, validates them, and shows you a diff to review before deploying.

---

## How it works

1. User types a prompt in the WebUI generate box (or calls `POST /api/generate`)
2. Dicode builds a prompt:
   - System context: JS globals reference, 2–3 example tasks, list of existing task IDs, list of available secrets
   - User prompt: the plain-language description
3. Claude generates `task.yaml` + `task.js` + `task.test.js`
4. Dicode runs Layer 1 validation (`validate_task`) on the result
5. If validation fails, the error is fed back to Claude for retry (max 3 attempts)
6. The generated files are shown as a diff in the WebUI
7. User reviews and confirms (or edits and re-validates)
8. On confirm: files are written to the local dev source
9. The local source picks up the change via fsnotify
10. The task appears in the registry, ready to run

---

## Configuration

```yaml
ai:
  provider: anthropic        # anthropic (default)
  model: claude-sonnet-4-6   # default
  api_key_env: ANTHROPIC_API_KEY
```

The API key is resolved from the secrets chain (not hardcoded in `dicode.yaml`).

---

## What the AI generates

For a prompt like: *"Check the Stripe API status page every 15 minutes. If any component is degraded or down, send an ntfy notification."*

**`task.yaml`:**
```yaml
name: Stripe Status Monitor
description: Monitors Stripe API status and alerts on degradation
trigger:
  cron: "*/15 * * * *"
env:
  - NTFY_TOKEN
params:
  - name: ntfy_topic
    default: "my-dicode-alerts"
```

**`task.js`:**
```javascript
const res = await http.get("https://www.stripestatus.com/api/v2/components.json")
if (!res.ok) throw new Error(`Status page unreachable: ${res.status}`)

const degraded = res.body.components.filter(c =>
  c.status !== "operational" && c.group === false
)

if (degraded.length > 0) {
  const names = degraded.map(c => c.name).join(", ")
  await notify.send(`Stripe degraded: ${names}`, { priority: "high" })
  log.warn(`Stripe components degraded: ${names}`)
} else {
  log.info("Stripe operational")
}

return { degraded: degraded.length, components: degraded.map(c => c.name) }
```

**`task.test.js`:**
```javascript
test("alerts when components are degraded", async () => {
  http.mock("GET", "https://www.stripestatus.com/api/v2/components.json", {
    status: 200,
    body: {
      components: [
        { name: "API", status: "degraded_performance", group: false },
        { name: "Dashboard", status: "operational", group: false }
      ]
    }
  })
  env.set("NTFY_TOKEN", "test-token")

  const result = await runTask()

  assert.equal(result.degraded, 1)
  assert.httpCalled("POST", "https://ntfy.sh/*")
})

test("no alert when all operational", async () => {
  http.mock("GET", "https://www.stripestatus.com/api/v2/components.json", {
    status: 200,
    body: {
      components: [
        { name: "API", status: "operational", group: false }
      ]
    }
  })
  env.set("NTFY_TOKEN", "test-token")

  const result = await runTask()

  assert.equal(result.degraded, 0)
  assert.httpNotCalled("POST", "https://ntfy.sh/*")
})
```

---

## Validation retry loop

If the generated code fails Layer 1 validation, the error is included in the next prompt:

```
Your previous response had validation errors:
- task.yaml: trigger.cron: "*/15 * *" is not a valid 5-field cron expression
- task.js: SyntaxError at line 7: Unexpected token '}'

Please fix these issues and regenerate all three files.
```

Up to 3 retry attempts. If all fail, the error is shown to the user with the last-generated files for manual review.

---

## MCP vs WebUI generation

Two paths to AI generation:

| | WebUI | MCP (agent) |
|---|---|---|
| Who triggers it? | Human user | AI agent |
| Confirmation? | User reviews diff and clicks confirm | Agent decides to commit |
| Use case | Interactive task creation | Autonomous task development |

The WebUI uses `pkg/ai/generator.go` directly. The MCP path uses `write_task_file` + `validate_task` + `test_task` + `commit_task` — the agent controls the loop.

---

## Prompt construction

The generator builds a structured prompt:

```
System:
You are a dicode task developer. Generate task.yaml, task.js, and task.test.js.

## JS Runtime Reference
[globals reference from tasks/skills/dicode-task-dev.md]

## Example tasks
[2-3 curated examples]

## Existing tasks
[list of task IDs in the registry]

## Available secrets
[list of secret names from secrets chain]

User:
[user's plain-language description]
```

The existing tasks list prevents ID collisions. The available secrets list lets Claude use real secret names in the generated `env:` declarations.
