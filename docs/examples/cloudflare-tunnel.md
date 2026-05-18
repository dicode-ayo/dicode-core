# Worked example: hardened Cloudflare Tunnel daemon

This example wires up a `cloudflared` Docker daemon that exposes a single
hostname to the public internet through Cloudflare's edge — and does it
without any operator-facing config file containing the tunnel's
credentials in cleartext.

The goal is to demonstrate the full stack of task-format primitives that
landed across PRs [#296], [#297], [#298], [#299], [#300], [#302], [#303],
[#309], [#310], and [#311]:

- A Docker daemon task with the modern hardening fields (`network_mode`,
  `extra_hosts`, `cap_drop`, `security_opt`, `read_only`, `user`).
- `${DATADIR}` expansion in `docker.volumes`, so the daemon mounts its
  config from a path under the daemon data dir without hard-coding
  `/home/<user>/...`.
- A `trigger.before:` **sequential output-piping pipeline** that renders
  the daemon's config and persists it to disk before the container
  starts — and re-fires every preflight on every restart so rotated
  secrets land in the rendered file.
- Per-edge `overrides:` on those preflight entries — the daemon pins the
  template body, the output path, and the Doppler-fed env block
  per-edge, leaving the prereq task's own (manual) fires alone.
- The `buildin/template` library task with `run_result.enabled: false`,
  so the rendered config (which embeds the tunnel UUID and the hostname)
  never touches `runs.return_value` on disk.
- The `buildin/write-local` library task receiving the rendered string
  via `${input.output}` interpolation — no wrapper task required.

The end state: `dicode daemon` brings the tunnel up after re-rendering
its config from the latest Doppler secrets; restarting the daemon (or
re-firing any pipeline stage by hand) re-runs the pipeline and picks
up rotated values without editing the task.yaml.

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

- `task.yaml`             — the daemon, declares its preflight pipeline
- `stage-creds/task.yaml` — the cert/credentials stager (optional;
  only needed if you chose the Doppler-encoded-file path)

The renderer + writer pair are declared **inline** in the daemon's
`trigger.before:` pipeline via `buildin/template` + `buildin/write-local`
— no wrapper task required.

The daemon's `task.yaml`:

```yaml
apiVersion: dicode/v1
kind: Task
name: Cloudflare Tunnel
description: |
  Public ingress for api.example.com via a hardened cloudflared
  container. Config is rendered into ${DATADIR}/cloudflared/config.yml
  by an inline buildin/template + buildin/write-local pipeline before
  each (re)start; credentials are mounted read-only from the same
  directory.
runtime: docker

trigger:
  daemon: true
  restart: always
  before:
    # Stage 1: render the tunnel config string from Doppler-fed env.
    # The template body is pinned per-edge so the buildin/template task
    # itself stays a generic library helper. Returns the rendered
    # string via in-memory delivery; nothing lands in runs.return_value
    # (buildin/template sets run_result.enabled: false).
    - task: buildin/template
      overrides:
        timeout: 30s
        params:
          template: |
            tunnel: ${CF_TUNNEL_ID}
            credentials-file: /etc/cloudflared/credentials.json
            ingress:
              - hostname: ${CF_TUNNEL_HOSTNAME}
                service: ${CF_TUNNEL_SERVICE}
              - service: http_status:404
        env:
          - name: CF_TUNNEL_ID
            from: task:doppler
          - name: CF_TUNNEL_HOSTNAME
            from: task:doppler
          - name: CF_TUNNEL_SERVICE
            from: task:doppler

    # Stage 2: persist stage 1's rendered string to disk. The
    # ${input.output} token resolves to the upstream stage's return
    # value at dispatch time. fs:rw is declared inline per-edge so the
    # write-local task itself ships with no implicit fs roots.
    - task: buildin/write-local
      overrides:
        timeout: 30s
        params:
          content: "${input.output}"
          path: "${DATADIR}/cloudflared/config.yml"
          mode: "0644"
        fs:
          - path: "${DATADIR}/cloudflared"
            permission: rw

    # Stage 3 (optional): stages cert.pem / credentials.json into
    # ${DATADIR}/cloudflared/ from base64-encoded Doppler entries. Omit
    # this entry entirely if you copied the files manually in the
    # one-time setup. Stays as a separate task because it produces two
    # output files (not one rendered string), so the template+write-local
    # pair doesn't apply.
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

The renderer + writer pair are declared inline in the daemon's
`before:` pipeline above — no wrapper task is required. Stage 1
(`buildin/template`) renders the config string from Doppler-fed env.
Stage 2 (`buildin/write-local`) receives the rendered string via
`${input.output}` interpolation and persists it to
`${DATADIR}/cloudflared/config.yml` at mode `0644`.

`trigger.before:` runs entries **sequentially in declaration order** —
each stage's return value is piped to the next via `${input.output}`,
and any failure short-circuits the rest of the pipeline and leaves the
daemon in `prereq_failed`. The cloudflared example's three stages
(`buildin/template` → `buildin/write-local` → `stage-creds`) run in
order; if any fails, the daemon doesn't start. The engine re-fires
every stage on every preflight attempt, so rotated Doppler secrets land
in the rendered file on the next restart with no manual intervention.

Both `buildin/template` and `buildin/write-local` set
`run_inputs.enabled: false` and `run_result.enabled: false`, so the
template body, the rendered config string, and the resolved path never
land in the persisted runs table. The values still flow in-memory
between stages and to the daemon-restart hook.

If you went the Doppler-encoded-file route in setup, here's the
stager (`stage-creds/task.yaml`). It stays as a separate task because
it produces **two** output files (cert.pem + credentials.json) from
two separate base64 inputs — the template + write-local pair only
handles a single rendered string.

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
2. Stage 1 (`buildin/template`) fires. The per-edge `overrides:` are
   merged onto a deep copy of the prereq's spec **at dispatch time
   only**; the library task's spec on disk is unaffected. The engine
   resolves `from: task:doppler` by spawning the Doppler secret
   provider once and caching the result for the rest of the pipeline
   (see [Secrets — providers](../concepts/secrets.md)). The task reads
   the pinned template body from `params.template`, substitutes the
   Doppler-resolved env, and returns the rendered string via in-memory
   delivery. Nothing lands in `runs.return_value` (the task sets
   `run_result.enabled: false`).
3. Stage 2 (`buildin/write-local`) fires. The literal token
   `${input.output}` in `overrides.params.content` is replaced with
   stage 1's return value at dispatch time. The task writes the
   rendered string to `${DATADIR}/cloudflared/config.yml` at mode
   `0644` (the per-edge `fs:rw` override scopes the write to that
   directory) and returns the resolved path.
4. Stage 3 (`stage-creds`, optional) fires. Decodes the base64-encoded
   cert + credentials from Doppler into the same directory at mode
   `0600`.
5. Once all stages return `status=success`, the engine starts the
   `cloudflared` container with the hardening flags applied, the
   credentials directory mounted read-only, and the rendered
   `config.yml` already in place.

If any stage fails, the pipeline short-circuits — later stages don't
run, the daemon is left in `prereq_failed`, and the failure shows up
in the run history with that stage's own error message. The engine
does **not** start a half-configured container.

---

## Secret rotation

Rotating a Doppler secret followed by a manual restart of the daemon
re-fires the entire `before:` pipeline against the fresh secret values
(the engine re-fires every preflight on every restart — there's no
"already-satisfied" short-circuit). For unattended rotation, re-run any
intermediate stage of the pipeline by hand — the engine re-fires
downstream stages and restarts every daemon that lists the touched
task in its `before:`:

```bash
# rotate the Cloudflare service URL
doppler secrets set CF_TUNNEL_SERVICE="http://host.docker.internal:8081"

# re-fire stage 1 by hand; the engine re-pipes its return value
# through stage 2 (buildin/write-local), then restarts the daemon to
# pick up the rewritten config.yml. Easier: just restart the daemon
# (`dicode daemon restart cloudflare-tunnel`) — the preflight pipeline
# re-fires from stage 1 with no manual `buildin/template` invocation.
dicode daemon restart cloudflare-tunnel
```

Restarts are coalesced (at most one in flight per daemon) so a flurry
of rotations produces one re-render and one restart, not a thrash
loop. The same propagation rule applies to `stage-creds` if you went
the encoded-files route — re-running it triggers a daemon restart
without touching the rendered config.

For unattended rotation, add a cron prereq somewhere upstream of the
pipeline and chain it: see [Chain triggers](../concepts/task-chaining.md).

---

## Verification

To inspect what the template will render before the daemon starts,
invoke `buildin/template` directly with the same template body and the
needed env. Because the template body is inlined into the daemon's
`task.yaml`, copy-paste it into the `--param template=` argument (or
keep a sidecar `template.yml` in your taskset for the same purpose):

```bash
# Inspect what the template will render. Beware: prints rendered
# content to stdout — lands in shell history. Redirect for anything
# secret-bearing.
dicode tasks run buildin/template \
  --param template="$(cat <<'EOF'
tunnel: ${CF_TUNNEL_ID}
credentials-file: /etc/cloudflared/credentials.json
ingress:
  - hostname: ${CF_TUNNEL_HOSTNAME}
    service: ${CF_TUNNEL_SERVICE}
  - service: http_status:404
EOF
)" \
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
| [#298] | The `buildin/template` library task. Without it stage 1 would have to be a hand-rolled task that reimplements `${VAR}` substitution and the no-persist contract every time. |
| [#299] | `trigger.chain.params` carrying operator-defined keys downstream alongside `input.output`. Not used directly in the daemon above (the daemon uses `before:`, not `chain:`) but the same merging semantics power the per-edge `overrides.params` field. |
| [#300] | `trigger.before:` itself — daemon preflight via task-id list, with the restart-on-prereq-rerun semantics. |
| [#302] | `run_result.enabled: false` on `buildin/template` and `buildin/write-local`. Without it the rendered tunnel config (with the UUID baked in) and the template body would persist to `runs.return_value`. |
| [#303] | Per-edge `overrides:` on each `before[]` entry and on `chain:` edges. Without it `buildin/template` and `buildin/write-local` would have to be either daemon-specific (one copy per daemon) or pre-overridden via the global `dicode tasks override` path. |
| [#309] | The `buildin/write-local` library task. Without it stage 2 would have to be a hand-rolled Deno task that calls `Deno.writeTextFile`. |
| [#310] | `${input.output}` interpolation in `before[i].overrides.params`. Without it stage 2 couldn't reach stage 1's rendered string declaratively — you'd need a wrapper task that invokes both via `dicode.run_task`. |
| [#311] | Sequential semantics on `trigger.before:`. Without it stages would run in parallel and `${input.output}` propagation wouldn't exist; the wrapper task would still be required. |

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
[#309]: https://github.com/dicode-ayo/dicode-core/pull/309
[#310]: https://github.com/dicode-ayo/dicode-core/pull/310
[#311]: https://github.com/dicode-ayo/dicode-core/pull/311
