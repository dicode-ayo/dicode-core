# Business Model

Dicode is open source (AGPL-3.0): the full engine is free to self-host. Revenue comes from cloud tiers that gate **volume and seats**, never features.

---

## Pricing principle

> Cloud tiers gate execution volume and user seats — never capabilities.
> Everything the engine can do is free, self-hosted, forever.

---

## Pricing tiers

| | Self-Hosted | Starter ($19/mo) | Pro ($49/mo) | Team ($149/mo) | Enterprise |
|---|---|---|---|---|---|
| **All features** | Yes | Yes | Yes | Yes | Yes |
| **Executions** | Unlimited | 10K/mo | 100K/mo | 500K/mo | Custom |
| **Users** | Unlimited | 1 | 5 | 20 | Unlimited |
| **Relay endpoints** | Self-hosted | 5 | Unlimited | Unlimited | Unlimited |
| **OAuth providers** | BYO apps | 5 | All 14 | All 14 | All 14 |
| **Managed database** | SQLite (local) | Managed Postgres | Managed Postgres | Managed Postgres | Dedicated |
| **Custom relay domain** | - | - | Yes | Yes | Yes |
| **Team features** | - | - | - | Approvals, RBAC | SSO/SAML, audit |
| **Support** | Community | Email | Priority | Dedicated | SLA |

---

## What drives the free/paid line

**Free (self-hosted)**: the full engine, all runtimes, all features. Covers 100% of personal and small-team automation use cases. No artificial limits.

**Paid (cloud tiers)**: you pay for infrastructure we run for you — managed relay, OAuth broker, database, backups, uptime monitoring. Plus execution volume and seat counts that naturally scale with usage.

The gate is **how much you use**, not **what you can do**.

---

## Revenue streams

### 1. Cloud hosting (primary)
Managed dicode instances with relay, OAuth, Postgres. The core revenue driver.

### 2. AI API routing (high potential)
If AI generation routes through our API, token costs can be marked up 20-40%. Users won't notice micro-cent differences, but at scale this compounds. BYO API key is always an option.

### 3. Relay as infrastructure
The webhook relay and OAuth broker are genuinely differentiated — no competitor bundles zero-config NAT traversal with multi-provider OAuth. This is the "why pay" answer.

### 4. Enterprise add-ons
SSO/SAML, audit logs, dedicated infrastructure, SLAs. Custom pricing for large organizations.

---

## Open source strategy

AGPL-3.0 license — copyleft that protects the project:
- Anyone can self-host, modify, and deploy dicode freely
- Anyone can fork and contribute
- Cloud providers cannot strip-mine the code into a proprietary service without sharing modifications
- The license ensures the code stays open while allowing commercial self-hosting

**Why AGPL over Apache 2.0**: Apache is too permissive for a solo/small team — any cloud provider could fork and out-market us with zero contribution back. AGPL follows the proven model of Grafana, MongoDB, and Windmill.

**Why AGPL over BSL**: AGPL is a real open-source license recognized by OSI. BSL (used by Sentry, CockroachDB) has clearer commercial protection but isn't truly open source. AGPL strikes the balance.

---

## Deployment profiles

**Desktop** — default for laptops:
- Tray icon with status
- OS desktop notifications
- Relay client (laptop behind NAT -> public URLs)
- Starts on login via `dicode service install`

**Headless** — for servers and Docker:
- `--headless` flag or `DICODE_HEADLESS=true`
- No tray, no desktop notifications
- Relay optional (skip if server has public IP)

**Managed cloud** (Starter/Pro/Team/Enterprise):
- Dicode hosted and managed by dicode.app
- Web-based onboarding (no binary to install)
- Managed relay, OAuth, database, backups
