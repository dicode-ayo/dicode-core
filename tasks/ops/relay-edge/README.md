# relay-edge

Reconciles the Cloudflare edge for a **free, self-hosted "bootstrap" relay**:
one proxied hostname for the public HTTP listener (fronted by a Cloudflare
Tunnel) and one grey (unproxied) A record for the mTLS control channel. It
enforces the control-channel-never-proxied invariant in code.

## Topology

```
                        ┌───────────────────────────────────────────┐
                        │                Cloudflare                 │
  webhook / OAuth       │                                           │
  clients ───HTTPS────▶ │  relay.dicode.io   (CNAME, PROXIED 🟠)    │
                        │        │  cfargotunnel.com                 │
                        └────────┼──────────────────────────────────┘
                                 │  Cloudflare Tunnel (cloudflared)
                                 ▼
                        host :5553  ── public HTTP webhook/OAuth listener


  dicode daemons ──mTLS──▶ broker.relay.dicode.io  (A, GREY / UNPROXIED ⚪)
                                 │  straight to the host IP, TLS not terminated
                                 ▼
                        host :5554  ── mTLS control channel
```

- **Public listener** (HTTP, local `:5553`): exposed through a Cloudflare Tunnel
  to a **proxied** (orange) hostname. Free, no inbound port, free TLS.
- **Control channel** (mTLS, `:5554`): exposed via a **grey (unproxied)** DNS A
  record straight to the host IP.

## The 4401 invariant

The control-channel record must **never** be proxied or tunneled. The daemon's
identity *is* its TLS client certificate (`getPeerX509Certificate`). Any
Cloudflare proxy or tunnel terminates TLS at the edge and strips the client
cert, so the broker sees an unauthenticated connection and closes every daemon
with **close 4401**.

`proxied` is therefore derived from each record's role, never accepted as input,
and `assertInvariant()` runs before any mutation: a `control` record is asserted
`proxied === false` and a `public` record `proxied === true`. A control record
that is ever proxied throws and no API call is made.

## What this task does — and does NOT do

- **Provisions** the CF Tunnel (when `tunnel_name` is set) and the DNS records.
- On tunnel creation it stores the connector token in kv under `tunnel_token`
  (Cloudflare returns it only once, at creation).
- It does **not** run `cloudflared`. The operator — or the
  [`ops/relay-cloudflared`](../relay-cloudflared/) sidecar task — reads
  `tunnel_token` from kv and runs the connector with it. When that sidecar runs
  the connector, set `ingress_host=host.docker.internal` here so the ingress
  targets the host from inside the container.

## Required Cloudflare API token scopes

- **Zone → DNS: Edit**
- **Account → Cloudflare Tunnel: Edit**

## Credentials

Store the token as a dicode secret, and provide the account id via env:

```
dicode secrets set cloudflare_api_token <token>
export CLOUDFLARE_ACCOUNT_ID=<account-id>
```

## Params

| Param | Type | Default | Description |
|---|---|---|---|
| `zone` | string | — (required) | Zone the records live in, e.g. `dicode.io`. |
| `public_hostname` | string | — (required) | Proxied hostname for the public listener, e.g. `relay.dicode.io`. |
| `control_hostname` | string | — (required) | Grey hostname for the mTLS channel, e.g. `broker.relay.dicode.io`. |
| `control_ip` | string | — (required) | Public IP of the host serving `:5554`. |
| `tunnel_name` | string | `""` | Named CF Tunnel to manage. Empty → the public record is left unmanaged. |
| `local_port` | number | `5553` | Local port the tunnel forwards the public hostname to. |
| `ingress_host` | string | `localhost` | Host cloudflared connects to for the local listener. Set `host.docker.internal` when running the [`ops/relay-cloudflared`](../relay-cloudflared/) containerized sidecar; keep `localhost` for a host-run connector. |
| `dry_run` | bool | `true` | Plan only, zero mutations. The cron drift check uses this default. |

## Usage

**Cron drift check** (hourly, `dry_run` default `true`): computes a plan, makes
zero mutating calls, and returns `{ dry_run: true, drift, changes, summary }`.
Chain a drift run into `buildin/notify` on the `drift` boolean and `summary`
string to get alerted when the edge has drifted from desired state.

**Manual apply**: trigger with `dry_run=false` to execute the plan. Returns
`{ dry_run: false, applied, tunnel_token_stored, summary }`.

```
dicode run ops/relay-edge \
  zone=dicode.io public_hostname=relay.dicode.io \
  control_hostname=broker.relay.dicode.io control_ip=203.0.113.7 \
  tunnel_name=relay-tunnel dry_run=false
```

Reconciliation never deletes unmanaged records: each desired record is a
create-if-absent / patch-if-different, nothing else.
