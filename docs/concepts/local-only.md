# Local-Only Mode

Git is fully optional. You can run dicode entirely on your machine with no GitHub account, no remote repository, and no internet connection. Everything works: task execution, secrets, AI generation (requires API key), MCP, testing, tray icon, WebUI.

---

## What you get without git

| Feature | Local-only | With git |
|---|---|---|
| Task execution | ✅ | ✅ |
| JS runtime + all globals | ✅ | ✅ |
| Secrets (local encrypted store) | ✅ | ✅ |
| WebUI + REST API | ✅ | ✅ |
| MCP server | ✅ | ✅ |
| AI generation | ✅ | ✅ |
| System tray | ✅ | ✅ |
| Testing (validate + test + dry-run) | ✅ | ✅ |
| Instant reload on file save | ✅ | ✅ |
| Webhook relay | ❌ (no account) | ✅ |
| Version history / rollback | ❌ | ✅ (git) |
| Shared tasks (team) | ❌ | ✅ |
| CI integration | ❌ | ✅ |

---

## Minimal config

Auto-generated on first run when you choose "local only" in the onboarding wizard:

```yaml
sources:
  - type: local
    path: ~/dicode-tasks
    watch: true

database:
  type: sqlite
```

Everything else is optional. You can add AI, notifications, and other features incrementally.

---

## First-run onboarding

When no `dicode.yaml` exists, dicode opens the onboarding wizard:

```
Welcome to dicode

How do you want to store your tasks?

  ○ Local only   — no accounts needed, tasks stay on this machine
                   great for personal automation, try it out

  ○ Git repo     — tasks versioned in GitHub/GitLab
                   shareable, auditable, works with CI

[Get started]
```

**Choosing "Local only":**
1. Creates `~/dicode-tasks/` directory
2. Writes minimal `dicode.yaml` with single local source
3. Opens WebUI — ready to create your first task

**Choosing "Git repo":**
1. Prompts for repo URL and auth token
2. Clones repo into `~/.dicode/repos/`
3. Writes full `dicode.yaml`
4. Opens WebUI

Both paths land on the same experience. The wizard runs once; subsequent starts skip it if `dicode.yaml` exists.

---

## Migrating to git later

When you're ready to version-control your tasks:

1. Add a git source to `dicode.yaml`:
   ```yaml
   sources:
     - type: local
       path: ~/dicode-tasks
       watch: true
     - type: git
       id: my-repo
       url: https://github.com/you/tasks
       branch: main
       auth:
         type: token
         token_env: GITHUB_TOKEN
   ```

2. Commit each local task to the git source:
   ```bash
   dicode task commit morning-email-check --to my-repo
   ```

3. Once all tasks are committed, remove the local source from config.

Tasks don't need to change — the same `task.yaml` and `task.js` work in both modes. The only thing that changes is where they're stored.

---

## AI generation in local-only mode

AI generation works the same way — generated files are written to the local source directory. The task is live immediately via fsnotify.

Requires `ANTHROPIC_API_KEY` in your environment or secrets store:
```bash
dicode secrets set ANTHROPIC_API_KEY sk-ant-...
```

Then in `dicode.yaml`:
```yaml
ai:
  provider: anthropic
  model: claude-sonnet-4-6
  api_key_env: ANTHROPIC_API_KEY
```

---

## Webhooks in local-only mode

Webhook tasks work locally — `POST http://localhost:8080/hooks/{path}` triggers them. But they can't receive webhooks from the internet (GitHub, Stripe, etc.) unless you either:

- Expose port 8080 (requires router configuration, fixed IP)
- Add a webhook relay account (see [Webhook Relay](./webhook-relay.md))

For purely local workflows (cron, manual, chain), this is not a concern.
