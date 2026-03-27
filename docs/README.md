# Dicode Documentation

## Start here

- [Introduction](./introduction.md) — what dicode is and why it exists
- [Current State](./current-state.md) — what is built, what is stubbed, what is planned
- [Implementation Plan](./implementation-plan.md) — ordered build roadmap

## Concepts

| Document | What it covers |
|---|---|
| [Task Format](./concepts/task-format.md) | `task.yaml`, `task.js`, `task.test.js` |
| [JS Runtime](./concepts/js-runtime.md) | goja engine, all injected globals |
| [Sources & Reconciler](./concepts/sources.md) | git source, local source, reconciliation loop |
| [Triggers](./concepts/triggers.md) | cron, webhook, manual, chain |
| [Task Chaining](./concepts/task-chaining.md) | chain triggers, `dicode.trigger()`, pipeline north star |
| [Secrets](./concepts/secrets.md) | provider chain, local encrypted store, external providers |
| [Testing & Validation](./concepts/testing.md) | validate, unit tests, dry-run, CI |
| [Notifications](./concepts/notifications.md) | `notify` global, ntfy, tray icon, desktop |
| [Task → Orchestrator API](./concepts/orchestrator-api.md) | `dicode` global, progress, trigger, ask |
| [MCP Server](./concepts/mcp-server.md) | MCP tools, agent workflow |
| [Agent Skill](./concepts/agent-skill.md) | skill file, install, workflow rules |
| [AI Generation](./concepts/ai-generation.md) | Claude API, prompt building, validation loop |
| [Task Store](./concepts/task-store.md) | install, share, marketplace |
| [Web UI & API](./concepts/webui-api.md) | REST API, HTMX frontend |
| [Deployment](./concepts/deployment.md) | desktop, headless, Docker, systemd |
| [Local-Only Mode](./concepts/local-only.md) | no git required, onboarding, migration |
| [Webhook Relay](./concepts/webhook-relay.md) | relay architecture, dicode.app tunnel |
| [Business Model](./concepts/business-model.md) | tiers, marketplace, distribution |

## Runtime guides

- [Deno Runtime](./deno-runtime.md) — TypeScript/JavaScript, SDK globals, npm/jsr imports
- [Python Runtime](./python-runtime.md) — uv, PEP 723 inline deps, SDK globals
- [Podman Runtime](./podman-runtime.md) — rootless containers via Podman CLI

## Reference

- [Configuration Reference](./concepts/task-format.md#configuration-reference)
- [CLI Reference](./concepts/deployment.md#cli-reference)
- [JS Globals Reference](./concepts/js-runtime.md#globals-reference)
- [REST API Reference](./concepts/webui-api.md#api-reference)
