# Agent Skill

The dicode agent skill is a markdown document that gives any AI agent full context to develop tasks correctly — without human guidance. It is embedded in the dicode binary and can be installed as a skill for Claude Code or any compatible agent.

---

## Installing the skill

```bash
# Print to stdout
dicode agent skill show

# Install globally for this user
dicode agent skill install

# Install as a Claude Code skill
dicode agent skill install --claude-code
```

The `--claude-code` flag writes the skill to `~/.claude/skills/dicode-task-developer.md` where Claude Code automatically picks it up as an available skill.

---

## What the skill covers

1. **Mandatory workflow** — the exact sequence of MCP tool calls to follow, in order, every time. No shortcuts.

2. **Hard rules** — things the agent must never do:
   - Never commit if `validate_task` or `test_task` fail
   - Always write tests
   - Never hardcode secrets
   - Return JSON-serializable values only
   - Check existing tasks before naming a new one

3. **`task.yaml` schema** — all fields, required vs optional, valid values for `trigger.chain.on`, cron syntax reference, common mistakes (two triggers, invalid cron, missing `from`)

4. **JS globals reference** — all globals with type signatures:
   - `http.get(url, opts?)`, `http.post(url, opts?)`, response shape
   - `kv.get(key)`, `kv.set(key, value)`, `kv.delete(key)`, `kv.list(prefix)`
   - `env.get(key)` — only for keys declared in `task.yaml`
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
