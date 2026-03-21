# Business Model

Dicode is open-core: the full engine is free and open source (Apache 2.0). Revenue comes from cloud services layered on top.

---

## Free vs paid

| Feature | Free | Pro ($12/mo) | Team ($20/seat/mo) |
|---|---|---|---|
| Self-hosted, unlimited tasks | ✅ | ✅ | ✅ |
| Public git repos | ✅ | ✅ | ✅ |
| SQLite database | ✅ | ✅ | ✅ |
| Full JS runtime + all globals | ✅ | ✅ | ✅ |
| MCP server | ✅ | ✅ | ✅ |
| AI generation (BYOK) | ✅ | ✅ | ✅ |
| Webhook relay (500/mo) | ✅ | ✅ | ✅ |
| Private git repos | ❌ | ✅ | ✅ |
| PostgreSQL / MySQL backend | ❌ | ✅ | ✅ |
| Webhook relay (unlimited) | ❌ | ✅ | ✅ |
| Custom relay subdomain | ❌ | ✅ | ✅ |
| Webhook replay (48h / 7d) | ❌ | ✅ (48h) | ✅ (7d) |
| Managed cloud hosting | ❌ | ❌ | ✅ |
| Team collaboration, RBAC | ❌ | ❌ | ✅ |
| SSO / SAML | ❌ | ❌ | Enterprise |
| SLA | ❌ | ❌ | Enterprise |

**The core engine is never gated.** All execution, all globals, all MCP tools, all testing layers — free forever for self-hosters.

---

## What drives the free/paid line

**Free**: public git repos + SQLite. This covers 100% of personal automation use cases.

**Paid gate**: private git repos and bring-your-own database (Postgres/MySQL). These are the natural upgrade triggers:
- Private repos: you're using dicode for work, your tasks contain business logic you don't want public
- BYO DB: you want HA, multi-instance, or managed cloud

There are no artificial feature limits for self-hosters. The paid features require infrastructure that costs money to provide.

---

## Deployment profiles

**Desktop** — default for laptops:
- Tray icon (green/yellow/red status)
- OS desktop notifications
- Relay client (laptop behind NAT → public webhook URLs)
- Starts on login via `dicode service install`

**Headless** — for servers and Docker:
- `--headless` flag or `DICODE_HEADLESS=true`
- No tray, no desktop notifications
- Relay still available (or skip if server has public IP)
- Starts on boot via `dicode service install --headless`

**Managed cloud** (Team/Enterprise):
- Dicode hosted and managed by dicode.app
- Multi-region, HA Postgres backend
- Web-based onboarding (no binary to install)
- Shared task library for the team

---

## Webhook relay revenue

The relay (`relay.dicode.app`) is the primary monetization surface for individual users:

- **Free**: 500 webhook deliveries/month
- **Pro ($12/mo)**: unlimited deliveries + custom subdomain + replay
- **Team ($20/seat/mo)**: everything Pro + team relay namespaces

Most personal users stay free. Power users (integrating with GitHub webhooks, Stripe events, etc.) upgrade to Pro.

---

## Task marketplace (future)

A searchable registry at `dicode.app/store` where developers can publish and monetize task packages.

**Revenue sharing**: 70% to the author, 30% to dicode.app.

**Pricing models**:
- Free (open source tasks)
- One-time purchase ($5–$50)
- Subscription ($2–$10/mo for premium task collections)

**Discovery**: tasks searchable by category, tags, and natural language. "Find a task that posts GitHub release notes to Slack."

---

## Managed cloud (Team/Enterprise)

dicode.app hosts and operates a multi-tenant dicode cluster:
- Multi-region HA (Postgres backend, multiple worker nodes)
- Web-based setup — no binary to download or maintain
- SSO/SAML for enterprise
- Audit logs, RBAC, compliance features
- Auto-updates (always on latest version)

Price: $20/seat/month, minimum 3 seats. Enterprise: custom contract with SLA.

---

## Open source strategy

Apache 2.0 license — the most permissive common license. Anyone can:
- Self-host for any purpose, commercial or personal
- Fork and build a product on top
- Contribute back (encouraged, not required)

The open source binary is the growth driver — developers install it, love it, eventually need private repos or the relay or managed hosting. The free tier is genuinely useful and has no artificial limits.

**Not a bait-and-switch**: the core engine will never be moved behind a paywall. Only cloud services (relay, managed hosting, BYO DB support) are paid.
