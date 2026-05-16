# Worked example: hardened Cloudflare Tunnel daemon

This example wires up a `cloudflared` Docker daemon that exposes a single
hostname to the public internet through Cloudflare's edge — and does it
without any operator-facing config file containing the tunnel's
credentials in cleartext.

The goal is to demonstrate the full stack of task-format primitives that
landed across PRs [#296], [#297], [#298], [#299], [#300], [#302], and
[#303]:

- A Docker daemon task with the modern hardening fields (`network_mode`,
  `extra_hosts`, `cap_drop`, `security_opt`, `read_only`, `user`).
- `${DATADIR}` expansion in `docker.volumes`, so the daemon mounts its
  config from a path under the daemon data dir without hard-coding
  `/home/<user>/...`.
- A `trigger.before:` preflight list that runs a renderer plus a
  cert-stager **before** the daemon container starts, restarting the
  daemon any time either prereq re-runs.
- Per-edge `overrides:` on those preflight entries — the daemon pins the
  template body and the Doppler-fed env block per-edge, leaving the
  prereq task's own (manual) fires alone.
- The `buildin/template` library task with `run_result.enabled: false`,
  so the rendered config (which embeds the tunnel UUID and the hostname)
  never touches `runs.return_value` on disk.

The end state: one `dicode tasks run cloudflare-tunnel` to verify what
will be rendered, then `dicode daemon` brings the tunnel up; rotating
the Doppler secrets re-triggers the renderer, which restarts the
container with new config.

---

## Prerequisites

- Docker installed; the `docker` runtime healthy in **Config →
  Runtimes**.
- A Cloudflare account with a registered domain.
- The `cloudflared` CLI installed locally for the one-time login + tunnel
  create step.
- A Doppler project with a token allowlisted for the relevant secrets
  (the daemon will read them at preflight time, not at config-load
  time).

---

## One-time operator setup

The tunnel's long-lived credentials are produced by `cloudflared`
itself. We mint them once, stash the files on disk where the container
can mount them, and put the variable bits in Doppler.

```bash
# Pop a browser to authorize cloudflared against your Cloudflare account.
# Writes cert.pem to ~/.cloudflared/cert.pem
cloudflared tunnel login

# Create the tunnel. Prints the UUID and writes credentials.json next to
# cert.pem.
cloudflared tunnel create my-tunnel
# → Tunnel credentials written to /home/<you>/.cloudflared/<UUID>.json
# → Created tunnel my-tunnel with id <UUID>

# Point your hostname at the tunnel. (Or use the dashboard — same result.)
cloudflared tunnel route dns my-tunnel api.example.com
```

Move the two credentials files under the dicode data dir so the daemon
can mount them with the same path inside the container regardless of
where the daemon is installed:

```bash
mkdir -p "${HOME}/.dicode/cloudflared"
cp ~/.cloudflared/cert.pem            "${HOME}/.dicode/cloudflared/cert.pem"
cp ~/.cloudflared/<UUID>.json         "${HOME}/.dicode/cloudflared/credentials.json"
chmod 600 "${HOME}/.dicode/cloudflared/"*
```

Stash the variable bits in Doppler. Keep the tunnel UUID and the
hostname here so a re-deploy or a hostname change doesn't require
editing task.yaml:

```bash
doppler secrets set CF_TUNNEL_ID="<UUID>"
doppler secrets set CF_TUNNEL_HOSTNAME="api.example.com"
doppler secrets set CF_TUNNEL_SERVICE="http://host.docker.internal:8080"
```

If you'd rather hold `cert.pem` / `credentials.json` in Doppler instead
of on disk, base64-encode them so they survive Doppler's string-typed
fields:

```bash
base64 -w0 ~/.dicode/cloudflared/cert.pem        | doppler secrets set CF_CERT_B64
base64 -w0 ~/.dicode/cloudflared/credentials.json | doppler secrets set CF_CREDS_B64
```

A second preflight task can then `base64 -d` them into place at boot.
We'll show both shapes below; pick one.

> The Doppler provider task (`buildin/secret-providers/doppler`, see
> [Secrets — built-in providers](../concepts/secrets.md#built-in-providers))
> needs `DOPPLER_TOKEN` in the encrypted secrets store first. One-time
> bootstrap:
>
> ```bash
> dicode secrets set DOPPLER_TOKEN dp.st.prd...
> ```

---

## The task.yaml

Everything below lives in one folder, say `tasks/cloudflare-tunnel/`.
The folder contains:

- `task.yaml`               — the daemon, declares its preflight chain
- `render-config/task.yaml` — the renderer (chains `buildin/template`)
- `stage-creds/task.yaml`   — the cert/credentials stager (optional;
  only needed if you chose the Doppler-encoded-file path)

The daemon's `task.yaml`:

```yaml
apiVersion: dicode/v1
kind: Task
name: Cloudflare Tunnel
description: |
  Public ingress for api.example.com via a hardened cloudflared
  container. Config is rendered into ${DATADIR}/cloudflared/config.yml
  by the render-config preflight before each (re)start; credentials are
  mounted read-only from the same directory.
runtime: docker

trigger:
  daemon: true
  restart: always
  before:
    # Render the tunnel config from Doppler secrets. The template body
    # is pinned per-edge so the prereq task itself stays a generic
    # library helper.
    - task: render-config
      overrides:
        timeout: 30s
        env:
          - name: CF_TUNNEL_ID
            from: task:doppler
          - name: CF_TUNNEL_HOSTNAME
            from: task:doppler
          - name: CF_TUNNEL_SERVICE
            from: task:doppler

    # Optional second prereq: stages cert.pem / credentials.json into
    # ${DATADIR}/cloudflared/ from base64-encoded Doppler entries. Omit
    # this entry entirely if you copied the files manually in the
    # one-time setup.
    - task: stage-creds
      overrides:
        timeout: 30s
        env:
          - name: CF_CERT_B64
            from: task:doppler
          - name: CF_CREDS_B64
            from: task:doppler

docker:
  image: cloudflare/cloudflared:latest
  command:
    - tunnel
    - --no-autoupdate
    - --config
    - /etc/cloudflared/config.yml
    - run

  # Hardening — none of these defaults are implicit; everything is opt-in.
  network_mode: bridge
  extra_hosts:
    - "host.docker.internal:host-gateway"
  cap_drop: [ALL]
  security_opt:
    - "no-new-privileges:true"
  read_only: true
  user: "65532:65532"

  # ${DATADIR} expands to the daemon data dir (typically ~/.dicode) at
  # spec-load time. The container sees a stable /etc/cloudflared
  # regardless of where dicode is installed on the host.
  volumes:
    - "${DATADIR}/cloudflared:/etc/cloudflared:ro"
```

The renderer (`render-config/task.yaml`) is a thin chain over the
`buildin/template` library task. It owns the template body and the
output filename; it consumes the Doppler-resolved env injected by the
daemon's per-edge override.

```yaml
apiVersion: dicode/v1
kind: Task
name: Render cloudflared config
description: |
  Render /etc/cloudflared/config.yml for the my-tunnel daemon.
  Substitutes ${CF_TUNNEL_ID} / ${CF_TUNNEL_HOSTNAME} /
  ${CF_TUNNEL_SERVICE} via buildin/template, then writes the result to
  ${DATADIR}/cloudflared/config.yml.
runtime: deno

trigger:
  manual: true

permissions:
  env:
    # Declared so the daemon's per-edge env override (which is a
    # full-replace, not an append) has named slots to land in. The
    # values supplied here are placeholders only — the daemon's
    # before-edge replaces them with `from: task:doppler` at dispatch.
    - name: CF_TUNNEL_ID
      value: ""
    - name: CF_TUNNEL_HOSTNAME
      value: ""
    - name: CF_TUNNEL_SERVICE
      value: ""
  fs:
    - path: "${DATADIR}/cloudflared"
      permission: rw
  dicode:
    tasks:
      - buildin/template

# The rendered string embeds the hostname and service URL. It's not
# secret-material per se but we never want it pinned in the runs table
# alongside the tunnel UUID.
run_result:
  enabled: false
```

`render-config/task.ts`:

```typescript
const TEMPLATE = `tunnel: \${CF_TUNNEL_ID}
credentials-file: /etc/cloudflared/credentials.json

ingress:
  - hostname: \${CF_TUNNEL_HOSTNAME}
    service: \${CF_TUNNEL_SERVICE}
  - service: http_status:404
`;

export default async function main({ env, log }: DicodeSdk) {
  // Run the buildin/template library task — it reads ${VAR} placeholders
  // from its own env (declared via permissions.env) and returns the
  // rendered string via WaitRun's in-memory delivery. Because
  // buildin/template sets run_result.enabled: false, the rendered output
  // never lands in runs.return_value.
  const rendered = await dicode.run_task("buildin/template", {
    template: TEMPLATE,
  });

  const dataDir = await env.get("DATADIR");
  const target = `${dataDir}/cloudflared/config.yml`;
  await Deno.writeTextFile(target, rendered, { mode: 0o600 });
  log.info(`wrote ${target}`);
  return { path: target };
}
```

A subtle detail: `dicode.run_task("buildin/template", ...)` passes the
template body as a param, but `buildin/template` resolves the
`${CF_TUNNEL_ID}` placeholders against **its own** declared env. The
daemon's per-edge override goes onto the renderer (this task) — the
renderer's env then needs to flow into `buildin/template`. Easiest way:
declare the same names on `buildin/template`'s per-edge override at the
`run_task` site. Today the simplest pattern is to declare them on the
renderer and let `buildin/template` inherit them via process env (the
Deno subprocess inherits the renderer's resolved env). If you want
strict isolation, override `buildin/template` globally with a matching
`permissions.env` block in your taskset's `entries.buildin/template`
section.

If you went the Doppler-encoded-file route in setup, here's the
stager (`stage-creds/task.yaml`):

```yaml
apiVersion: dicode/v1
kind: Task
name: Stage cloudflared credentials
description: |
  Decode CF_CERT_B64 / CF_CREDS_B64 into ${DATADIR}/cloudflared/. Idempotent;
  re-runs simply overwrite the on-disk copy.
runtime: deno

trigger:
  manual: true

permissions:
  env:
    - name: CF_CERT_B64
      value: ""
    - name: CF_CREDS_B64
      value: ""
  fs:
    - path: "${DATADIR}/cloudflared"
      permission: rw

run_result:
  enabled: false
silent: true
```

```typescript
// stage-creds/task.ts
export default async function main({ env, log }: DicodeSdk) {
  const dataDir = await env.get("DATADIR");
  const dir = `${dataDir}/cloudflared`;
  await Deno.mkdir(dir, { recursive: true });

  const decode = (b64: string) => Uint8Array.from(atob(b64), c => c.charCodeAt(0));

  await Deno.writeFile(`${dir}/cert.pem`,
    decode(await env.get("CF_CERT_B64")), { mode: 0o600 });
  await Deno.writeFile(`${dir}/credentials.json`,
    decode(await env.get("CF_CREDS_B64")), { mode: 0o600 });

  log.info(`staged credentials into ${dir}`);
}
```

---

## Why this works

The chain of events on `dicode daemon` startup:

1. The trigger engine registers `cloudflare-tunnel` as a daemon. Because
   its `trigger.before` is non-empty, the engine does not start the
   container yet — it queues a preflight pass.
2. Both prereqs fire in parallel (one goroutine each, awaited via
   `WaitRun`). The per-edge `overrides:` on each before-entry are merged
   onto a deep copy of the prereq's spec **at dispatch time only**; the
   prereq's standalone manual fires continue to use the spec on disk.
3. For each prereq, the engine resolves `from: task:doppler` by
   spawning `buildin/secret-providers/doppler` once and caching the
   result for the duration of the dispatch (see [Secrets — providers](../concepts/secrets.md)).
4. `render-config` invokes `buildin/template`. The rendered string
   flows back via the in-memory `WaitRun` channel — `buildin/template`
   sets `run_result.enabled: false`, so no copy lands in the database.
5. `render-config` writes the result to
   `${DATADIR}/cloudflared/config.yml`. It also returns a small
   `{ path }` summary, which **is** persisted (the renderer task does
   not opt out of return-value persistence) and shows up in the run
   detail page so an operator can see the preflight succeeded.
6. Once both prereqs return `status=success`, the engine starts the
   `cloudflared` container with the hardening flags applied, the
   credentials directory mounted read-only, and the rendered
   `config.yml` already in place.

If either prereq fails, the daemon is left in the `PrereqFailed`
state and the failure shows up in the run history with the prereq's
own error message. The engine does **not** start a half-configured
container.

---

## Secret rotation

Re-running either preflight task by hand triggers the same restart
path that the engine uses on first boot:

```bash
dicode tasks run render-config
```

When the engine sees `render-config` finish with `status=success`, it
walks the registered daemons and queues a restart for every daemon
that lists `render-config` in its `trigger.before`. Restarts are
coalesced (at most one in flight per daemon) so a flurry of rotations
produces one re-render and one restart, not a thrash loop. The same
applies to `stage-creds` if you went the encoded-files route.

This makes the rotation story trivial:

```bash
# rotate the Cloudflare service URL
doppler secrets set CF_TUNNEL_SERVICE="http://host.docker.internal:8081"

# re-fire the renderer; the daemon notices and restarts on the new file
dicode tasks run render-config
```

For unattended rotation, add a cron prereq somewhere upstream of
`render-config` and chain it: see [Chain triggers](../concepts/task-chaining.md).

---

## Verification

A dry run of the renderer prints the would-be `config.yml` content
(the renderer task itself returns the summary, not the rendered
string — the rendered string is captured by `buildin/template`'s
in-memory delivery and written to disk; to *inspect* it, run
`buildin/template` directly):

```bash
# Inspect what the template will render. Beware: prints rendered
# content to stdout — lands in shell history. Redirect for anything
# secret-bearing.
dicode tasks run buildin/template \
  --param template="$(cat tasks/cloudflare-tunnel/render-config/template.yml)" \
  > /tmp/rendered.yml
cat /tmp/rendered.yml
```

Once the daemon is up, confirm the container picked up the rendered
config:

```bash
docker ps | grep cloudflared
docker exec <container-id> cat /etc/cloudflared/config.yml
```

You should see the same `tunnel:` / `ingress:` block with the Doppler
values substituted.

The dicode WebUI's run detail page for the daemon shows the preflight
chain as child runs of the daemon's main run, so a failed preflight
surfaces in the same place as any other task failure.

---

## What each PR contributed

| PR | What you'd lose without it |
| --- | --- |
| [#296] | The hardening fields (`network_mode`, `cap_drop`, `security_opt`, `read_only`, `user`, `extra_hosts`) on the daemon's `docker:` block. |
| [#297] | `${DATADIR}` and `${TASK_DIR}` expansion in `docker.volumes`. Without it you'd hard-code an absolute host path. |
| [#298] | The `buildin/template` library task. Without it the renderer would have to reimplement `${VAR}` substitution and the no-persist contract every time. |
| [#299] | `trigger.chain.params` carrying operator-defined keys downstream alongside `input.output`. Not used directly in the daemon above (the daemon uses `before:`, not `chain:`) but the same merging semantics power the per-edge `overrides.params` field below. |
| [#300] | `trigger.before:` itself — daemon preflight via task-id list, with the restart-on-prereq-rerun semantics. |
| [#302] | `run_result.enabled: false` on `buildin/template` and on the renderer. Without it the rendered tunnel config (with the UUID baked in) would persist to `runs.return_value`. |
| [#303] | Per-edge `overrides:` on each `before[]` entry and on `chain:` edges. Without it the renderer and the stager would have to be either daemon-specific (one renderer per daemon) or pre-overridden via the global `dicode tasks override` path. |

---

## Related docs

- [Task Format → `trigger.before`](../concepts/task-format.md#daemon-preflight-via-triggerbefore)
- [Task Format → `trigger.chain.params` & `chain.overrides`](../concepts/task-format.md#chain-params-and-per-edge-overrides)
- [Task Format → `run_result.enabled`](../concepts/task-format.md#suppressing-return-value-persistence)
- [Task Format → Docker hardening fields](../concepts/task-format.md#container-fields-reference)
- [Task Chaining](../concepts/task-chaining.md)
- [Secrets — providers](../concepts/secrets.md)

[#296]: https://github.com/dicode-ayo/dicode-core/pull/296
[#297]: https://github.com/dicode-ayo/dicode-core/pull/297
[#298]: https://github.com/dicode-ayo/dicode-core/pull/298
[#299]: https://github.com/dicode-ayo/dicode-core/pull/299
[#300]: https://github.com/dicode-ayo/dicode-core/pull/300
[#302]: https://github.com/dicode-ayo/dicode-core/pull/302
[#303]: https://github.com/dicode-ayo/dicode-core/pull/303
