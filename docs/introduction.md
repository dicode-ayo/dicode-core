# Introduction

## What is dicode?

Dicode is an open-source, AI-native task orchestrator. It is a single Go binary that:

- **Watches one or more task sources** (git repos or local directories) and reconciles running tasks automatically — add a file, the task is live; delete a file, it stops. No restart needed.
- **Executes tasks** on a schedule (cron), via HTTP webhook, on demand, or when another task completes (chaining).
- **Generates task code from natural language** using Claude. Describe what you want; dicode writes the JavaScript, tests it, and deploys it.
- **Exposes an MCP server** so AI agents (Claude Code, Cursor, custom agents) can develop and deploy tasks autonomously using the full validate → test → dry-run → commit workflow.

Tasks are plain JavaScript files stored as folders in a git repo (or a local directory). There is no proprietary DSL, no drag-and-drop builder, and no vendor lock-in. You own the code.

---

## Why dicode?

### The gap

Existing automation tools force a choice:

- **No-code tools** (Zapier, Make, n8n in visual mode) — fast to start, hit a wall when logic gets complex, black-box workflows you can't version or test.
- **Code-first platforms** (Airflow, Prefect, Temporal) — powerful but require significant infrastructure (databases, worker clusters, cloud accounts) and steep learning curves.
- **Cron + scripts** — simple but no UI, no monitoring, no retries, no secrets management, no AI.

### The dicode position

Dicode is **code-first but infrastructure-free**. A single binary, no required database (SQLite embedded), no worker cluster, no cloud account. Drop it on a laptop or a VPS and it works.

```
No-code  ←————————————————————————→  Full code
Zapier    n8n    [dicode]    Temporal    Airflow
          ↑                      ↑
      visual/YAML            heavy infra
                 ↑
         code-first, zero infra
```

### The AI angle

Dicode is designed from the ground up for AI-assisted development. The MCP server exposes every operation (write, validate, test, run, commit) as a tool that an AI agent can call. The agent skill document gives any AI agent the context to develop tasks correctly without human guidance.

This is the north star: tell an AI agent what you want automated, and it handles the entire lifecycle — write, test, validate, deploy — using dicode's MCP tools.

---

## Core principles

**1. Git is the source of truth** (but not required)
Tasks live as files in a git repo. Version history, code review, rollback — all via standard git workflows. For users without a git account, a local directory works just as well.

**2. Code you can read and own**
AI generates JavaScript that you can inspect, edit, and understand. No proprietary workflow format. Tasks are portable — copy the folder anywhere.

**3. Single binary, zero mandatory infrastructure**
Runs on a developer's laptop (with a tray icon) or a headless VPS. SQLite is embedded. No Postgres, no Redis, no Kubernetes required.

**4. Testing is a first-class citizen**
Every task can have a `task.test.js` with mocked HTTP globals. The CI integration runs `dicode task validate --all && dicode task test --all` on every push. AI generates tests alongside task code.

**5. Open source, always free for self-hosters**
The full engine is open source (AGPL-3.0). Cloud tiers gate execution volume and user seats, never features. Self-hosted users get everything, forever.

---

## Key concepts at a glance

```
tasks git repo (or local dir)
└── morning-email-check/
    ├── task.yaml    ← trigger (cron/webhook/manual/chain), params, env vars
    ├── task.js      ← JavaScript logic, runs in goja (sandboxed)
    └── task.test.js ← unit tests with mocked http/env/params

dicode binary
├── Reconciler      watches sources, syncs task registry (ArgoCD-style)
├── JS Runtime      goja + http, kv, log, params, env, notify, dicode globals
├── Trigger engine  cron (robfig/cron), webhook, manual, chain
├── Secrets         provider chain: local encrypted SQLite → env vars → Vault/AWS SM
├── WebUI + API     HTMX frontend, REST API, live log streaming
├── MCP server      tools for AI agents: validate, test, dry-run, commit
└── Relay client    WebSocket tunnel to dicode.app for public webhook URLs
```

---

## Who is dicode for?

**Individual developers** automating personal workflows: email digests, API health checks, Slack bots, data exports, cron jobs that used to live in scattered shell scripts.

**Small teams** that want shared, versioned automation without a DevOps team or cloud budget.

**AI agent developers** building agentic systems that need a reliable, local task execution backend with MCP integration.

**Self-hosters and homelab enthusiasts** who want full control over their automation without sending data to a third-party SaaS.

---

## What dicode is not

- **Not a data pipeline tool** — no DAGs, no data lineage, no Spark integration. For ETL at scale, use Dagster or Prefect.
- **Not a CI/CD system** — tasks are automation scripts, not build pipelines. For CI/CD, use GitHub Actions or Dagger.
- **Not a serverless platform** — tasks run in a single process on your machine or server. For auto-scaling execution, use a serverless platform.
- **Not a workflow builder with a visual canvas** — if you want drag-and-drop, use n8n or Make.
