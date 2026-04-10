# Agent Skill

The dicode agent skill is a markdown document that gives any AI agent full context to develop tasks correctly — without human guidance. It is embedded in the dicode binary (`pkg/agent/skill.md`).

> **Note**: The skill references a full MCP tool workflow (`list_tasks` → `list_secrets` → `validate_task` → `test_task` → `dry_run_task` → `commit_task`). Only `list_tasks`, `get_task`, `run_task`, `list_sources`, and `switch_dev_mode` are currently implemented. The remaining tools are planned. The skill will need updating as tools are added.

---

## Installing the skill

CLI commands for `dicode agent skill show/install` are planned but not yet implemented. For now, copy the skill directly from the source:

```bash
# Copy the embedded skill to your project
cat pkg/agent/skill.md >> CLAUDE.md
```

---

## What the skill covers

1. **Recommended workflow** — the sequence of MCP tool calls to follow. Currently only `list_tasks`, `get_task`, and `run_task` are available; the full workflow (`validate_task` → `test_task` → `commit_task`) requires planned tools.

2. **Hard rules** — things the agent must never do:
   - Never hardcode secrets
   - Return JSON-serializable values only
   - Check existing tasks before naming a new one
   - Always write tests (when test harness is available)

3. **`task.yaml` schema** — all fields, required vs optional, valid values for `trigger.chain.on`, cron syntax reference, common mistakes (two triggers, invalid cron, missing `from`)

4. **SDK globals reference** — all globals with type signatures (Deno/Python):
   - `params.get(key)`, `params.all()` — task parameters
   - `kv.get(key)`, `kv.set(key, value)`, `kv.delete(key)`, `kv.list(prefix)`
   - `Deno.env.get(key)` / `env.get(key)` — only for keys declared in `task.yaml`
   - `params.get(name)`
   - `log.info/warn/error/debug(msg, data?)`
   - `notify.send(msg, opts?)`
   - `dicode.progress(msg, data?)`, `dicode.trigger(id, payload?)`, `dicode.isRunning(id)`
   - `input` — chain/webhook payload

5. **Test harness reference** — `test()`, `runTask()`, mock API, assert API

6. **Common mistakes table** — patterns to avoid and why:
   | Wrong | Right |
   |---|---|
   | `const key = "SLACK_TOKEN"` | `const key = env.get("SLACK_TOKEN")` |
   | `return [1, 2, 3, function(){}]` | `return [1, 2, 3]` (no functions in return) |
   | Two triggers in task.yaml | Exactly one trigger |
   | `http.mock("*")` matching nothing | Use specific patterns |

---

## The skill file

The skill is embedded in the binary at build time via `//go:embed skill.md` in `pkg/agent/skill.go`. This means the binary always ships with the correct skill for its version — no version mismatch between agent instructions and MCP tool behavior.

```go
// pkg/agent/skill.go
//go:embed skill.md
var Skill string
```

The `dicode agent skill show` command prints this string. The `dicode agent skill install` command writes it to disk.

---

## Using with Claude Code

After `dicode agent skill install --claude-code`:

In a Claude Code session, you can activate the skill:
```
/skills use dicode-task-developer
```

Or reference it in a prompt:
```
Using the dicode-task-developer skill, create a task that checks our API's health
endpoint every 5 minutes and sends a Slack alert if it's down.
```

Claude Code will follow the mandatory workflow: `list_tasks` → `list_secrets` → `get_js_api` → write files → `validate_task` → `test_task` → `dry_run_task` → `commit_task`.

---

## Using with a custom agent

```go
import "github.com/dicode/dicode/pkg/agent"

// agent.Skill contains the full skill markdown
systemPrompt := fmt.Sprintf("You are a task developer assistant.\n\n%s", agent.Skill)
```

The `Skill` string is the complete document — embed it in your agent's system prompt and connect the agent to dicode's MCP server.
