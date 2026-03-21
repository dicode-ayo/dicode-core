# Dicode — Business Plan

## Mission

Make automation accessible to every developer — not just those with a DevOps team, a cloud budget, or time to learn a pipeline DSL. Write what you want in plain English. Dicode writes the code, tests it, and runs it.

---

## Product

Dicode is an open-source, AI-native task orchestrator. A single Go binary that:

- Watches git repos for task scripts and reconciles them automatically (GitOps-style)
- Executes tasks on a cron schedule, via webhook, or on demand
- Generates task code from natural language using Claude
- Exposes an MCP server so AI agents can develop and deploy tasks autonomously

See `README.md` for full technical documentation.

---

## Open Source Strategy

**The core engine is free forever.** No artificial feature limits on self-hosted deployments. This is non-negotiable — it drives adoption, community trust, and the task marketplace ecosystem.

Dicode is licensed under **Apache 2.0**. Enterprise features (RBAC, SSO, audit logs, SLAs) are built in a separate closed-source layer on top of the open core (open core model, similar to GitLab CE/EE).

### Why open source first

- **Distribution**: developers discover tools through GitHub stars, not ads. An open binary people can `go install` or `brew install` is the fastest path to users.
- **Trust**: automation tools touch secrets, APIs, and critical workflows. Open source lets users audit what they're running.
- **Task marketplace**: the community task store only has value if there's a large community. Open source builds that community.
- **MCP / agent ecosystem**: AI agents need open, inspectable tools. A closed binary is a harder sell to the agentic tooling space.

---

## Deployment Profiles

Three ways to run dicode. Same binary, same config format.

### 1. Desktop app

Runs on the user's personal machine. Tray icon, OS notifications, starts on login.

**Target user**: individual developer automating personal workflows (email digests, API monitors, Slack bots, cron jobs).

**Startup behavior**:

| OS | Mechanism | Command |
|---|---|---|
| macOS | `~/Library/LaunchAgents/app.dicode.plist` | `dicode service install` |
| Linux | `~/.config/autostart/dicode.desktop` | `dicode service install` |
| Windows | Registry `HKCU\...\Run` | `dicode service install` |

The tray icon right-click menu includes a "Start on login" toggle.

### 2. Headless server

Runs on a VPS, homelab, or as a Docker container. No tray, no desktop notifications. Managed as a system service.

Auto-detected when `$DISPLAY` is absent or `--headless` flag is set.

```bash
# Install as systemd service
dicode service install --headless
dicode service start
dicode service status
dicode service logs
```

```bash
# Docker
docker run -d \
  -e GITHUB_TOKEN=xxx \
  -e ANTHROPIC_API_KEY=xxx \
  -p 8080:8080 \
  -v ~/.dicode:/data \
  dicode/dicode
```

### 3. dicode.cloud (managed)

Fully hosted. No binary to install, no server to manage. Sign up at dicode.app, connect your git repo, start creating tasks.

---

## Free vs Paid Gate

The gate is architectural, not feature-based. Core functionality never locked.

### What's always free (self-hosted)

- Full task execution engine (all trigger types, chaining, JS runtime)
- Git + local sources, reconciler
- MCP server, agent skill
- Testing, validation, dry-run, CI generation
- Community task store (install/share)
- Tray icon, desktop + mobile notifications
- Local encrypted secrets (SQLite)
- Webhook relay (500 deliveries/month)

### What's paid

| Feature | Why it's a natural gate |
|---|---|
| **Private git repos** | Personal automation is free; business automation pays |
| **Bring your own DB** (Postgres/MySQL) | SQLite = single machine. External DB = production/HA/team |
| **Unlimited webhook relay** | Free relay drives adoption; power use pays |
| **Team RBAC** | Useless solo; essential for teams |
| **Shared secrets store** | Team credential management is an enterprise problem |
| **Audit logs** | Compliance requirement, not a personal need |
| **SSO / SAML** | Enterprise-only requirement |
| **Analytics + long retention** | Personal users don't need 1yr of run history |
| **AI generation credits** | Real cost; self-hosted users bring their own key |

---

## Webhook Relay

### The problem

Webhooks require a publicly reachable URL. A laptop behind home NAT doesn't have one. Today's solutions (ngrok, Cloudflare Tunnel, VPS) all add friction.

### The solution

Every dicode account gets a stable public URL:

```
https://dicode.app/u/{user_uid}/hooks/{task_path}
```

When a webhook hits this URL, `dicode.app` forwards it to the user's local dicode instance via a persistent WebSocket tunnel — identical in architecture to ngrok, Stripe's webhook CLI, and Cloudflare Tunnel.

```
GitHub push event
  → POST https://dicode.app/u/abc123/hooks/deploy-notify
      → dicode.app relay server
          → WebSocket tunnel (persistent, auto-reconnect)
              → user's local dicode instance
                  → task fires locally
```

The local dicode binary connects to the relay at startup (if account credentials are configured). Zero setup for the user.

### Why this is strategically important

1. **Solves a real problem** even for free users → drives account creation
2. **Every self-hosted user gets a dicode.app account** → conversion funnel
3. **Stable URLs** survive IP changes and ISP switches
4. **dicode.app sees webhook traffic** → analytics, replay, debugging (paid features)
5. **Natural upgrade trigger**: user hits 500/month limit on a free plan → upgrades

### Relay tiers

| | Free | Pro | Self-hosted server |
|---|---|---|---|
| URL format | `dicode.app/u/{uid}/...` | `{name}.dicode.app/...` | Your domain |
| Deliveries/month | 500 | Unlimited | Unlimited |
| Payload retention | None | 7 days | You manage |
| Replay failed webhooks | No | Yes | N/A |
| Latency guarantee | Best effort | Priority queue | Your infra |

---

## Database Abstraction

SQLite is the default (embedded, no setup). Paid plans unlock external databases for production deployments and team use.

```yaml
# free / default
database:
  type: sqlite
  path: ~/.dicode/data.db

# paid — postgres, mysql/mariadb
database:
  type: postgres
  url_env: DATABASE_URL
```

External DB unlocks:
- Multiple dicode instances sharing state (horizontal scaling)
- Existing corporate database infrastructure
- High availability (RDS, Cloud SQL, PlanetScale)
- Backup managed by the database provider

The database layer is abstracted behind an interface (`pkg/db/`) so swapping is config-only — no task code changes.

---

## Pricing Tiers

### Self-hosted (always free)

Full-featured. Public git repos. SQLite. 500 relay deliveries/month. Bring your own Anthropic API key for AI generation.

### Cloud Free

Hosted on dicode.app. No binary to install.

- 3 tasks
- 100 runs/month
- 10 AI generations/month
- Public git repos only
- 500 webhook relay deliveries/month
- Community task store
- 7-day run log retention

### Cloud Pro — ~$12/month

- Unlimited tasks
- Unlimited runs
- Unlimited AI generations
- **Private git repos**
- Unlimited webhook relay + custom subdomain + 7-day replay
- Managed secrets vault
- 90-day run log retention
- Priority support

### Cloud Team — ~$20/seat/month

Everything in Pro, plus:

- Multiple users
- **Role-based access control** (owner / editor / viewer)
- **Shared secrets store** with per-secret access controls
- **Audit log** (who ran what, when, with what params)
- Task ownership and assignment
- **Private team task store**
- 1-year log retention
- SSO (Google Workspace, Okta, Azure AD)

### Enterprise — custom pricing

Self-hosted or cloud. For organisations with compliance requirements.

- Everything in Team
- **SLA** (99.9% uptime guarantee)
- **Compliance exports** (SOC2 audit evidence, SIEM-compatible run logs)
- **Custom AI model** — bring your own Claude/OpenAI endpoint or fine-tuned model
- **On-premise secret backend** — dicode connects to existing Vault, AWS SM, or GCP SM
- **White-label / OEM** — embed dicode in your own product under your brand
- **Private task store mirror** — internal registry behind your firewall
- Dedicated Slack channel + named support engineer

---

## Task Marketplace

The community task store is free (just git repos). The marketplace is a curated layer on top.

### Tiers

| Tier | Who | Cost |
|---|---|---|
| **Community** | Anyone | Free |
| **Verified** | Reviewed by dicode team | Free to install |
| **Premium** | Third-party authors | Paid per install |
| **Private** | Organisation-internal | Team plan required |

### Revenue sharing

Premium task authors receive 70% of install revenue (similar to VS Code Marketplace, Shopify App Store). Dicode takes 30%.

This creates a flywheel:
```
Good tasks → more users → more installs → authors earn money
→ more authors → more good tasks → ...
```

### Discovery

- Search by tag, trigger type, runtime, author
- "Trending this week", "Most installed", "Curated collections"
- AI-powered: "install or generate?" — before generating a new task from scratch, the AI checks the marketplace for an existing match

---

## Distribution & Growth

### Primary channels

**GitHub** — open source repo is the primary acquisition channel. README → install → tray icon → dicode.app account (for webhook relay) → upgrade.

**Task marketplace** — viral. Users share tasks, others install them, see dicode.app branding.

**MCP ecosystem** — as AI agents proliferate, "dicode MCP server" becomes a standard tool in agent workflows. Claude Code, Cursor, and custom agents all benefit from having a local task executor.

**Developer communities** — Hacker News, Reddit (r/selfhosted, r/homelab, r/devops), Twitter/X, dev.to.

### The relay as a growth mechanism

Every desktop user who configures a webhook creates a dicode.app account. That account ties them to the platform, enables upgrade prompts, and gives dicode analytics on task usage patterns.

### Pricing philosophy

- **Never punish self-hosters.** The binary is fully featured. This builds trust.
- **Charge for infrastructure and collaboration**, not features. Teams and cloud users pay for what they actually cost us (compute, relay bandwidth, support).
- **AI is the only feature gate on self-hosted** — because AI calls have a real marginal cost.

---

## Competitive Position

| | dicode | Windmill | n8n | Airflow |
|---|---|---|---|---|
| Single binary | ✅ | ❌ (Postgres required) | ❌ | ❌ |
| Git as source of truth | ✅ | partial | ❌ | ❌ |
| Auto-sync from git | ✅ | manual deploy | ❌ | ❌ |
| AI task generation | ✅ | ✅ | ❌ | ❌ |
| MCP server | ✅ | ❌ | ❌ | ❌ |
| Desktop tray app | ✅ | ❌ | ❌ | ❌ |
| Webhook relay (no VPS needed) | ✅ | ❌ | ❌ | ❌ |
| Task marketplace | ✅ | ❌ | ❌ | ❌ |
| Task testing framework | ✅ | ❌ | ❌ | ❌ |
| Self-contained (no infra) | ✅ | ❌ | ❌ | ❌ |
| Open source | ✅ | ✅ | ✅ | ✅ |

**Core differentiator**: the only automation tool that works equally well on a developer's laptop (desktop app with tray) and a production server (headless/Docker), with no infrastructure to manage and AI that writes your automation code.

---

## Monetization Summary

| Revenue stream | Mechanism | Scale |
|---|---|---|
| Cloud subscriptions | Pro/Team/Enterprise SaaS | Primary |
| Webhook relay overage | Per-delivery above free tier | Secondary |
| Task marketplace | 30% of premium task revenue | Long-term |
| Enterprise licenses | Custom contracts, OEM | High-value |
| AI credits (cloud) | Usage-based generation limits | Bundled in Pro+ |

---

## Technical Additions for Business Features

### New packages

| Package | Purpose |
|---|---|
| `pkg/relay/` | WebSocket tunnel client (connects to dicode.app relay) |
| `pkg/db/` | Database abstraction (sqlite / postgres / mysql) |
| `pkg/service/` | OS service management (systemd, LaunchAgent, Windows Service) |
| `pkg/startup/` | Run-on-login management (XDG autostart, registry, LaunchAgent) |
| `pkg/auth/` | Account auth for dicode.app (JWT, token storage) |

### New CLI commands

```bash
# Service management
dicode service install [--headless]   # install as OS service
dicode service uninstall
dicode service start / stop / restart / status / logs

# Relay
dicode relay login                    # authenticate with dicode.app
dicode relay status                   # show tunnel status + URL
dicode relay logout

# Account
dicode account login / logout / whoami / upgrade
```

### New config fields

```yaml
database:
  type: sqlite              # sqlite | postgres | mysql
  path: ~/.dicode/data.db   # sqlite only
  url_env: DATABASE_URL     # postgres/mysql

relay:
  enabled: true             # default: true if account configured
  account_env: DICODE_TOKEN # env var holding account token

server:
  headless: false           # auto-detected; override with --headless flag
```
