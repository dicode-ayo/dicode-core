# Best Practices Reference: Go + HTMX + CSS

> Generated 2026-03-24. Tailored to this codebase: `github.com/dicode/dicode`.
> Go 1.25, chi v5, zap, SQLite (modernc), HTMX 1.9.x, vanilla CSS.

---

## Table of Contents

1. [Go Best Practices](#1-go-best-practices)
   - 1.1 Error Handling
   - 1.2 Security
   - 1.3 HTTP Server Security
   - 1.4 Concurrency
   - 1.5 Interface Design
   - 1.6 Testing
   - 1.7 Logging
   - 1.8 Configuration
   - 1.9 Memory & Allocations
   - 1.10 Package Naming & Structure
   - 1.11 Static Analysis
   - 1.12 Global State
   - 1.13 Resource Cleanup
   - 1.14 DRY Principles
   - 1.15 net/http Middleware Patterns
   - 1.16 Database
   - 1.17 Context Values
2. [HTMX Best Practices](#2-htmx-best-practices)
3. [CSS Best Practices](#3-css-best-practices)

---

## 1. Go Best Practices

### 1.1 Error Handling

#### Wrap errors at every boundary with `%w`

Always wrap errors with context as they travel up the call stack. Use `%w` (not `%v`) so callers can inspect with `errors.Is`/`errors.As`.

```go
// Good — this codebase already does this in most places
if err := yaml.NewDecoder(f).Decode(&cfg); err != nil {
    return nil, fmt.Errorf("parse config: %w", err)
}

// Bad — loses type information, callers cannot unwrap
return nil, fmt.Errorf("parse config: %v", err)
```

#### Sentinel errors for known states

Define sentinel errors as package-level variables for conditions callers must check.

```go
// In pkg/secrets/provider.go — this codebase uses a custom type; both are valid
var ErrNotFound = errors.New("not found")

// Custom error type (current pattern in this repo) — preferred when you need fields
type NotFoundError struct{ Key string }
func (e *NotFoundError) Error() string { return fmt.Sprintf("secret %q not found", e.Key) }

// Caller usage
var nfe *secrets.NotFoundError
if errors.As(err, &nfe) {
    // handle missing key
}
```

#### Avoid naked `errors.New` strings that leak to users

Error strings should not be capitalised and should not end with punctuation (Go convention). Do not expose internal error strings directly in HTTP responses.

```go
// Good
return fmt.Errorf("start run record: %w", err)

// Bad — capital letter, leaks internals to HTTP response
jsonErr(w, "Could not start run: "+err.Error(), 500)
```

#### `errors.Is` vs `errors.As`

- `errors.Is` — compare to a sentinel value (pointer equality through the chain).
- `errors.As` — extract a typed error from anywhere in the chain.

```go
if errors.Is(err, context.Canceled) { ... }          // sentinel
if errors.As(err, new(*url.Error)) { ... }            // typed
```

#### Do not ignore errors silently

The `_ = someCall()` pattern is acceptable only for documented best-effort cases (e.g. `_ = s.srv.Shutdown(shutCtx)`). Always log or propagate.

---

### 1.2 Security

#### Path traversal prevention

Any handler that joins a user-supplied value into a file path must validate the result stays inside the intended directory. This codebase uses an allowlist (`allowedFiles`) for the file editor API — that pattern is correct and should be applied consistently.

```go
// Current pattern — good, explicit allowlist
var allowedFiles = map[string]bool{"task.js": true, "task.test.js": true}
if !allowedFiles[filename] {
    jsonErr(w, "file not allowed", http.StatusBadRequest)
    return
}

// Alternative: use filepath-securejoin (already in go.mod as a transitive dep)
import "github.com/cyphar/filepath-securejoin"
safe, err := securejoin.SecureJoin(spec.TaskDir, filename)
if err != nil {
    jsonErr(w, "invalid path", http.StatusBadRequest)
    return
}
```

Never do:
```go
// Dangerous — user controls filename, could be ../../etc/passwd
path := filepath.Join(spec.TaskDir, r.PathValue("filename"))
os.ReadFile(path)
```

#### SQL injection prevention

Always use parameterised queries. Never interpolate user input into query strings. This codebase uses a `db.Exec`/`db.Query` abstraction with `[]any` parameters — the pattern is correct.

```go
// Good (current pattern)
r.db.Exec(ctx, `INSERT INTO runs ... VALUES (?, ?, ?, ?, ?)`, id, taskID, ...)

// Never do this
query := "SELECT * FROM runs WHERE id = '" + userInput + "'"
```

#### SSRF prevention

The `handleAIStream` handler calls an external API at a URL from config (`cfg.AI.BaseURL`). If this URL can be set via the UI (`/api/settings/ai`), validate it against an allowlist of acceptable prefixes before using it in the HTTP client. Never let arbitrary user-supplied URLs be used as HTTP endpoints.

```go
// Suggested guard for apiSaveAISettings
allowed := []string{
    "https://api.openai.com/",
    "https://api.anthropic.com/",
    "http://localhost:",
    "http://127.0.0.1:",
}
func isAllowedBaseURL(u string) bool {
    for _, prefix := range allowed {
        if strings.HasPrefix(u, prefix) { return true }
    }
    return false
}
```

#### Secrets in config

`AIConfig.APIKey` is stored in `dicode.yaml` and serialised into `apiGetConfig`. The `/api/config` endpoint returns the full `Config` struct including `AIKey`. Ensure this endpoint is either gated behind authentication or the `APIKey` field is redacted before serialisation.

```go
// Add a MarshalJSON or use a view-model struct without the key field
type aiConfigView struct {
    BaseURL   string `json:"base_url"`
    Model     string `json:"model"`
    APIKeySet bool   `json:"api_key_set"` // only signal presence, never the value
}
```

---

### 1.3 HTTP Server Security

#### Set timeouts — current code is missing them

`Start()` creates an `http.Server` with no timeouts:
```go
// Current — missing all timeouts
s.srv = &http.Server{
    Addr:    fmt.Sprintf(":%d", s.port),
    Handler: s.Handler(),
}
```

Add read, write, and idle timeouts. SSE endpoints need a longer (or zero) write timeout — handle that via per-handler `http.ResponseController` in Go 1.20+.

```go
s.srv = &http.Server{
    Addr:         fmt.Sprintf(":%d", s.port),
    Handler:      s.Handler(),
    ReadTimeout:  10 * time.Second,
    WriteTimeout: 30 * time.Second,  // increase or set 0 for SSE routes
    IdleTimeout:  120 * time.Second,
    ReadHeaderTimeout: 5 * time.Second,
}
```

#### Security headers middleware

Add a middleware that sets hardening headers on every response. The current router has no such middleware.

```go
func securityHeaders(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("X-Content-Type-Options", "nosniff")
        w.Header().Set("X-Frame-Options", "DENY")
        w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
        // Tighten CSP once HTMX inline scripts are moved to external files
        w.Header().Set("Content-Security-Policy",
            "default-src 'self'; script-src 'self' 'unsafe-inline' https://unpkg.com; style-src 'self' 'unsafe-inline'")
        next.ServeHTTP(w, r)
    })
}

// In Handler():
r.Use(securityHeaders)
```

#### Rate limiting

There is currently no rate limiting. The `/secrets/unlock` and `/api/tasks/{id}/run` endpoints are especially worth protecting.

```go
// Using golang.org/x/time/rate (stdlib-adjacent)
import "golang.org/x/time/rate"

limiter := rate.NewLimiter(rate.Every(time.Second), 20) // 20 req/s burst

func rateLimitMiddleware(l *rate.Limiter) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            if !l.Allow() {
                http.Error(w, "too many requests", http.StatusTooManyRequests)
                return
            }
            next.ServeHTTP(w, r)
        })
    }
}
```

#### Authentication middleware

The current auth model is cookie-based only for `/secrets`. Consider a general auth middleware for the entire UI if the server is exposed on a network.

```go
func (s *Server) requireAuth(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if s.cfg.Server.Secret == "" {
            next.ServeHTTP(w, r) // no auth configured
            return
        }
        // check token from header or cookie
    })
}
```

#### Session cookie security

Current cookie in `handleSecretsUnlock` is missing the `Secure` flag. When deployed behind HTTPS this flag must be set.

```go
http.SetCookie(w, &http.Cookie{
    Name:     secretsCookie,
    Value:    token,
    Path:     "/secrets",
    HttpOnly: true,
    Secure:   true,                  // add this
    SameSite: http.SameSiteStrictMode,
    MaxAge:   8 * 3600,
})
```

---

### 1.4 Concurrency

#### Goroutine leak prevention

Every goroutine launched with `go func()` must have a clear termination condition driven by context cancellation or channel close. The `fireAsync` goroutine is correctly terminated by `runCtx`. The `startSource` forwarder goroutine relies on the source channel being closed when `srcCtx` is cancelled — ensure every `source.Start` implementation closes its output channel on context cancellation.

```go
// Pattern: always drain/close in the goroutine that owns the channel
go func() {
    defer close(out)
    for {
        select {
        case <-ctx.Done():
            return
        case ev := <-in:
            out <- ev
        }
    }
}()
```

#### Context propagation

Prefer `r.Context()` for work tied to the lifetime of an HTTP request. Use `context.Background()` only for work that must outlive the request (e.g. writing run records after the HTTP response is sent).

```go
// Current — fireAsync correctly uses context.Background() for the run goroutine
// because the run must continue even after the HTTP response is sent.
go func() { e.dispatch(runCtx, spec, opts) }()
```

Do not store a `context.Context` in a struct field. The `Reconciler.runCtx` field is a code smell — pass context explicitly to methods instead.

```go
// Avoid
type Reconciler struct { runCtx context.Context }

// Prefer — pass ctx as parameter
func (rc *Reconciler) startSource(ctx context.Context, src source.Source) error { ... }
```

#### Mutex discipline

- Hold mutexes for the minimum duration needed.
- Never call external functions (I/O, network) while holding a mutex.
- Prefer `sync.RWMutex` for read-heavy maps (already done in `Registry`).
- Do not copy a `sync.Mutex` value — always use pointer receivers or embed in a struct allocated on the heap.

#### Channel patterns

- Buffered channels for fan-out/broadcast (e.g. `LogBroadcaster` client channels — `make(chan string, 64)` is correct).
- The `select { case ch <- line: default: }` non-blocking send in `LogBroadcaster.Write` is the right pattern to avoid blocking the writer when a slow client is present.
- Close channels only from the sender side; never from the receiver.

#### sync.Map vs `map` + `sync.Mutex`

`sync.Map` is optimal for write-once, read-many patterns. `runCancels sync.Map` in `Engine` is appropriate because entries are written once on run start and deleted once on run end. For the `webhooks` map that is rebuilt on `Register`/`Unregister`, a plain `sync.RWMutex` + map is better (current code does this correctly).

---

### 1.5 Interface Design

#### Accept interfaces, return concrete types

```go
// Good — SecretsManager is an interface accepted as a parameter
type SecretsManager interface {
    List(ctx context.Context) ([]string, error)
    Set(ctx context.Context, key, value string) error
    Delete(ctx context.Context, key string) error
}

// Return the concrete *LocalProvider from its constructor, not the interface
func NewLocalProvider(...) *LocalProvider { ... }
```

#### Interface Segregation Principle (ISP)

Keep interfaces small. `SecretsManager` has three methods — that is a good size. Avoid defining an interface that includes every method of a struct.

#### Define interfaces at the usage site, not the definition site

The interface should live in the package that consumes it (`pkg/webui`), not in the package that implements it (`pkg/secrets`). This is already done correctly with `SecretsManager` in `pkg/webui`.

#### The `db.DB` interface

The `db.Scanner` and `db.DB` interfaces are a clean abstraction enabling `:memory:` SQLite in tests. This is a good pattern. Ensure the interface stays minimal — do not add methods just because the concrete type has them.

---

### 1.6 Testing

#### Table-driven tests

Use table-driven tests for any function with multiple input/output combinations.

```go
func TestTriggerLabel(t *testing.T) {
    tests := []struct {
        name  string
        input task.TriggerConfig
        want  string
    }{
        {"cron", task.TriggerConfig{Cron: "0 * * * *"}, "cron: 0 * * * *"},
        {"manual", task.TriggerConfig{Manual: true}, "manual"},
        {"webhook", task.TriggerConfig{Webhook: "/hooks/my"}, "webhook: /hooks/my"},
    }
    for _, tc := range tests {
        t.Run(tc.name, func(t *testing.T) {
            got := triggerLabel(tc.input)
            if got != tc.want {
                t.Errorf("got %q, want %q", got, tc.want)
            }
        })
    }
}
```

#### Test helpers with `t.Helper()`

`newTestServer` and `registerTask` correctly call `t.Helper()`. This should be applied to every helper function that calls `t.Fatal`/`t.Error`, so failure lines point to the test, not the helper.

#### Avoid polling in tests

The current `TestAPI_GetRun_and_Logs` uses a `time.Sleep(50ms)` polling loop. Use `testify/assert` eventually helpers or a channel signal from the engine instead. At minimum, replace the sleep loop with a deadline check.

```go
// Better: expose a way to wait for run completion in tests
// or use require.Eventually from testify
require.Eventually(t, func() bool {
    run, _ := reg.GetRun(ctx, runID)
    return run != nil && run.Status != registry.StatusRunning
}, 5*time.Second, 50*time.Millisecond)
```

#### Integration tests

Use `httptest.NewServer` and a real SQLite `:memory:` database (as the test suite does) for integration tests. Mark heavy tests with `t.Skip` when `CI` is not set, or use build tags.

```go
//go:build integration
package webui_test
```

#### Test isolation

Each test should create its own `t.TempDir()` for task files and a fresh in-memory database. The current `registerTask` helper does this correctly.

#### Subtests for parallel execution

```go
t.Run("subtest", func(t *testing.T) {
    t.Parallel()
    // ...
})
```

---

### 1.7 Logging

#### Structured logging — use zap consistently

This codebase uses `go.uber.org/zap` throughout, which is correct. All log calls should use typed fields (`zap.String`, `zap.Error`, `zap.Duration`) rather than `fmt.Sprintf` in the message.

```go
// Good (current pattern)
s.log.Info("run started", zap.String("task", spec.ID), zap.String("run", runID))

// Bad
s.log.Info(fmt.Sprintf("run started: task=%s run=%s", spec.ID, runID))
```

#### Do not log sensitive data

Never log secret values, passphrases, tokens, or API keys. Log only the key name, never the value.

```go
// Good
s.log.Info("secret set", zap.String("key", key))

// Bad — never do this
s.log.Debug("secret resolved", zap.String("key", key), zap.String("value", value))
```

#### Use appropriate log levels

| Level | When |
|-------|------|
| `Debug` | Verbose dev-only diagnostics |
| `Info` | Normal operational events (task started, task registered) |
| `Warn` | Degraded but recoverable states (config reload failed, dropped log line) |
| `Error` | Failures that need attention (run failed, DB error) |

Avoid logging at `Error` for expected conditions like "task not found" from an API request — that is a `Warn` or even `Info` with an error field.

#### Log sampling for high-frequency events

For the SSE log broadcaster (`Write`) which can be called thousands of times per second, avoid logging every dropped message. Use zap's built-in sampling logger.

```go
sampledLog := zap.New(zapcore.NewSamplerWithOptions(
    logger.Core(), time.Second, 100, 10,
))
```

---

### 1.8 Configuration

#### Validate at load time, fail fast

`config.validate()` is called in `Load` — this is correct. Every required field and every constraint should be checked here, not scattered through the application.

```go
func (cfg *Config) validate() error {
    if cfg.Server.Secret != "" && len(cfg.Server.Secret) < 16 {
        return fmt.Errorf("server.secret must be at least 16 characters")
    }
    // ... existing source validation
}
```

#### Never store secrets in config structs that get serialised

`AIConfig.APIKey` is exported and tagged `yaml:"api_key,omitempty"`. It will be included in `yaml.Marshal` output and in `apiGetConfig` JSON responses. Add a custom marshaller or use a separate secrets-only struct.

#### Environment variable expansion

`expandHome` only handles `~/`. Consider using `os.ExpandEnv` for paths that should support `$HOME` and other env vars, or document clearly that only `~/` is supported.

#### Defaults should be explicit

`applyDefaults` is a good pattern. Document each default with a comment explaining why that value was chosen.

---

### 1.9 Memory & Allocations

#### Avoid allocations in hot paths

The `LogBroadcaster.Write` is called for every log line. Avoid `strings.TrimRight(string(p), "\n")` which allocates; use `bytes.TrimRight` on the raw slice.

```go
// Avoids one string allocation
line := string(bytes.TrimRight(p, "\n"))
```

#### sync.Pool for frequently allocated objects

If template data map literals (e.g. `map[string]any{"Title": ..., "Tasks": ...}`) are allocated on every request, they are GC'd quickly. For very high request rates, consider `sync.Pool` for reusable buffers.

```go
var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
    buf := bufPool.Get().(*bytes.Buffer)
    buf.Reset()
    defer bufPool.Put(buf)
    // render to buf first, then copy to w
}
```

#### Ring buffer sizing

`LogBroadcaster.recent` is capped at 300 entries. Validate this is appropriate — at 1 KB/line that is 300 KB held per connected client's snapshot. Consider making it configurable.

#### Avoid `append` to nil slices in tight loops

In `Registry.All()` the slice is pre-allocated with `make([]*task.Spec, 0, len(r.tasks))` — this is correct.

---

### 1.10 Package Naming & Structure

#### Package names

- Short, lowercase, no underscores (except `_test`): `webui`, `registry`, `trigger` — all correct.
- Do not stutter: `secrets.Provider` not `secrets.SecretsProvider`.
- Test packages: use `package foo_test` (black-box) for integration tests, `package foo` (white-box) for unit tests that need unexported access.

#### Package structure

Current structure follows a clean layered architecture:

```
cmd/dicode/          — entry point only, wires dependencies
pkg/config/          — configuration loading
pkg/registry/        — domain: task registry + run log
pkg/trigger/         — domain: scheduling and execution
pkg/webui/           — delivery: HTTP handlers + templates
pkg/runtime/         — infrastructure: JS and Docker execution
pkg/secrets/         — infrastructure: secret providers
pkg/source/          — infrastructure: task discovery
```

This is good. Avoid letting `pkg/webui` import `pkg/trigger` implementation details — keep it to the `Engine` interface. Consider extracting an interface for `Engine` in `pkg/webui` to improve testability.

#### Avoid `pkg/` for everything

The `pkg/` prefix is a convention, not a requirement. For a Go module this is fine. Do not put main application logic in `internal/` unless you want to prevent external import — that is a valid choice for non-library code.

---

### 1.11 Static Analysis

#### Run `go vet` in CI (mandatory)

```makefile
lint:
    go vet ./...
```

#### staticcheck

```shell
go install honnef.co/go/tools/cmd/staticcheck@latest
staticcheck ./...
```

Key checks relevant to this codebase:
- `SA1006` — `Printf` with no verbs
- `SA4006` — unused variables
- `ST1003` — naming conventions
- `S1039` — unnecessary use of `fmt.Sprintf`

#### golangci-lint recommended config

```yaml
# .golangci.yml
linters:
  enable:
    - errcheck        # unchecked errors
    - gosimple        # simplifications
    - govet           # go vet checks
    - ineffassign     # useless assignments
    - staticcheck     # comprehensive static analysis
    - unused          # unused code
    - gosec           # security issues (SQL injection, path traversal, etc.)
    - bodyclose       # unclosed http.Response.Body
    - contextcheck    # context.Background() where ctx should be passed
    - noctx           # http.NewRequest without context
    - exhaustive      # missing switch cases on enums

linters-settings:
  gosec:
    excludes:
      - G304  # file path from variable — handled by allowlist pattern
```

---

### 1.12 Global State

#### Avoid package-level variables that hold mutable state

`allowedFiles` in `server.go` is a `map[string]bool` package variable — this is acceptable because it is read-only after init. Package-level mutable state (counters, caches, singletons) should be avoided in favour of dependency injection.

#### Avoid `init()` functions

Use explicit constructors (`New*` functions) instead of `init()` for setup logic. `init()` makes test isolation and dependency ordering hard to reason about.

#### Template `funcMap` is defined inline in `New`

This is good — it avoids a global `funcMap` that could be mutated concurrently. However, `slice`, `string`, `deref`, `list`, `not`, `derefBool` are generic utilities; consider whether they need to be in the funcMap at all or could be handled in the templates differently.

---

### 1.13 Resource Cleanup

#### Always `defer` close for files and connections

```go
// Good (current pattern in config.Load)
f, err := os.Open(path)
if err != nil { return nil, err }
defer f.Close()
```

#### Defer ordering matters

Defers execute LIFO. When deferring a mutex unlock and a resource close, the order should be:

```go
mu.Lock()
defer mu.Unlock()    // unlocks second (correct — released before function returns)
// ... work
```

#### HTTP response body

If this codebase makes any outbound HTTP calls (e.g. relay, notifications), always close `resp.Body`:

```go
resp, err := http.Get(url)
if err != nil { return err }
defer resp.Body.Close()
```

#### SSE handler cleanup

`handleLogsStream` correctly defers `s.logs.unsubscribe(ch)`. This is essential — without it, the client channel leaks and `clients` map grows unboundedly.

#### Graceful shutdown

`Start()` correctly implements graceful shutdown:
```go
go func() {
    <-ctx.Done()
    shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    _ = s.srv.Shutdown(shutCtx)
}()
```
The 5-second timeout for shutdown is reasonable. Note that active SSE connections will be forcibly closed — consider draining the log broadcaster before shutdown if log continuity matters.

---

### 1.14 DRY Principles in Go

#### When to abstract

Extract a function when:
- The same logic appears 3+ times (rule of three).
- The logic has a clear name and single responsibility.
- A helper reduces noise in a test or handler body.

`jsonOK` and `jsonErr` helpers are good examples — used on every handler.

#### When NOT to abstract

- Do not create an abstraction for two uses that might diverge.
- Do not create a helper that merely wraps one line (e.g. `func setHeader(w http.ResponseWriter, k, v string) { w.Header().Set(k, v) }`).
- Prefer duplication over the wrong abstraction — a shared function that takes a boolean flag to change its behaviour is a code smell.

#### Template helpers vs Go helpers

The `triggerLabel`, `fmtTime`, `fmtDuration` template funcs are good. Keep formatting logic in Go (testable), not in templates.

---

### 1.15 net/http Middleware Patterns

#### Chi middleware chain

The current setup uses `middleware.Recoverer` and `middleware.Logger`. The recommended order is:

```go
r.Use(middleware.RequestID)   // assign request ID first
r.Use(middleware.RealIP)      // trust X-Real-IP if behind a proxy
r.Use(securityHeaders)        // set security headers early
r.Use(middleware.Recoverer)   // recover panics
r.Use(middleware.Logger)      // log after recovery so panics are logged too
r.Use(rateLimitMiddleware)    // rate limit before any business logic
```

#### Middleware should not modify `r.Body` after read

If a middleware reads `r.Body` (e.g. for CSRF validation), it must restore it:

```go
body, _ := io.ReadAll(r.Body)
r.Body = io.NopCloser(bytes.NewReader(body))
```

#### Avoid returning early from middleware without calling `next`

Forgetting to call `next.ServeHTTP(w, r)` will silently return an empty response.

#### Context key typing

Never use a plain string as a context key — it causes collisions across packages. Use a private type:

```go
type contextKey string
const keyRequestID contextKey = "request_id"
ctx = context.WithValue(ctx, keyRequestID, id)
```

---

### 1.16 Database

#### Parameterised queries — already correct

All SQL in this codebase uses `?` placeholders. Maintain this pattern without exception.

#### Transactions for multi-statement operations

Any operation that requires atomicity (e.g. creating a run record and appending its first log entry) should use a transaction. The current `db.DB` interface may need a `BeginTx` method for this.

#### Connection pool settings

SQLite has limited concurrency. Set `SetMaxOpenConns(1)` for SQLite to prevent "database is locked" errors, or use WAL mode. For Postgres/MySQL, tune pool settings:

```go
sqlDB.SetMaxOpenConns(25)
sqlDB.SetMaxIdleConns(5)
sqlDB.SetConnMaxLifetime(5 * time.Minute)
```

#### Migrations

Use a migration tool or embed migration SQL. Never apply schema changes manually. Consider:
- `golang-migrate/migrate` — file-based up/down migrations
- `pressly/goose` — Go-first migration tool
- Embed SQL files with `//go:embed`

```go
//go:embed migrations/*.sql
var migrationFS embed.FS
```

#### Avoid `SELECT *`

Always name columns in SELECT statements. `SELECT *` breaks if columns are added or reordered, and it is impossible to audit what data is returned.

#### Avoid N+1 queries

When rendering task lists with run status, load all needed data in one or two queries rather than querying inside a loop.

---

### 1.17 Context Values

#### Only use `context.WithValue` for request-scoped, cross-cutting concerns

Acceptable: request ID, authenticated user ID, trace span.

Not acceptable: database connections, configuration, logger instances. Pass these as function parameters or struct fields.

```go
// Correct use
ctx = context.WithValue(ctx, keyRequestID, requestID)

// Anti-pattern — pass logger as parameter, not via context
ctx = context.WithValue(ctx, keyLogger, logger) // avoid
```

#### Do not pass business logic through context

The `fireAsync` function uses `context.Background()` for run goroutines. This is correct — the run should not inherit the HTTP request's cancellation. Do not pass the HTTP request context down to goroutines that outlive the request.

---

## 2. HTMX Best Practices

### 2.1 HTTP Verb Semantics

Use the correct HTTP method for each operation. HTMX attributes map directly to HTTP methods:

| Action | Attribute | Method |
|--------|-----------|--------|
| Fetch/read | `hx-get` | GET |
| Create/submit | `hx-post` | POST |
| Full update | `hx-put` | PUT |
| Partial update | `hx-patch` | PATCH |
| Delete | `hx-delete` | DELETE |

```html
<!-- Good -->
<button hx-post="/api/tasks/{{.Spec.ID}}/run" hx-swap="none">Run</button>
<button hx-delete="/api/secrets/{{.}}" hx-target="closest tr" hx-swap="outerHTML">Delete</button>

<!-- Bad — using POST for a delete -->
<button hx-post="/api/secrets/{{.}}/delete">Delete</button>
```

### 2.2 Response Format: Partial HTML Only

HTMX endpoints must return partial HTML fragments, not full pages. Never return a full `<!DOCTYPE html>` from an HTMX-targeted endpoint.

```go
// Good — server.go partial handlers return only the fragment
func (s *Server) uiTaskRows(w http.ResponseWriter, r *http.Request) {
    s.renderPartial(w, "tasks-rows", s.buildTaskRows(r.Context()))
}
```

To distinguish HTMX requests from full-page navigations, check the `HX-Request` header:

```go
func isHTMXRequest(r *http.Request) bool {
    return r.Header.Get("HX-Request") == "true"
}
```

Return a full page for direct navigation, a partial for HTMX requests.

### 2.3 hx-swap Variants

| Variant | Use case |
|---------|----------|
| `innerHTML` | Replace content inside the target element (default) |
| `outerHTML` | Replace the element itself (use when the element carries ID/attributes) |
| `beforeend` | Append to a list (infinite scroll, log streaming) |
| `afterbegin` | Prepend to a list (newest-first feeds) |
| `none` | Trigger server-side action with no DOM change (fire-and-forget) |
| `delete` | Remove the target element entirely |

Current usage in `run.html`:
```html
<!-- Correct — replacing the card itself (it carries the id) -->
hx-swap="outerHTML"
hx-select="#run-status-card"
```

The `hx-select` + `outerHTML` pattern is correct but relies on the server rendering the entire page and HTMX extracting the matching element. Consider returning just the card fragment from the endpoint to reduce server work.

### 2.4 Polling: Limit Frequency and Scope

Current polling intervals:
- Task list: `every 3s` — acceptable for a dev tool
- Run rows: `every 3s`
- Run log: `every 2s`

Recommendations:

1. **Stop polling when done**: The run log poll is conditioned on `{{if eq .Run.Status "running"}}` — this is correct. Without this guard, polling continues forever after the run finishes.

2. **Prefer SSE over polling for real-time data**: The app already has `/logs/stream` (SSE). Consider using SSE or WebSocket for run status updates instead of polling every 2–3 seconds.

3. **Use `hx-trigger="every 3s [condition]"` with a disable guard**:
```html
<tbody
  hx-get="/ui/tasks/rows"
  hx-trigger="every 3s"
  hx-swap="innerHTML"
  hx-disabled-elt="this">
```

4. **Avoid polling on hidden tabs**: Use the Page Visibility API to pause polling:
```javascript
document.addEventListener('visibilitychange', function() {
  if (document.hidden) {
    htmx.config.defaultSettleDelay = 0;
    // htmx does not pause polling automatically; you must remove/re-add triggers
  }
});
```

### 2.5 hx-push-url for Browser History

When HTMX handles navigation (full content replacement), use `hx-push-url` to keep the browser URL in sync:

```html
<!-- Navigation link that updates URL -->
<a hx-get="/tasks/{{.ID}}"
   hx-target="main"
   hx-push-url="true">{{.Name}}</a>
```

For partial updates (row refreshes, status badges), do not push URL — it would pollute browser history.

### 2.6 Security: CSP and innerHTML

**Avoid loading HTMX from unpkg in production.** The current base template loads:
```html
<script src="https://unpkg.com/htmx.org@1.9.12" crossorigin="anonymous"></script>
```

Risks:
- CDN unavailability breaks the entire UI.
- No SRI (Subresource Integrity) hash means a compromised CDN could inject malicious code.

Fix: vendor htmx, serve it from `/static/`, and add a SRI hash:

```html
<script src="/static/htmx.min.js"
        integrity="sha384-XXXX..."
        crossorigin="anonymous"></script>
```

**Do not use `hx-boost` with untrusted link targets.** `hx-boost` turns all anchor tags in a region into HTMX requests. If a page contains user-supplied URLs, they will be boosted.

**XSS via innerHTML**: HTMX's default `innerHTML` swap injects raw HTML from the server. This is safe only when the Go templates correctly escape user data (which `html/template` does by default). Never use `template.HTML()` conversions on untrusted input.

### 2.7 Out-of-Band Swaps (hx-swap-oob)

Use `hx-swap-oob` to update multiple parts of the page from a single response:

```html
<!-- In a partial response, update both the row and a counter badge -->
<tr id="run-row-{{.ID}}">...</tr>
<span id="run-count" hx-swap-oob="innerHTML">{{.TotalRuns}}</span>
```

OOB swaps require the element to have an `id`. The primary response content and OOB elements can be in the same HTML fragment.

Use OOB swaps sparingly — if you find yourself OOB-swapping 4+ elements, consider whether SSE or a full-page turbo approach is clearer.

### 2.8 Form Handling and Validation Feedback

Pattern for inline form validation:

```html
<form hx-post="/api/secrets"
      hx-target="#secret-form-feedback"
      hx-swap="innerHTML">
  <input name="key" required>
  <input type="password" name="value">
  <button type="submit">Save</button>
  <div id="secret-form-feedback"></div>
</form>
```

Server returns either a success fragment or an error fragment targeting `#secret-form-feedback`. Use HTTP 422 (Unprocessable Entity) for validation errors so HTMX handles them as content swaps rather than errors:

```go
if key == "" {
    w.WriteHeader(http.StatusUnprocessableEntity)
    fmt.Fprint(w, `<span class="error">Key is required</span>`)
    return
}
```

By default HTMX only swaps on 2xx responses. Configure error handling:
```javascript
htmx.on('htmx:responseError', function(evt) {
    console.error('HTMX error:', evt.detail.xhr.status, evt.detail.xhr.responseText);
});
```

### 2.9 SSE with hx-ext="sse"

The current log stream uses a hand-rolled `EventSource` in vanilla JavaScript. The HTMX SSE extension (`hx-ext="sse"`) provides a declarative alternative:

```html
<div hx-ext="sse" sse-connect="/logs/stream" sse-swap="message"
     hx-target="#log-console" hx-swap="beforeend">
</div>
```

The hand-rolled approach gives more control over rendering (JSON parsing, coloured output) — it is the right call here. Keep the manual EventSource for the log console. Use `hx-ext="sse"` for simpler cases like a notification banner.

### 2.10 Loading Indicators

Always provide feedback for operations that take more than ~200ms. Use `hx-indicator`:

```html
<!-- Indicator element (hidden by default, shown during request) -->
<span id="run-spinner" class="htmx-indicator">Running…</span>

<button hx-post="/api/tasks/{{.ID}}/run"
        hx-swap="none"
        hx-indicator="#run-spinner">
  ▶ Run now
</button>
```

```css
.htmx-indicator { display: none; }
.htmx-request .htmx-indicator,
.htmx-request.htmx-indicator { display: inline; }
```

### 2.11 CSRF Protection

HTMX does not add CSRF tokens automatically. For any state-changing request (POST, PUT, DELETE), implement CSRF protection:

**Option 1: Double-submit cookie pattern** (simplest for this stack)
```go
// Middleware: generate CSRF token and set cookie
func csrfMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if isMutating(r.Method) {
            cookieToken, _ := r.Cookie("csrf_token")
            headerToken := r.Header.Get("X-CSRF-Token")
            if cookieToken == nil || cookieToken.Value != headerToken {
                http.Error(w, "CSRF validation failed", http.StatusForbidden)
                return
            }
        }
        next.ServeHTTP(w, r)
    })
}
```

**Option 2: `hx-headers` with meta tag token**
```html
<meta name="csrf-token" content="{{.CSRFToken}}">
<script>
  document.body.addEventListener('htmx:configRequest', function(evt) {
    evt.detail.headers['X-CSRF-Token'] =
      document.querySelector('meta[name=csrf-token]').content;
  });
</script>
```

This app currently has no CSRF protection on mutation endpoints — this is a gap when the server is accessible from a browser.

### 2.12 Avoid Re-implementing SPA Patterns

HTMX works best with server-rendered hypermedia. Anti-patterns to avoid:

- **Avoid large client-side state**: Do not mirror server state in JavaScript variables and sync them on every HTMX response.
- **Avoid HTMX as a fetch wrapper**: If you are building complex client logic around HTMX events, consider whether you actually want a lightweight SPA framework.
- **Avoid nested hx-get trees**: Deep nesting of HTMX requests (A triggers B triggers C) creates waterfall loading and is hard to debug.
- **Prefer forms over scripted fetch**: The `setSecret()` and `deleteSecret()` functions in `secrets.html` use raw `fetch()`. They could be simplified to HTMX-powered forms:

```html
<!-- Cleaner — no JavaScript needed -->
<form hx-post="/api/secrets" hx-target="#secrets-list" hx-swap="innerHTML">
  <input name="key" ...>
  <input type="password" name="value" ...>
  <button type="submit">Save</button>
</form>
```

### 2.13 Template Partials Design

Good partial design follows these rules:

1. **One template per logical UI unit**: `tasks-rows`, `runs-rows`, `editor`, `trigger-editor` are the right granularity.
2. **Partials must be independently renderable**: A partial should be renderable without the full page context.
3. **ID anchors on the outermost element**: The target element must have a stable `id` for HTMX to find it.
4. **Name templates consistently**: The `{{define "tasks-rows"}}` name matches the Go template name passed to `renderPartial`. Keep these in sync.
5. **Partials should not embed full-page chrome**: A partial that includes `<html>` or `<body>` will break the layout when swapped in.

### 2.14 Event-Driven Patterns

HTMX emits lifecycle events that can drive coordination:

```javascript
// After a run is triggered, refresh the run list
document.body.addEventListener('htmx:afterRequest', function(evt) {
    if (evt.detail.pathInfo.requestPath.endsWith('/run')) {
        htmx.trigger('#run-list', 'refresh');
    }
});
```

Use `hx-trigger="load"` to eagerly load slow data after the page renders:
```html
<div id="run-stats"
     hx-get="/api/tasks/{{.ID}}/stats"
     hx-trigger="load">
  Loading stats…
</div>
```

---

## 3. CSS Best Practices

### 3.1 CSS Custom Properties vs Inline Styles

This codebase uses inline styles extensively throughout the templates. This creates maintenance problems: colours are duplicated, spacing is inconsistent, and theming (e.g. dark mode) is impossible without JavaScript.

**Migrate to CSS custom properties:**

```css
/* Define in :root (or [data-theme]) */
:root {
  --color-bg: #f8f9fa;
  --color-surface: #ffffff;
  --color-text: #212529;
  --color-text-muted: #6c757d;
  --color-primary: #0d6efd;
  --color-primary-hover: #0b5ed7;
  --color-header-bg: #1a1a2e;
  --color-header-text: #ffffff;
  --color-pre-bg: #1e1e2e;
  --color-pre-text: #cdd6f4;
  --color-border: #dee2e6;
  --color-badge-success-bg: #d1e7dd;
  --color-badge-success-text: #0f5132;
  --color-badge-failure-bg: #f8d7da;
  --color-badge-failure-text: #842029;

  --space-xs: 0.25rem;
  --space-sm: 0.5rem;
  --space-md: 1rem;
  --space-lg: 1.5rem;

  --radius-sm: 4px;
  --radius-md: 6px;

  --font-size-sm: 0.82rem;
  --font-size-base: 0.9rem;
}
```

Replace inline `style="background: #1a1a2e; color: #fff"` with a class that uses the custom property.

**Inline styles are acceptable for:**
- Dynamic values computed server-side (e.g. `style="width: {{.Progress}}%"`)
- One-off layout overrides during prototyping

**Inline styles are not acceptable for:**
- Repeated visual patterns (colours, spacing, typography)
- Anything that needs to change for dark mode or theming

### 3.2 Naming: BEM vs Utility-First

This codebase uses a loose mix of semantic class names (`.badge`, `.btn`, `.card`, `.meta`) with inline styles for everything else. For a small internal tool, this is pragmatic. For a growing codebase, pick one discipline and stick to it.

**BEM (Block, Element, Modifier)** — good for component-heavy UIs:
```css
.badge { ... }                    /* Block */
.badge--success { ... }           /* Modifier */
.badge--failure { ... }

.btn { ... }
.btn--sm { ... }
.btn--danger { ... }
```

**Utility-first** — good for small teams and rapid iteration. If adopting Tailwind or Twind, the inline styles become classes:
```html
<!-- Tailwind equivalent of current inline styles -->
<span class="inline-block px-2 py-1 rounded text-xs font-semibold bg-green-100 text-green-800">
  success
</span>
```

For this project's scale, a minimal BEM approach (as currently started with `.badge`, `.btn`, `.card`) is the right call. Complete it by removing all inline style colour/spacing declarations.

### 3.3 Avoiding !important Abuse

`!important` is not currently used. Keep it that way. If specificity conflicts arise, solve them by:
1. Restructuring the selector to be more specific.
2. Using `:is()` to group selectors without increasing specificity cost.
3. Using CSS layers (`@layer`) to establish explicit precedence.

```css
/* Prefer this over !important */
.card .btn--danger { background: var(--color-danger); }

/* CSS Layers — explicit precedence */
@layer base, components, utilities;
@layer utilities {
  .text-danger { color: var(--color-danger); }
}
```

### 3.4 CSS Specificity Pitfalls

Specificity order (low to high): element < class < ID < inline style

Current issues:
- `base.html` uses element selectors (`table`, `th`, `td`, `tr`) in the global style block. These low-specificity rules will conflict with any component-level styles added later.
- ID-based selectors (`#logconsole`, `#run-status-card`) have high specificity and are hard to override.

```css
/* Avoid ID selectors in stylesheets */
#logconsole { height: 200px; }  /* specificity: 1,0,0 — hard to override */

/* Prefer class selectors */
.log-console { height: 200px; } /* specificity: 0,1,0 — easy to override */
```

Use `:where()` to write zero-specificity selectors for resets:
```css
/* Zero specificity — safe to override anywhere */
:where(th, td) { padding: 0.6rem 1rem; }
```

### 3.5 Responsive Design

Currently there are no media queries or container queries. The `max-width: 1100px` on `main` provides basic centering but the layout is not truly responsive.

**Container queries** for component-level responsiveness (modern approach):

```css
.card-grid {
  container-type: inline-size;
  container-name: card-grid;
}

@container card-grid (width < 600px) {
  .card { flex-direction: column; }
}
```

**Fluid typography** instead of fixed font sizes:
```css
/* Clamp: min 0.8rem, ideal 2vw, max 1rem */
body { font-size: clamp(0.8rem, 2vw, 1rem); }
```

**Responsive table pattern** (current tables will overflow on mobile):
```css
@media (max-width: 640px) {
  table, thead, tbody, th, td, tr { display: block; }
  thead tr { display: none; } /* hide headers */
  td::before {
    content: attr(data-label);
    font-weight: 600;
    display: inline-block;
    width: 8rem;
  }
}
```

### 3.6 Dark Mode Implementation

No dark mode is currently implemented. The recommended approach uses `prefers-color-scheme` with a `data-theme` override for user preference persistence:

```css
/* Light mode defaults already in :root */
:root { --color-bg: #f8f9fa; ... }

/* Dark mode via media query */
@media (prefers-color-scheme: dark) {
  :root {
    --color-bg: #0d1117;
    --color-surface: #161b22;
    --color-text: #e6edf3;
    --color-text-muted: #8b949e;
    --color-border: #30363d;
  }
}

/* Manual override via data attribute (for a theme toggle button) */
[data-theme="dark"] {
  --color-bg: #0d1117;
  /* ... */
}
[data-theme="light"] {
  --color-bg: #f8f9fa;
  /* ... */
}
```

JavaScript toggle:
```javascript
function toggleTheme() {
  const current = document.documentElement.dataset.theme;
  document.documentElement.dataset.theme = current === 'dark' ? 'light' : 'dark';
  localStorage.setItem('theme', document.documentElement.dataset.theme);
}
// Restore on load (avoids flash)
document.documentElement.dataset.theme = localStorage.getItem('theme') || 'auto';
```

### 3.7 Performance: contain, content-visibility, will-change

**`content-visibility: auto`** for off-screen content (log console, large tables):
```css
.log-console {
  content-visibility: auto;
  contain-intrinsic-size: 0 200px; /* hint for layout estimation */
}
```

**`will-change`** only for elements that are about to animate:
```css
/* Apply BEFORE animation starts, remove AFTER */
.loading-spinner { will-change: transform; }
/* Do NOT apply globally — wastes GPU memory */
* { will-change: transform; } /* never do this */
```

**`contain`** for isolated components that do not affect layout of siblings:
```css
.card { contain: layout paint; }
```

### 3.8 Accessibility

**Focus styles**: The current stylesheet has no focus styles. Browsers provide default focus outlines but many CSS resets remove them. Always define visible focus styles:

```css
:focus-visible {
  outline: 2px solid var(--color-primary);
  outline-offset: 2px;
}

/* Remove focus ring for mouse users only */
:focus:not(:focus-visible) { outline: none; }
```

**Contrast ratios** (WCAG AA minimum: 4.5:1 for normal text, 3:1 for large text):
- `.meta` text (`#6c757d` on `#f8f9fa`): ratio ≈ 4.5:1 — passes AA minimum.
- `nav a` text (`#ccc` on `#1a1a2e`): ratio ≈ 5.3:1 — passes.
- `.badge-running` text (`#664d03` on `#fff3cd`): ratio ≈ 4.7:1 — passes AA.

Verify with: https://webaim.org/resources/contrastchecker/

**Reduced motion**:
```css
@media (prefers-reduced-motion: reduce) {
  *, *::before, *::after {
    animation-duration: 0.01ms !important;
    animation-iteration-count: 1 !important;
    transition-duration: 0.01ms !important;
  }
}
```

**ARIA attributes** for dynamic HTMX regions:
```html
<!-- Announce live region updates to screen readers -->
<tbody aria-live="polite" aria-atomic="false"
       hx-get="/ui/tasks/rows" hx-trigger="every 3s" hx-swap="innerHTML">
```

**Semantic HTML**: Use `<button>` for actions and `<a>` for navigation. Do not use `<div onclick>`.

### 3.9 Avoid Inline Styles in HTML

The templates contain many inline styles. Migration path:

1. Extract all colour values to CSS custom properties.
2. Create utility classes for common patterns (`display:flex`, `gap`, `margin-left:auto`).
3. Move the `<style>` block in `base.html` to an external `static/styles.css` file.
4. Enable the browser to cache the stylesheet across page navigations.

```html
<!-- Instead of -->
<div style="display:flex;gap:1rem;align-items:center">

<!-- Use a utility class -->
<div class="flex items-center gap-md">
```

### 3.10 Animation Best Practices

**Always animate `transform` and `opacity` — never `top`/`left`/`width`/`height`**:

```css
/* Good — GPU composited, no layout recalculation */
.spinner { animation: spin 1s linear infinite; }
@keyframes spin { to { transform: rotate(360deg); } }

/* Bad — causes layout reflow on every frame */
.slide-in { animation: slide 0.3s ease; }
@keyframes slide { from { left: -100px; } to { left: 0; } }

/* Good — use transform instead */
@keyframes slide { from { transform: translateX(-100px); } to { transform: translateX(0); } }
```

**Duration guidelines**:
- Micro-interactions (button hover): 100–150ms
- Element appear/disappear: 200–300ms
- Page transitions: 300–500ms
- Anything over 500ms feels slow

**HTMX transition classes** — HTMX adds `.htmx-settling` and `.htmx-swapping` classes during swaps:
```css
.htmx-swapping { opacity: 0; transition: opacity 0.15s ease-out; }
.htmx-settling { opacity: 1; }
```

### 3.11 CSS Grid vs Flexbox

**Use Flexbox for:**
- One-dimensional layouts (rows OR columns)
- Navigation bars, button groups, form controls
- Centering a single item

**Use Grid for:**
- Two-dimensional layouts (rows AND columns simultaneously)
- Page-level layout (header, sidebar, main, footer)
- Card grids that need consistent row heights

```css
/* Page layout — Grid */
body {
  display: grid;
  grid-template-rows: auto 1fr auto; /* header, main, footer */
  min-height: 100vh;
}

/* Nav bar — Flexbox */
header { display: flex; align-items: center; gap: 1rem; }

/* Card grid — Grid */
.task-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
  gap: 1rem;
}
```

### 3.12 Modern Pseudo-Classes: :is(), :where(), :has()

**`:is()` — group selectors, takes specificity of most specific argument:**
```css
/* Instead of */
h1 a, h2 a, h3 a { color: inherit; }

/* Use */
:is(h1, h2, h3) a { color: inherit; }
```

**`:where()` — zero specificity, safe for base styles:**
```css
/* Reset that will never override component styles */
:where(button, [role="button"]) { cursor: pointer; }
```

**`:has()` — parent selector (now widely supported):**
```css
/* Style a card that contains a running badge */
.card:has(.badge--running) {
  border-left: 3px solid var(--color-warning);
}

/* Style a table row that has a failure badge */
tr:has(.badge-failure) { background: #fff5f5; }
```

### 3.13 Logical Properties for i18n-Safe Layouts

Replace physical properties with logical equivalents for correct RTL/LTR behaviour:

```css
/* Physical (breaks RTL) */
.card { margin-left: 1rem; padding-right: 0.5rem; }

/* Logical (works in all writing modes) */
.card { margin-inline-start: 1rem; padding-inline-end: 0.5rem; }

/* Shorthand logical properties */
.card {
  margin-inline: auto;     /* margin-left + margin-right */
  padding-block: 1rem;     /* padding-top + padding-bottom */
  padding-inline: 1.25rem; /* padding-left + padding-right */
  border-inline-start: 2px solid var(--color-primary); /* left border in LTR */
}
```

This app is likely English-only, but adopting logical properties now costs nothing and prevents future rework.

### 3.14 Font Loading Strategies

No custom fonts are currently loaded — `system-ui, sans-serif` is used, which is the optimal choice for a developer tool. No font loading strategy is needed.

If custom fonts are added in the future:

```css
@font-face {
  font-family: 'Inter';
  src: url('/static/inter.woff2') format('woff2');
  font-display: swap;    /* show fallback font immediately, swap when loaded */
  font-weight: 400 700;  /* variable font range */
}
```

`font-display` values:
- `swap` — show fallback instantly, swap when loaded (good for body text)
- `optional` — use fallback if font loads slowly, no swap (best for performance)
- `block` — invisible text until font loads (bad, avoid)
- `fallback` — 100ms block then swap (compromise)

Preload critical fonts:
```html
<link rel="preload" href="/static/inter.woff2" as="font" type="font/woff2" crossorigin>
```

---

## Quick Reference: Common Issues in This Codebase

| Area | Issue | Severity | Reference |
|------|-------|----------|-----------|
| Go | No HTTP server timeouts | High | §1.3 |
| Go | `AIConfig.APIKey` exposed in `/api/config` | High | §1.2, §1.8 |
| Go | No CSRF protection on mutation endpoints | High | §2.11 |
| Go | No rate limiting on sensitive endpoints | Medium | §1.3 |
| Go | HTMX loaded from CDN without SRI hash | Medium | §2.6 |
| Go | Session cookie missing `Secure` flag | Medium | §1.3 |
| Go | `context.Context` stored in `Reconciler` struct | Low | §1.4 |
| Go | Test polling loop uses `time.Sleep` | Low | §1.6 |
| HTMX | Polling continues on all views (no visibility guard) | Low | §2.4 |
| HTMX | Secrets page uses raw `fetch()` instead of HTMX forms | Low | §2.12 |
| CSS | All visual styles are inline — no custom properties | Medium | §3.1, §3.9 |
| CSS | No focus styles defined | Medium | §3.8 |
| CSS | No dark mode support | Low | §3.6 |
| CSS | No responsive breakpoints for small screens | Low | §3.5 |
| CSS | Tables will overflow on mobile | Low | §3.5 |
