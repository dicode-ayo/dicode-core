# Podman Runtime

dicode supports running containers via [Podman](https://podman.io) — a
daemonless, rootless container engine that is a drop-in CLI alternative to
Docker.

Unlike the Deno and Python runtimes, dicode does **not** download Podman
automatically. It must be installed via your system package manager.

---

## Setup

1. Install Podman via your package manager:
   - **Fedora / RHEL:** `dnf install podman`
   - **Ubuntu / Debian:** `apt install podman`
   - **macOS:** `brew install podman`
   - See [podman.io/docs/installation](https://podman.io/docs/installation) for full instructions.
2. Open **Config → Runtimes** in the dicode web UI — Podman will show as **installed** once the binary is found in `PATH`.
3. Tasks with `runtime: podman` will now run.

> dicode searches `PATH` for the `podman` binary at startup. No `dicode.yaml` entry is required.

---

## Task structure

Uses the same `docker:` config section as the Docker runtime — no new fields needed.

```
tasks/
└── my-container-task/
    └── task.yaml
```

### task.yaml

```yaml
name: Nginx Dev Server
description: Serves /tmp on port 8888. Kill from the run page when done.
runtime: podman

trigger:
  manual: true

docker:
  image: nginx:alpine
  pull_policy: missing       # always | missing (default) | never
  ports:
    - "8888:80"              # host:container
  volumes:
    - "/tmp:/usr/share/nginx/html:ro"
```

A more complete example:

```yaml
name: Data Pipeline
runtime: podman

trigger:
  cron: "0 3 * * *"

docker:
  image: python:3.12-slim
  command: ["python", "/scripts/pipeline.py"]
  pull_policy: missing
  volumes:
    - "/data/input:/input:ro"
    - "/data/output:/output"
  working_dir: /scripts
  env_vars:
    BATCH_SIZE: "500"
```

---

## Differences from the Docker runtime

| | Docker | Podman |
|---|---|---|
| Daemon required | Yes (`dockerd`) | No — daemonless |
| Runs as | Root (by default) | Rootless (by default) |
| Binary management | System / Docker Desktop | System package manager |
| dicode integration | Go SDK (`docker/docker/client`) | CLI subprocess |
| stdout/stderr | Multiplexed Docker framing | Plain line-by-line streams |
| Config section | `docker:` | `docker:` (same) |

**Rootless containers** — Podman runs containers as the current user by default,
which means port numbers below 1024 may require additional system configuration
(`sysctl net.ipv4.ip_unprivileged_port_start=80`).

---

## Live logs

Container stdout is streamed as **info**-level log entries; stderr as **warn**-level entries. Both are visible in real-time on the run detail page.

---

## Kill

Podman tasks may run indefinitely. Use the **Kill** button on the run detail page (or `POST /api/runs/{runID}/kill`) to stop the container gracefully (`podman stop --time 10`).

---

## No default timeout

Unlike JS tasks (60 s default), Podman tasks have no timeout unless you set `timeout:` explicitly in `task.yaml`.

---

## Orphan cleanup

Containers are named `dicode-<runID>`. On startup, dicode removes any containers with a `dicode.run-id` label left behind by a previous session that was killed ungracefully.

---

## Hardening fields

The `docker:` section supports container-isolation fields that map to podman CLI flags:

| Field | Podman flag |
| --- | --- |
| `network_mode` | `--network` |
| `extra_hosts` | `--add-host` (repeated) |
| `cap_drop` / `cap_add` | `--cap-drop` / `--cap-add` |
| `security_opt` | `--security-opt` |
| `read_only` | `--read-only` |
| `user` | `--user` |

A startup warning is emitted for values that visibly weaken isolation — `network_mode: host`, `cap_add` of `SYS_ADMIN`/`SYS_PTRACE`/`SYS_MODULE`/`ALL`, and `security_opt` disabling seccomp/AppArmor/SELinux. The task still runs; the warning surfaces in the UI so reviewers notice.

See [Task Format → Container fields reference](./concepts/task-format.md#container-fields-reference) for the full schema and a hardened-defaults example.

---

## Configuration reference

No `dicode.yaml` entry is required. Podman is registered automatically if the binary is found in `PATH` at startup.

See [Task Format](./concepts/task-format.md) for the full `task.yaml` and `docker:` field reference.
