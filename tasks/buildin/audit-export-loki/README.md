# audit-export-loki

Ships the dicode security audit trail (#45) to [Grafana Loki](https://grafana.com/oss/loki/)
via its push API. Runs on a cron tick (default every 60s), pushes a batch,
and exits — dicode tasks are short-lived subprocesses, so this is
poll-batch-push, not a long-lived stream.

## How it works

1. Read the last shipped cursor from `dicode.kv` (key `cursor`).
2. `dicode.audit.query({ after: cursor, order: 'asc', limit: 1000 })` —
   the next batch in forward chronological order.
3. Map events to Loki streams. Labels are low-cardinality only
   (`job`, `event_type`, `actor_kind`, `target_kind`, `allowed`); the full
   JSON event (including `id`, `run_id`, actor/target ids) is the log line.
4. `POST {endpoint}/loki/api/v1/push`.
5. Advance the kv cursor **only after a 2xx**.

Delivery is **at-least-once**: if the push fails or the daemon dies between
the push and the cursor write, the same batch is re-sent next tick. Dedupe
downstream on the event `id` (Loki itself dedupes identical (timestamp,
line) pairs within a stream, and the `id` field makes records uniquely
identifiable for any downstream pipeline).

## Configuration

Set params via a taskset override or `dicode.yaml`:

| Param | Required | Notes |
|---|---|---|
| `endpoint` | yes | Loki base URL, e.g. `http://localhost:3100` or `https://logs-prod-eu-west-0.grafana.net`. |
| `tenant_id` | no | Multi-tenant Loki — sent as `X-Scope-OrgID`. |
| `batch_limit` | no | Events per run, 1..1000 (default 1000). |
| `auth_user` | no | Basic-auth username (Grafana Cloud: numeric instance ID). |

Auth token is **never** a param. Resolve it into `LOKI_AUTH_TOKEN` from the
secrets store (`dicode secrets set LOKI_AUTH_TOKEN ...`) or the host env.
When `auth_user` is set the token is the basic-auth password; otherwise it
is sent as a `Bearer` token. With no token, no auth header is sent (fine
for an unauthenticated local Loki).

### Network permission

`net` defaults to `[]` (deny all). Widen it to your Loki host — and only
your Loki host — via an override:

```yaml
buildin/audit-export-loki:
  overrides:
    params:
      endpoint: { value: "https://logs-prod-eu-west-0.grafana.net" }
      auth_user: { value: "123456" }
    permissions:
      net: ["logs-prod-eu-west-0.grafana.net"]
```

## Datadog (and other backends) — the seam

This task is deliberately Loki-specific; it is not a generic adapter. A
Datadog variant is a copy of `task.ts` that swaps two things and leaves the
cursor/kv/at-least-once skeleton untouched:

- **`buildStreams` → payload mapping.** Replace the Loki `streams` shape
  with Datadog's logs-intake array (`[{ ddsource, ddtags, message, ... }]`).
- **The push request.** `POST https://http-intake.logs.datadoghq.com/api/v2/logs`
  with the `DD-API-KEY` header instead of `/loki/api/v1/push` + basic/bearer.

Everything else — cursor read, `dicode.audit.query({ order: 'asc' })`,
advance-on-2xx — is identical. Keep each backend as its own builtin task
rather than a runtime-branching mega-task.
