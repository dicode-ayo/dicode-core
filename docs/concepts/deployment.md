# Deployment

Dicode is a single binary. No dependencies, no runtime, no database server. Drop it anywhere and it runs.

---

## Running the daemon

```bash
# Download binary (macOS/Linux/Windows)
curl -sL https://dicode.app/install.sh | sh

# Run in the foreground with a specific config
dicode daemon --config /path/to/dicode.yaml --port 8080

# Or just run any CLI command — it auto-starts the daemon in the
# background (using ./dicode.yaml) if one isn't already running
dicode list
```

On first run (no `dicode.yaml` in the working directory), the onboarding wizard runs. It picks a surface automatically: a non-TTY session (systemd, Docker, CI) gets a silent default config, a TTY with no display (`$DISPLAY`/`$WAYLAND_DISPLAY` unset) gets the CLI wizard, and a TTY with a display prompts you to choose browser or CLI (default: browser). Set `DICODE_ONBOARDING=silent|cli|browser` to force a surface.

To run dicode under a process supervisor (systemd, launchd, a Docker restart policy, etc.), point it at `dicode daemon --config <path>` — there is no built-in service-install command.

---

## Docker

```bash
docker run -d \
  --name dicode \
  -p 8080:8080 \
  -v ~/.dicode:/data \
  -v ~/tasks:/tasks \
  -e DICODE_DATA_DIR=/data \
  -e ANTHROPIC_API_KEY=... \
  ghcr.io/dicode/dicode:latest
```

**Docker Compose:**
```yaml
services:
  dicode:
    image: ghcr.io/dicode/dicode:latest
    ports:
      - "8080:8080"
    volumes:
      - ./data:/data
      - ./tasks:/tasks
    environment:
      DICODE_DATA_DIR: /data
      ANTHROPIC_API_KEY: ${ANTHROPIC_API_KEY}
    restart: unless-stopped
```

**Health check:** `GET /healthz` returns `200 OK` when dicode is ready.

---

## Kubernetes (Helm)

A Helm chart ships in [`deploy/helm/dicode`](https://github.com/dicode-ayo/dicode-core/tree/main/deploy/helm/dicode). Requires Kubernetes ≥ 1.27 and Helm ≥ 3.8.

```bash
git clone https://github.com/dicode-ayo/dicode-core.git
cd dicode-core
helm install dicode ./deploy/helm/dicode \
  --create-namespace --namespace dicode \
  --set secret.create=true \
  --set secret.values.ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY
```

Then port-forward and open the dashboard:

```bash
kubectl -n dicode port-forward svc/dicode 8080:8080
open http://localhost:8080
```

Run the bundled connection test:

```bash
helm test dicode --namespace dicode
```

**Defaults:**
- Single replica, `Recreate` rollout strategy
- 5 GiB PVC at `/data` (SQLite database, master key, `dicode.yaml`)
- `runAsNonRoot` UID 65532, `readOnlyRootFilesystem`, drops all capabilities, `seccompProfile: RuntimeDefault`
- Liveness + readiness probes on the unauth `/healthz` route

**Notes:**
- The PVC is annotated `helm.sh/resource-policy: keep`, so `helm uninstall` does NOT delete your task data. Remove it manually with `kubectl delete pvc dicode-data` if you want a clean wipe.
- `replicaCount > 1` is **not** yet supported — the daemon has no leader election, so multiple replicas may run cron triggers more than once.
- Both `ghcr.io/dicode-ayo/dicode-core` and `dicodeayo/dicode-core` (Docker Hub) are published on every release-please tag; the chart pulls from GHCR by default.

See [`deploy/helm/dicode/README.md`](https://github.com/dicode-ayo/dicode-core/tree/main/deploy/helm/dicode/README.md) for every chart parameter, ingress / secret / config examples, and the publishing roadmap.

---

## Configuration reference

### `dicode.yaml`

```yaml
# Task sources — where tasks come from, as entries of the root TaskSet
spec:
  entries:
    main:
      ref:
        url: https://github.com/you/tasks
        branch: main
        poll_interval: 60s
        auth:
          token_env: GITHUB_TOKEN
    dev:                          # optional: for local dev
      ref:
        path: ~/tasks-dev

# Storage
database:
  type: sqlite                   # sqlite (default) | postgres | mysql
  # For postgres/mysql (paid):
  # url_env: DATABASE_URL

# WebSocket relay (for webhook URLs on laptops)
relay:
  enabled: true
  server_url: wss://relay.dicode.app

# Secrets
secrets:
  providers:
    - type: local                # encrypted SQLite
    - type: env                  # host env vars (fallback)

# Notifications: delivered by tasks via on_failure_chain.
# Point at buildin/alert (desktop), buildin/notifications, or any task
# you write yourself for ntfy / Slack / Discord / email / etc.
defaults:
  on_failure_chain: buildin/alert

# Server
server:
  port: 8080
  auth: true    # enable the global auth wall (passphrase login)
  mcp: true     # enable MCP server at /mcp

# AI generation — task id invoked for AI chat/edit; defaults to
# buildin/dicodai, a buildin/ai-agent preset. Provider/model/api key are
# params on that task's own config, not top-level dicode.yaml keys.
ai:
  task: buildin/dicodai

# Logging
log_level: info   # debug | info | warn | error
```

### Environment variables

| Variable | Description |
|---|---|
| `DICODE_DATA_DIR` | Directory for DB and master key (default: `~/.dicode`) |
| `DICODE_MASTER_KEY` | Master encryption key (overrides `~/.dicode/master.key`) |
| `DICODE_ONBOARDING` | Force the first-run wizard surface: `silent`, `cli`, or `browser` |

---

## CLI reference

```
dicode daemon [--config <path>] [--port N]     Run the daemon in the foreground

dicode version                                 Print version
dicode list                                     List all registered tasks
dicode run <task-id> [key=value ...]           Trigger a task run and wait for the result
dicode logs <run-id>                           Fetch log lines for a run
dicode status [task-id]                        Daemon health or latest run for a task
dicode resume [run-id]                         List suspended runs, or resume one

dicode task test <task-id>                     Run the task's sibling task.test.* through its runtime
dicode task create <name> [--ai]               Scaffold a task; with --ai, open an edit session
dicode task edit <task-id> <prompt>             Open or resume an AI edit session
dicode task save <session-id>                  Apply a session's changes
dicode task cancel <session-id>                Discard a session
dicode task delete <task-id>                   Delete a task
dicode task approve <task-id>                  Approve a changed task (see the approval gate)
dicode task pending                            List tasks awaiting approval

dicode secrets list                            List secret keys
dicode secrets set <key> <value>               Store a secret
dicode secrets delete <key>                    Delete a secret

dicode relay trust-broker                      Trust the relay's OAuth broker

dicode ai <prompt> [flags]                     Run the configured AI task with a prompt

dicode auth reset-passphrase                   Reset the server auth passphrase

dicode mcp install|uninstall|print-config      Manage the MCP client integration
```
