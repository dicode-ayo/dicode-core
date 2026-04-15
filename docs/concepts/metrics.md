# Metrics endpoint

Dicode exposes a lightweight JSON metrics endpoint that surfaces daemon health, active task counts, and child-process resource usage. No external monitoring agent is required.

## Endpoint

```
GET /api/metrics
```

Requires authentication when `server.auth: true` is set in `dicode.yaml`. Returns `401 Unauthorized` for unauthenticated requests.

## Response schema

```json
{
  "daemon": {
    "heap_alloc_mb": 42.3,
    "heap_sys_mb": 68.0,
    "goroutines": 87,
    "cpu_ms": 1240
  },
  "tasks": {
    "active_tasks": 5,
    "children_rss_mb": 310.5,
    "children_cpu_ms": 4800,
    "max_concurrent_tasks": 8,
    "active_task_slots": 5,
    "waiting_tasks": 0
  }
}
```

### `daemon` object

| Field | Type | Description |
|---|---|---|
| `heap_alloc_mb` | float | Go heap currently allocated (MB) |
| `heap_sys_mb` | float | Go heap reserved from OS (MB) |
| `goroutines` | int | Number of live goroutines |
| `cpu_ms` | int | Daemon CPU time (user+sys) in milliseconds — **Linux only**, omitted otherwise |

### `tasks` object

| Field | Type | Description |
|---|---|---|
| `active_tasks` | int | Number of task runs currently in progress. **Includes runs queued on the concurrency semaphore** — subtract `waiting_tasks` to get the count of runs actually executing |
| `children_rss_mb` | float | Aggregate RSS of all active Deno child processes (MB) — **Linux only**, omitted otherwise |
| `children_cpu_ms` | int | Aggregate CPU time (user+sys) of all active Deno child processes (ms) — **Linux only**, omitted otherwise |
| `max_concurrent_tasks` | int | Configured concurrency cap (`execution.max_concurrent_tasks`). `0` means unlimited |
| `active_task_slots` | int | Semaphore slots currently held by running task goroutines. `0` when no cap is configured |
| `waiting_tasks` | int | Task goroutines parked waiting for a free semaphore slot. Non-zero values indicate backpressure — consider raising `max_concurrent_tasks` |

## Platform notes

Fields marked "Linux only" are sourced from `/proc/self/stat` and `/proc/<pid>/status`. On macOS, Windows, and other non-Linux platforms those fields are omitted from the JSON object (`omitempty`), so the response remains valid and parseable — the fields simply do not appear.

## WebUI dashboard

The webui example includes a `dc-metrics` component at the `/metrics` route that polls `GET /api/metrics` every 5 seconds and renders daemon and task cards automatically. No configuration required.

## IPC control socket (`cli.metrics`)

Metrics are also accessible over the daemon's Unix control socket, enabling the CLI and TUI to query live health without going through HTTP.

**Request:**
```json
{ "id": "1", "method": "cli.metrics" }
```

**Response** (`result` field):
```json
{
  "daemon": { "heap_alloc_mb": 42.3, "goroutines": 87, "cpu_ms": 1240 },
  "tasks":  { "active_tasks": 5, "children_rss_mb": 310.5 }
}
```

The response shape (`MetricsSnapshot`) mirrors the HTTP JSON exactly. The field set is identical to `/api/metrics`; Linux-only `/proc` fields are omitted on other platforms.

Authentication uses the pre-shared CLI token written to `dataDir/daemon.token` on daemon startup — the same token used by all `cli.*` commands.

## Metrics are computed on-demand

There is no background aggregation thread. Every request to `/api/metrics` reads current values synchronously:

- `runtime/metrics.Read()` for Go heap figures (no stop-the-world pause)
- `runtime.NumGoroutine()` for goroutine count
- `/proc/self/stat` for daemon CPU time (Linux)
- `/proc/<pid>/stat` for each active Deno child CPU time (Linux)
- `/proc/<pid>/status` for each active Deno child RSS (Linux)
- The engine's `runCancels` map for active task count

This means the endpoint is always fresh and adds negligible overhead.
