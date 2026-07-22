# relay-cloudflared

Runs the Cloudflare Tunnel connector (`cloudflared`) that [`relay-edge`](../relay-edge/)
provisioned, as a supervised dicode **docker daemon task**. There is no
`task.ts` — cloudflared *is* the long-running process. It dials Cloudflare's
anycast edge and forwards the **proxied** public webhook/OAuth hostname into the
host's local dicode listener on `:5553`.

```
  webhook / OAuth        ┌── Cloudflare edge ──┐
  clients ───HTTPS─────▶ │ relay.dicode.io 🟠  │
                         └──────────┬──────────┘
                                    │ Cloudflare Tunnel
                          ┌─────────▼───────────┐
                          │  cloudflared        │  (this container)
                          └─────────┬───────────┘
                                    │ http://host.docker.internal:5553
                                    ▼
                          host :5553  ── dicode public HTTP listener
```

## Token handoff

cloudflared authenticates with a **connector token**, read from `TUNNEL_TOKEN`.
There are two ways to get it:

1. **From `relay-edge`.** When `relay-edge` creates the tunnel it stores the
   connector token in its kv under `tunnel_token` (Cloudflare returns it only
   once, at creation). Read it and set it as a dicode secret:

   ```
   dicode secrets set cloudflared_tunnel_token <token>
   ```

2. **From the Cloudflare dashboard.** Zero Trust → Networks → Tunnels → your
   tunnel → *Configure* → the connector install command embeds the same token.

This task never receives the token on the command line: `permissions.env`
resolves the `cloudflared_tunnel_token` secret into the container's
`TUNNEL_TOKEN` env var (docker secret-env injection, #629), so it stays out of
argv and the process listing.

## Networking — the critical caveat

The container reaches the host through `host.docker.internal`, mapped via
`extra_hosts: ["host.docker.internal:host-gateway"]`. This deliberately avoids
`network_mode: host` (which trips the container-security floor and needs an
operator opt-in).

**dicode's public listener (`:5553`) must bind a non-loopback interface
(`0.0.0.0`) for the container to reach it.** A container connecting to
`host.docker.internal` arrives at the host over the docker bridge, *not* over
loopback — a listener bound to `127.0.0.1` only will refuse the connection.

If you cannot (or prefer not to) bind `:5553` on `0.0.0.0`, the fallback is:

- set `docker.network_mode: host` on this task, drop `extra_hosts`, and run
  `relay-edge` with `ingress_host=localhost` (the default), **and**
- opt into host networking in `dicode.yaml`:

  ```yaml
  container_security:
    allow_host_network: true
  ```

  Without that opt-in the container-security floor rejects `network_mode: host`.

## relay-edge coupling

Because cloudflared reaches the host via `host.docker.internal`, the tunnel
ingress must target that name. Run `relay-edge`'s apply with:

```
dicode run ops/relay-edge \
  zone=dicode.io public_hostname=relay.dicode.io \
  control_hostname=broker.relay.dicode.io control_ip=203.0.113.7 \
  tunnel_name=relay-tunnel ingress_host=host.docker.internal dry_run=false
```

`ingress_host` defaults to `localhost` (for a host-run connector); this sidecar
needs `host.docker.internal`.

## Image pinning

`image` is pinned to a specific tag, never `:latest`, to keep the connector out
of a supply-chain surprise. **The pinned tag in `task.yaml` is a plausible
recent stable, not a verified newest release — bump it to a current
`cloudflared` version, ideally by digest:**

```yaml
docker:
  image: cloudflare/cloudflared@sha256:<digest-of-a-current-release>
```

Pull the digest for a release you trust and replace the tag.

## Hardening

- `cap_drop: ["ALL"]` — cloudflared needs no Linux capabilities.
- `security_opt: ["no-new-privileges:true"]`.
- `read_only: true` — token-based, remotely-managed config writes nothing to
  disk. If a future connector feature needs a writable path the container
  crash-loops (`restart: on-failure`) with a clear error; drop `read_only`
  then.
- `permissions.net: ["*"]` — cloudflared dials Cloudflare's anycast edge over
  dynamic hostnames, so a static allowlist isn't feasible. (Per-host net
  filtering isn't enforced for containers; an empty `net` would leave the
  container on network `none` with no connectivity.)

## Operator setup sequence

1. `dicode run ops/relay-edge ... tunnel_name=relay-tunnel ingress_host=host.docker.internal dry_run=false`
2. Read the connector token from relay-edge's kv `tunnel_token` (or the CF
   dashboard) and `dicode secrets set cloudflared_tunnel_token <token>`.
3. Ensure the dicode public listener (`:5553`) binds `0.0.0.0` (or use the
   `network_mode: host` fallback above).
4. This daemon task starts on daemon boot; confirm with `dicode status
   ops/relay-cloudflared` and `dicode logs <run-id>` (expect cloudflared's
   "Registered tunnel connection" lines).
