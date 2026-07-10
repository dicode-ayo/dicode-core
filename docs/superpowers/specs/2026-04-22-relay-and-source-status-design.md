# Relay connection + taskset pull status — design

Addresses [#87](https://github.com/dicode-ayo/dicode-core/issues/87) and extends scope with a per-source pull-status indicator in the task list (the latent bug surfaced by [#175](https://github.com/dicode-ayo/dicode-core/issues/175) warranted making pull health visible in the UI).

## User-visible outcome

- **Header pill:** small rounded badge in the web UI header showing `Relay: connected` (green), `Relay: disconnected, retry in Xs` (red), or hidden entirely when the relay is disabled in config. Hover tooltip shows the remote URL, time since last state change, and reconnect-attempt count.
- **Per-source dot in the task list:** inline green/red/grey dot in each source-group header (next to the namespace label), derived from the source's last pull. Hover tooltip: "last pull: 2m ago · OK" or the error message when the pull failed. Local sources show no dot (N/A).

## Backend

### Relay status

The relay client is not a Go package — it runs as the `buildin/relay-client` TS task (migrated from Go in PR #254; see `docs/implementation-plan.md`). There is no `pkg/relay.Client`, no `statusState`, and no `Status()` accessor. Instead, the task publishes a JSON status blob via `dicode.kv.set("status", ...)` on every connect/disconnect/reconnect transition.

### `GET /api/relay/status`

`apiRelayStatus` in `pkg/webui/relay_status.go` reads the kv row at key `"buildin/relay-client:status"` (via a direct SQL `SELECT value FROM kv WHERE key = ?`, since the kv table is namespaced by task ID) and passes the JSON straight through as the response body. Returns `{"enabled":false}` when no status row exists yet — relay disabled in config, or the task hasn't completed its first connect. The response schema (`connected`, `remote_url`, `since`, `last_error`, `reconnect_attempts`, …) is owned by the relay-client task, not by a Go struct.

### `pkg/taskset.Source`

Add pull-status tracking:

```go
type PullStatus struct {
    LastPullAt time.Time `json:"last_pull_at"`
    OK         bool      `json:"ok"`
    Error      string    `json:"error,omitempty"`
}

func (s *Source) PullStatus() PullStatus
```

Updated on every pull attempt:
- Initial clone/pull path in `Start()` (around `source.go:95`).
- Ticker-driven pull at `source.go:219-225`.
- `Sync()`-triggered pull at `source.go:260-265`.

Thread-safe via a mutex; local (non-git) sources leave the zero value so the API can skip them.

### `pkg/webui.SourceInfo` additions

```go
type SourceInfo struct {
    // ...existing fields...
    LastPullAt    *time.Time `json:"last_pull_at,omitempty"`
    LastPullOK    bool       `json:"last_pull_ok,omitempty"`
    LastPullError string     `json:"last_pull_error,omitempty"`
}
```

`LastPullAt` is a `*time.Time`, not a value: `time.Time` + `omitempty` does not omit the zero value (it serializes as `"0001-01-01T00:00:00Z"`, which is truthy in JS and would render a spurious status dot for every local / never-pulled source). Additive — existing MCP and webui clients ignore unknown fields. `SourceManager.List()` (`pkg/webui/sources.go`) populates the three new fields from `Source.PullStatus()` only once a pull has actually been attempted, leaving them nil/omitted otherwise.

## Frontend

### `dc-relay-status`

New file `tasks/buildin/webui/app/components/dc-relay-status.js`, ~60 LoC Lit element:

```js
class DcRelayStatus extends LitElement {
  async _poll() {
    this._status = await get('/api/relay/status');
  }
  connectedCallback() { super.connectedCallback(); this._poll(); this._timer = setInterval(() => this._poll(), 5000); }
  disconnectedCallback() { super.disconnectedCallback(); clearInterval(this._timer); }
  render() {
    if (!this._status?.enabled) return html``;
    const cls = this._status.connected ? 'ok' : 'err';
    const text = this._status.connected ? 'Relay: connected' : 'Relay: disconnected';
    const tt = this._tooltip();
    return html`<span class="pill pill-${cls}" title="${tt}">${text}</span>`;
  }
  _tooltip() {
    const s = this._status;
    const rel = relativeTime(s.since);
    if (s.connected) return `${s.remote_url} · connected ${rel}`;
    return `${s.remote_url} · disconnected ${rel}${s.last_error ? ' · ' + s.last_error : ''} · ${s.reconnect_attempts} retries`;
  }
}
```

Mounted once in the app-shell template next to the existing navigation. Styling: tiny reuse of `.pill` classes or a three-line local block.

### `dc-task-list` source-group dot

Fetches `/api/sources` alongside `/api/tasks`, keys by source name, and augments the existing source-group header at [dc-task-list.js:120-124](tasks/buildin/webui/app/components/dc-task-list.js#L120) with a colored dot + tooltip:

```js
const src = this._sourceByName.get(ns);
const dot = src ? html`<span class="dot dot-${pullDotClass(src)}" title="${pullTooltip(src)}"></span>` : '';
```

`pullDotClass(src)`:
- No `last_pull_at` → grey "never"
- `last_pull_ok === true` → green
- `last_pull_ok === false` → red

`pullTooltip(src)`:
- `"last pull: 5m ago · OK"` (green)
- `"last pull: 2m ago · error: pull: object not found"` (red)
- `""` skipped for grey

Local sources don't set `last_pull_at`, so they fall through to grey and we render no dot (suppressed in the template).

## Testing

| Test | File | Asserts |
|---|---|---|
| `TestAPIRelayStatus_Disabled` | `pkg/webui/relay_status_test.go` | `/api/relay/status` with no kv status row returns `{"enabled":false}` |
| `TestAPIRelayStatus_FromKv` | same | With a status row present, the response passes the stored JSON through verbatim |
| `TestSource_PullStatus_InitialZero` | `pkg/taskset/pull_status_test.go` | New Source has zero-value PullStatus |
| `TestSource_RecordPull_Success` | same | After a successful pull, `OK=true` and `LastPullAt` updated |
| `TestSource_RecordPull_Failure` | same | After a pull against a bogus URL, `OK=false` and `Error` populated |
| `TestSource_RecordPull_ErrorClearedOnNextSuccess` | same | A prior error is cleared once a later pull succeeds |
| `TestSourceManager_List_LocalSource_NoPullFieldsInJSON` | `pkg/webui/sources_test.go` | Local sources omit the pull-health fields entirely (nil `LastPullAt`, no spurious dot) |

Relay-client connect/reconnect behavior is covered on the TypeScript side in `tasks/buildin/relay-client/task.test.ts` (there is no Go relay client to unit test).

Frontend has no automated tests today for these components (consistent with the rest of `tasks/buildin/webui/`); manual smoke test covers it.

## Rollout / migration

- Additive JSON fields on `SourceEntry` — MCP and webui clients ignore unknowns.
- Polling every 5s adds ≤1 req/s against the daemon from each active tab. Negligible.
- No database migration; all state is in-memory.

## Out of scope

- WebSocket push for relay-status transitions — poll is fine for alpha.
- Per-task (not per-source) status indicators — redundant since a source maps 1→many tasks.
- Dashboard acknowledgement / mute UX for persistent failures — post-alpha.
