# Deployment

Dicode is a single binary. No dependencies, no runtime, no database server. Drop it anywhere and it runs.

---

## Desktop

The default mode for developer machines. Includes system tray icon and OS desktop notifications.

```bash
# Download binary (macOS/Linux/Windows)
curl -sL https://dicode.app/install.sh | sh

# Start dicode
dicode

# Start with tray icon
dicode --tray

# Start with specific config
dicode --config /path/to/dicode.yaml
```

On first run (no `dicode.yaml` in the current directory), the onboarding wizard opens in your browser.

---

## Headless (server / VPS)

For machines without a display. No tray icon, no desktop notifications.

```bash
dicode --headless
# or
DICODE_HEADLESS=true dicode
```

The `--headless` flag is automatically applied when no display is detected (`$DISPLAY` is unset on Linux).

---

## Run on startup

### Desktop (macOS)

```bash
dicode service install
```

Creates a LaunchAgent at `~/Library/LaunchAgents/app.dicode.plist`. Dicode starts on login and restarts automatically if it crashes.

### Desktop (Linux — XDG autostart)

```bash
dicode service install
```

Creates `~/.config/autostart/dicode.desktop`. Starts with the desktop session.

### Desktop (Windows)

```bash
dicode service install
```

Adds a registry entry under `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`.

### Server (systemd)

```bash
sudo dicode service install --headless
```

Creates `/etc/systemd/system/dicode.service` and enables it. Dicode starts on boot and restarts on failure.

```bash
# After install
sudo systemctl status dicode
sudo journalctl -u dicode -f
```

### Windows Service

```bash
dicode service install --headless
```

Registers dicode as a Windows Service.

### Other `service` commands

```bash
dicode service start
dicode service stop
dicode service restart
dicode service status
dicode service logs
dicode service uninstall
```

---

## Docker

```bash
docker run -d \
  --name dicode \
  -p 8080:8080 \
  -v ~/.dicode:/data \
  -v ~/tasks:/tasks \
  -e DICODE_HEADLESS=true \
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
      DICODE_HEADLESS: "true"
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
# Task sources — where tasks come from
sources:
  - type: git
    id: main                     # unique source ID
    url: https://github.com/you/tasks
    branch: main
    poll_interval: 60s
    auth:
      type: token
      token_env: GITHUB_TOKEN
  - type: local                  # optional: for local dev
    id: dev
    path: ~/tasks-dev
    watch: true

# Storage
database:
  type: sqlite                   # sqlite (default) | postgres | mysql
  # For postgres/mysql (paid):
  # url_env: DATABASE_URL

# WebSocket relay (for webhook URLs on laptops)
relay:
  enabled: true
  token_env: DICODE_RELAY_TOKEN  # from dicode.app account

# Secrets
secrets:
  providers:
    - type: local                # encrypted SQLite
    - type: env                  # host env vars (fallback)

# Notifications
notifications:
  on_failure: true
  on_success: false
  provider:
    type: ntfy
    url: https://ntfy.sh
    topic: my-dicode-alerts
    token_env: NTFY_TOKEN

# Server
server:
  port: 8080
  api_secret_env: DICODE_API_SECRET  # optional: protect REST API
  mcp: true                          # enable MCP server at /mcp
  tray: false                        # enable system tray icon

# AI generation
ai:
  provider: anthropic
  model: claude-sonnet-4-6
  api_key_env: ANTHROPIC_API_KEY

# Logging
log_level: info   # debug | info | warn | error
```

### Environment variables

| Variable | Description |
|---|---|
| `DICODE_CONFIG` | Path to config file (default: `dicode.yaml`) |
| `DICODE_HEADLESS` | Set to `true` to disable tray/desktop notifications |
| `DICODE_DATA_DIR` | Directory for DB and master key (default: `~/.dicode`) |
| `DICODE_MASTER_KEY` | Master encryption key (overrides `~/.dicode/master.key`) |
| `DICODE_API_SECRET` | REST API auth secret |
| `DICODE_RELAY_TOKEN` | Webhook relay account token |

---

## CLI reference

```
dicode [--config <path>] [--tray] [--headless]

Subcommands:
  dicode version                         Print version

  dicode task validate <id|--all>        Schema + syntax + cycle check
  dicode task test <id|--all> [--watch]  Run task.test.js
  dicode task run <id> [options]         Execute a task
    --dry-run                            Intercepted HTTP, no side effects
    --verbose                            Show full log
    --param key=value                    Override a parameter
  dicode task commit <id> --to <source>  Promote local task to git
  dicode task diff <id>                  Show changes vs committed version
  dicode task install <url> [--param k=v] Install from store

  dicode secrets set <key> <value>
  dicode secrets get <key>
  dicode secrets list
  dicode secrets delete <key>

  dicode ci init --github|--gitlab       Generate CI workflow file

  dicode service install [--headless]    Install as system service
  dicode service uninstall
  dicode service start|stop|restart
  dicode service status
  dicode service logs

  dicode relay status                    Show relay connection status
```
