# Relay connection + taskset pull status — design

Addresses [#87](https://github.com/dicode-ayo/dicode-core/issues/87) and extends scope with a per-source pull-status indicator in the task list (the latent bug surfaced by [#175](https://github.com/dicode-ayo/dicode-core/issues/175) warranted making pull health visible in the UI).

## User-visible outcome

- **Header pill:** small rounded badge in the web UI header showing `Relay: connected` (green), `Relay: disconnected, retry in Xs` (red), or hidden entirely when the relay is disabled in config. Hover tooltip shows the remote URL, time since last state change, and reconnect-attempt count.
- **Per-source dot in the task list:** inline green/red/grey dot in each source-group header (next to the namespace label), derived from the source's last pull. Hover tooltip: "last pull: 2m ago · OK" or the error message when the pull failed. Local sources show no dot (N/A).

## Backend

### `pkg/relay.Client`

Add status tracking fields and a public accessor:

```go
type statusState struct {
    mu                sync.RWMutex
    connected         bool
    since             time.Time
    lastError         string
    reconnectAttempts int
}

type Status struct {
    Enabled           bool      `json:"enabled"`            // true when a relay client was constructed
    Connected         bool      `json:"connected"`
    RemoteURL         string    `json:"remote_url,omitempty"`
    HookBaseURL       string    `json:"hook_base_url,omitempty"`
    Since             time.Time `json:"since"`              // when the current (dis)connected state started
    LastError         string    `json:"last_error,omitempty"`
    ReconnectAttempts int       `json:"reconnect_attempts"`
}

func (c *Client) Status() Status { /* reads under statusState.mu */ }
```

Status updates:
- On successful handshake (inside `runOnce`, after the welcome): `connected=true, since=now, lastError="", reconnectAttempts=0`.
- On `runOnce` error (in `Run`'s reconnect loop): `connected=false, since=now, lastError=err.Error(), reconnectAttempts++`.

The existing broker-protocol and hook-base-url fields are already tracked; `Status()` reads them under their existing mutexes and copies into the returned struct.

### `GET /api/relay/status`

New handler in `pkg/webui` that returns `Status` as JSON. Falls back to `{"enabled": false}` when `s.relayClient == nil`. Same auth rules as the rest of `/api/*`.

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

### `mcp.SourceEntry` additions

```go
type SourceEntry struct {
    // ...existing fields...
    LastPullAt   time.Time `json:"last_pull_at,omitempty"`
    LastPullOK   bool      `json:"last_pull_ok,omitempty"`
    LastPullError string   `json:"last_pull_error,omitempty"`
}
```

Additive — existing MCP clients ignore unknown fields. `SourceManager.List()` populates the three new fields from `Source.PullStatus()` when the underlying source is a live taskset.

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
| `TestClient_Status_WhenDisabled` | `pkg/relay/client_test.go` | Zero-value Client reports `Enabled=false` |
| `TestClient_Status_AfterHandshake` | same | After a successful handshake simulation, `Connected=true`, `Since` populated, `LastError==""` |
| `TestClient_Status_AfterFailure` | same | After a reconnect-loop error, `Connected=false`, `LastError` set, `ReconnectAttempts` ticked |
| `TestRelayStatusAPI_Disabled` | `pkg/webui/server_test.go` | `/api/relay/status` with `s.relayClient==nil` returns `{"enabled":false}` |
| `TestRelayStatusAPI_Connected` | same | With a stubbed-status Client, returns the serialized Status |
| `TestSource_PullStatus_Initial` | `pkg/taskset/source_test.go` | New Source has zero-value PullStatus |
| `TestSource_PullStatus_AfterOK` | same | After a successful pull tick (hit the live bare repo), `OK=true` and `LastPullAt` updated |
| `TestSource_PullStatus_AfterError` | same | After a pull against a bogus URL, `OK=false` and `Error` populated |
| `TestSourceManager_List_PopulatesPull` | `pkg/webui/sources_test.go` | `List()` carries PullStatus into SourceEntry for live taskset sources |

Frontend has no automated tests today for these components (consistent with the rest of `tasks/buildin/webui/`); manual smoke test covers it.

## Rollout / migration

- Additive JSON fields on `SourceEntry` — MCP and webui clients ignore unknowns.
- Polling every 5s adds ≤1 req/s against the daemon from each active tab. Negligible.
- No database migration; all state is in-memory.

## Out of scope

- WebSocket push for relay-status transitions — poll is fine for alpha.
- Per-task (not per-source) status indicators — redundant since a source maps 1→many tasks.
- Dashboard acknowledgement / mute UX for persistent failures — post-alpha.
