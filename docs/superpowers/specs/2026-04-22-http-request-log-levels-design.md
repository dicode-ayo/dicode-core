# HTTP request log levels — design

Addresses [#23](https://github.com/dicode-ayo/dicode-core/issues/23). The chi `middleware.Logger` at [pkg/webui/server.go:322](../../../pkg/webui/server.go#L322) emits every HTTP request at the default (info) level, drowning application-level log lines. This spec replaces it with a zap-based middleware that routes by status code and adds a per-request correlation ID.

## Acceptance (copied from #23, plus one adjacent)

- 2xx/3xx request lines logged at **debug**
- 4xx logged at **warn**
- 5xx logged at **error**
- No change to existing structured log fields *outside the request logger*
- **Adjacent:** every request carries a `req_id` field for correlation (requires `middleware.RequestID`)

## Design

### New file: `pkg/webui/log_middleware.go`

Two types implementing chi's `middleware.LogFormatter` / `middleware.LogEntry` interfaces:

```go
type zapLogFormatter struct{ log *zap.Logger }

func (f *zapLogFormatter) NewLogEntry(r *http.Request) middleware.LogEntry {
    return &zapLogEntry{
        log:    f.log,
        method: r.Method,
        path:   r.URL.Path,
        remote: r.RemoteAddr,
        reqID:  middleware.GetReqID(r.Context()),
    }
}

type zapLogEntry struct {
    log                          *zap.Logger
    method, path, remote, reqID  string
}

func (e *zapLogEntry) Write(status, bytes int, _ http.Header, elapsed time.Duration, _ interface{}) {
    fields := []zap.Field{
        zap.String("method", e.method),
        zap.String("path",   e.path),
        zap.Int("status",    status),
        zap.Int("bytes",     bytes),
        zap.Duration("dur",  elapsed),
        zap.String("remote", e.remote),
        zap.String("req_id", e.reqID),
    }
    switch {
    case status >= 500:
        e.log.Error("http", fields...)
    case status >= 400:
        e.log.Warn("http", fields...)
    default:
        e.log.Debug("http", fields...)
    }
}

func (e *zapLogEntry) Panic(v interface{}, stack []byte) {
    e.log.Error("http panic",
        zap.String("method", e.method),
        zap.String("path",   e.path),
        zap.String("req_id", e.reqID),
        zap.Any("panic",     v),
        zap.ByteString("stack", stack),
    )
}
```

### Wire-up in `pkg/webui/server.go`

Replace the bare `r.Use(middleware.Logger)` with:

```go
r.Use(middleware.RequestID)
r.Use(middleware.RequestLogger(&zapLogFormatter{log: s.log}))
r.Use(middleware.Recoverer)
```

**Ordering is load-bearing.** `RequestID` must precede `RequestLogger` so `GetReqID` resolves inside `NewLogEntry`. `RequestLogger` must precede `Recoverer` because chi's `Recoverer` reads the `LogEntry` from the request context (populated by `RequestLogger`) to route panics through our `Panic` method — reverse the order and panics fall through to chi's `PrintPrettyStack` on stderr, bypassing zap entirely.

### Level semantics

| Status | Level | Rationale |
|---|---|---|
| 1xx/2xx/3xx | Debug | Normal traffic, silenced by default, visible under `--log-level debug`. |
| 4xx | Warn | Client errors — usually the caller's fault but worth seeing. Matches existing `s.log.Warn` usage for auth rejections. |
| 5xx | Error | Server-side failure, always surfaced. |
| panic | Error | Captured via `Panic(v, stack)`, includes the stack bytes. |

### What stays unchanged

- Existing `s.log.Info/Warn/Error` call sites inside handlers (login flow, config save, etc.) — untouched.
- `middleware.Recoverer` still catches panics and writes 500; only its position in the stack changes (inner to `RequestLogger`).
- `securityHeaders` and the rest of the chain — unchanged.

## Testing

New file: `pkg/webui/log_middleware_test.go`.

Use `go.uber.org/zap/zaptest/observer` to build an in-memory logger, run httptest requests, assert level + fields.

| Test | Setup | Asserts |
|---|---|---|
| `TestRequestLogger_2xx_Debug` | handler returns 200 | one observed entry at `DebugLevel` with `status=200`, `method`, `path` |
| `TestRequestLogger_4xx_Warn`  | handler returns 404 | one entry at `WarnLevel`, `status=404` |
| `TestRequestLogger_5xx_Error` | handler returns 500 | one entry at `ErrorLevel`, `status=500` |
| `TestRequestLogger_ReqID_Propagates` | stack with `middleware.RequestID` + formatter | entry's `req_id` field is non-empty and matches `X-Request-Id` response header |
| `TestRequestLogger_Panic_Logged` | handler panics, stack has `Recoverer` then our formatter | entry at `ErrorLevel` with `msg="http panic"` and a `panic` field |

No change to existing tests.

## Migration / rollout

Zero-config. Behaviour change: previously-visible request lines disappear at default log level. Docs already mention `log_level: debug` as the way to re-enable verbose output — no README edit required.

## Out of scope

- Request ID in response header for client-side correlation — `middleware.RequestID` already sets `X-Request-Id` automatically; no extra work.
- Structured-only output format (JSON). Zap's configured encoder (currently console) governs the on-wire format; we're only emitting fields.
- Sampling / rate-limiting of high-volume debug lines — not needed while debug is off by default.
