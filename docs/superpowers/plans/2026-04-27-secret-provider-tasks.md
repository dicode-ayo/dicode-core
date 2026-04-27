# Task-Based Secret Providers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Resolve external secrets (Doppler / 1Password / Vault) by spawning a buildin "provider task" referenced from `EnvEntry.From` as `task:<provider-id>`, with TTL caching, batched per-launch spawn, and typed failure plumbing.

**Architecture:** Extend `EnvEntry.From` with an `env:` / `task:` prefix grammar; add a shared env-resolver package (`pkg/runtime/envresolve`) that both Deno and Python runtimes call before spawning the consumer process. Provider tasks are normal Deno tasks that call `dicode.output(map, { secret: true })` — daemon-side IPC handler routes the map back to the resolver, feeds it to the run-log redactor, and writes `[redacted]` placeholders to the run log. Provider results are cached in-memory keyed by `(provider-task-id, secret-name)` with TTL declared in the provider's `task.yaml`. The legacy `secrets.Chain` (`secret:` lookups) coexists unchanged.

**Tech Stack:** Go 1.22+ (daemon), TypeScript (Deno SDK shim), Python 3 (Python SDK), YAML (task.yaml schema), SQLite (run-log only — cache is in-memory).

---

## File Structure

### New files

| Path | Responsibility |
|---|---|
| `pkg/task/envparse.go` | `parseFrom(s string) (FromKind, string)` — splits `env:` / `task:` / bare prefix; pure function; no I/O. |
| `pkg/task/envparse_test.go` | Table-driven tests for `parseFrom`. |
| `pkg/runtime/envresolve/resolver.go` | Shared env-resolution stage used by both Deno and Python runtimes. Walks `permissions.env`, groups `task:` entries by provider, resolves via cache + provider spawn, falls back to legacy paths for `env:`/bare/`secret:`. |
| `pkg/runtime/envresolve/resolver_test.go` | Table-driven unit tests with a mocked `ProviderRunner`. |
| `pkg/runtime/envresolve/cache.go` | TTL cache: `(providerID, secretName) → {value, expiresAt}` with content-hash invalidation. |
| `pkg/runtime/envresolve/cache_test.go` | Unit tests for cache hit/miss, TTL expiry, content-hash bust. |
| `pkg/runtime/envresolve/errors.go` | Typed errors: `ErrProviderUnavailable`, `ErrRequiredSecretMissing`, `ErrProviderMisconfigured`. |
| `tasks/buildin/secret-providers/doppler/task.yaml` | Reference Doppler provider; declares `provider.cache_ttl: 5m` and `secret: DOPPLER_TOKEN`. |
| `tasks/buildin/secret-providers/doppler/task.ts` | Deno task body calling Doppler REST API. |
| `tasks/buildin/secret-providers/doppler/task.test.ts` | Deno test using mocked `fetch`. |

### Modified files

| Path | Why |
|---|---|
| `pkg/task/spec.go:184-191` | `EnvEntry` documentation update — `From` accepts `env:` / `task:` prefix. |
| `pkg/task/spec.go:280-303` | Add optional `Provider` block on `Spec` (`cache_ttl`). |
| `pkg/task/spec.go:395-456` (validate) | Validate provider task content shape if `Provider` field present. |
| `pkg/registry/reconciler.go:125-164` | Reject task whose `permissions.env[].From` is `task:<id>` referencing an unregistered task. |
| `pkg/runtime/deno/runtime.go:177-214` | Replace inline env-resolver with call into `envresolve.Resolve`. |
| `pkg/runtime/python/runtime.go:161-196` | Same replacement. |
| `pkg/runtime/deno/sdk/shim.ts:241-246` | Extend `dicode.output` API surface — overload accepting `(map, { secret: true })`. |
| `pkg/runtime/python/sdk/dicode_sdk.py:259-284` | Mirror Deno secret-output overload. |
| `pkg/ipc/message.go:52-55` | Add optional `Secret bool` and `secretMap json.RawMessage` fields to `Request`. |
| `pkg/ipc/server.go:399-413` | Handle `output` with `secret: true` flag — validate flat map, route via callback, persist redacted log entry. |
| `pkg/ipc/capability.go` | Add `CapOutputSecret` capability granted by default to every task token. |
| `pkg/registry/registry.go:28-42` | Add `FailureReason` field to `Run`; add migration to insert `fail_reason` column. |
| `pkg/registry/registry.go:132-139` | Add `FinishRunWithReason(ctx, runID, status, reason)`. |
| `pkg/db/sqlite.go:110-118` | New `ALTER TABLE runs ADD COLUMN fail_reason TEXT NOT NULL DEFAULT ''`. |
| `pkg/trigger/engine.go:1175-1230` | When env-resolver returns `ErrProviderUnavailable` / `ErrRequiredSecretMissing`, mark consumer run failed with typed reason BEFORE dispatch, and trigger `FireChain` for the consumer. |
| `pkg/trigger/engine.go:636-679` | `FireChain` already covers consumer chain; provider task chain fires through the existing post-run path — no change needed beyond what its own run produces, but add doc comment cross-reference. |
| `tasks/buildin/taskset.yaml` | Register `secret-providers/doppler` so the reconciler picks it up. |
| `docs/concepts/secrets.md` | Add a "Provider tasks" section with one full `from: task:doppler` working example. |

---

## Task Map Overview

1. **Schema parser** for `From` prefix grammar (Tasks 1–3).
2. **Reconciler validation** of `task:<id>` references (Task 4).
3. **Provider TTL cache** package (Tasks 5–7).
4. **Resolver core** with typed errors (Tasks 8–11).
5. **IPC `secret: true` flag** wiring (Tasks 12–15).
6. **SDK extensions** Deno + Python (Tasks 16–19).
7. **Wire resolver** into Deno + Python runtimes (Tasks 20–22).
8. **Failure plumbing** typed reasons + chain (Tasks 23–25).
9. **Doppler reference provider** (Tasks 26–28).
10. **Docs** (Task 29).

---

### Task 1: Add FromKind enum and parseFrom skeleton

**Files:**
- Create: `pkg/task/envparse.go`

- [ ] **Step 1: Create the new file with the type and stub**

```go
// Package task — env-from prefix grammar.
//
// EnvEntry.From historically held a bare host-env-var name. Issue #119
// introduces an optional prefix:
//
//	env:NAME       — host OS environment variable
//	task:PROVIDER  — resolve via provider task PROVIDER
//	NAME           — bare; treated as env:NAME for backwards compatibility
package task

import "strings"

// FromKind is the discriminator for an EnvEntry.From value.
type FromKind int

const (
	// FromKindEnv resolves via os.Getenv. Default for bare values.
	FromKindEnv FromKind = iota
	// FromKindTask resolves by spawning a provider task whose ID is the
	// returned target.
	FromKindTask
)

// parseFrom splits an EnvEntry.From string into (kind, target). Whitespace
// is trimmed. An empty string yields (FromKindEnv, "") so callers can
// detect the no-from case.
//
// Grammar:
//
//	"env:NAME"        → (FromKindEnv,  "NAME")
//	"task:PROVIDER"   → (FromKindTask, "PROVIDER")
//	"NAME"            → (FromKindEnv,  "NAME")  // bare = env (legacy)
//	""                → (FromKindEnv,  "")
//
// Unknown prefixes (e.g. "foo:bar") are treated as bare names so existing
// task.yaml files containing colons in env var names continue to load.
// The reconciler is responsible for catching truly malformed names.
func parseFrom(s string) (FromKind, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return FromKindEnv, ""
	}
	if rest, ok := strings.CutPrefix(s, "task:"); ok {
		return FromKindTask, strings.TrimSpace(rest)
	}
	if rest, ok := strings.CutPrefix(s, "env:"); ok {
		return FromKindEnv, strings.TrimSpace(rest)
	}
	return FromKindEnv, s
}
```

- [ ] **Step 2: Commit**

```bash
git add pkg/task/envparse.go
git commit -m "feat(task): add parseFrom prefix grammar (env:/task:/bare)"
```

---

### Task 2: Write table-driven test for parseFrom

**Files:**
- Create: `pkg/task/envparse_test.go`

- [ ] **Step 1: Write the failing test**

```go
package task

import "testing"

func TestParseFrom(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantKind FromKind
		wantTgt  string
	}{
		{"empty", "", FromKindEnv, ""},
		{"bare name", "FOO", FromKindEnv, "FOO"},
		{"explicit env", "env:FOO", FromKindEnv, "FOO"},
		{"task prefix", "task:doppler", FromKindTask, "doppler"},
		{"task prefix with hyphen", "task:secret-providers/doppler", FromKindTask, "secret-providers/doppler"},
		{"trim whitespace bare", "  FOO  ", FromKindEnv, "FOO"},
		{"trim whitespace prefix", "  task:doppler  ", FromKindTask, "doppler"},
		{"unknown prefix → bare", "foo:bar", FromKindEnv, "foo:bar"},
		{"empty env target", "env:", FromKindEnv, ""},
		{"empty task target", "task:", FromKindTask, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotKind, gotTgt := parseFrom(tt.in)
			if gotKind != tt.wantKind || gotTgt != tt.wantTgt {
				t.Errorf("parseFrom(%q) = (%d, %q), want (%d, %q)",
					tt.in, gotKind, gotTgt, tt.wantKind, tt.wantTgt)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it passes**

Run: `go test ./pkg/task/... -run TestParseFrom -v`

Expected:
```
=== RUN   TestParseFrom
=== RUN   TestParseFrom/empty
=== RUN   TestParseFrom/bare_name
... (all subtests)
--- PASS: TestParseFrom (0.00s)
PASS
```

- [ ] **Step 3: Commit**

```bash
git add pkg/task/envparse_test.go
git commit -m "test(task): table-driven coverage for parseFrom"
```

---

### Task 3: Update EnvEntry doc comment to mention prefix grammar

**Files:**
- Modify: `pkg/task/spec.go:166-191`

- [ ] **Step 1: Replace doc comment**

Replace the existing `EnvEntry` block godoc (currently at `pkg/task/spec.go:166-191`) with the version below. Keep the struct field tags exactly as-is — only the doc comment changes.

```go
// EnvEntry declares one environment variable the task is allowed to access.
// Supports five forms in YAML:
//
//   - HOME                          # bare name: allowlist $HOME from host env, same name
//   - name: API_KEY                 # rename from host env: read $GH_TOKEN, expose as API_KEY
//     from: GH_TOKEN
//   - name: TOKEN                   # explicit env prefix (equivalent to bare)
//     from: env:GH_TOKEN
//   - name: PG_URL                  # provider-task lookup: spawn task "doppler" to resolve PG_URL
//     from: task:doppler
//   - name: DB_PASS                 # secret injection: resolve "db_password" from secrets store
//     secret: db_password
//   - name: LOG_LEVEL               # literal value (used by taskset overrides)
//     value: "info"
//
// Lookup rules:
//   - secret:        → secrets store only; run fails if key not found
//   - from: env:NAME → host OS environment only (os.Getenv); injected as entry.Name
//   - from: task:ID  → provider task ID; resolver spawns ID once per consumer
//                      launch (batched across all task: entries with the same ID)
//   - from: bare     → identical to from: env:bare (backwards compat)
//   - bare entry     → allowlisted in --allow-env; script reads it from host env at runtime
//
// The optional `if_missing:` directive (only meaningful alongside `secret:`)
// runs a prereq task when the secret is absent. See the IfMissing type.
type EnvEntry struct {
```

- [ ] **Step 2: Run existing tests to confirm no regression**

Run: `go test ./pkg/task/... -timeout 60s`

Expected: `ok  github.com/dicode/dicode/pkg/task ...`

- [ ] **Step 3: Commit**

```bash
git add pkg/task/spec.go
git commit -m "docs(task): document EnvEntry from-prefix grammar"
```

---

### Task 4: Reconciler rejects task: references to unknown providers

**Files:**
- Modify: `pkg/registry/reconciler.go:125-164`
- Test: `pkg/registry/reconciler_test.go`

- [ ] **Step 1: Write the failing test**

Append to `pkg/registry/reconciler_test.go`:

```go
func TestReconciler_RejectsUnknownTaskProvider(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	tmpDB := newTestDB(t)
	reg := New(tmpDB)

	// Build a consumer spec referencing an unregistered provider.
	consumer := &task.Spec{
		ID:   "consumer",
		Name: "consumer",
		Permissions: task.Permissions{
			Env: []task.EnvEntry{{Name: "PG_URL", From: "task:nonexistent-provider"}},
		},
		Trigger: task.TriggerConfig{Manual: true},
	}

	rc := NewReconciler(reg, nil, zap.NewNop())
	rc.runCtx = ctx
	rc.merged = make(chan source.Event, 1)

	// Inject the event directly (bypassing source.Source for unit-level coverage).
	rc.handle(source.Event{
		Kind:    source.EventAdded,
		TaskID:  "consumer",
		Spec:    consumer,
		Source:  "test",
		TaskDir: "",
	})

	if _, ok := reg.Get("consumer"); ok {
		t.Fatalf("consumer with unknown task: provider should NOT have been registered")
	}
}
```

(`newTestDB` already exists in `pkg/registry/reconciler_test.go`; reuse it.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/registry/... -run TestReconciler_RejectsUnknownTaskProvider -v`

Expected: FAIL — consumer was registered because reconciler does no provider validation.

- [ ] **Step 3: Add the validation hook in `reconciler.go`**

Replace the body of `handle` at `pkg/registry/reconciler.go:125-164` with the version below — adds a call to `validateTaskProviders` between `LoadDirWithVars` and `Register`.

```go
func (rc *Reconciler) handle(ev source.Event) {
	switch ev.Kind {
	case source.EventAdded, source.EventUpdated:
		var spec *task.Spec
		if ev.Spec != nil {
			spec = ev.Spec
			spec.ID = ev.TaskID
		} else {
			var err error
			spec, err = task.LoadDirWithVars(ev.TaskDir, ev.ExtraVars)
			if err != nil {
				rc.log.Warn("failed to load task",
					zap.String("task", ev.TaskID),
					zap.String("source", ev.Source),
					zap.Error(err),
				)
				return
			}
		}
		if err := rc.validateTaskProviders(spec); err != nil {
			rc.log.Warn("task references unknown provider",
				zap.String("task", ev.TaskID),
				zap.String("source", ev.Source),
				zap.Error(err),
			)
			return
		}
		if err := rc.registry.Register(spec); err != nil {
			rc.log.Error("failed to register task", zap.String("task", ev.TaskID), zap.Error(err))
			return
		}
		rc.log.Info("task registered",
			zap.String("task", ev.TaskID),
			zap.String("kind", string(ev.Kind)),
		)
		if rc.OnRegister != nil {
			rc.OnRegister(spec)
		}

	case source.EventRemoved:
		rc.registry.Unregister(ev.TaskID)
		rc.log.Info("task unregistered", zap.String("task", ev.TaskID))
		if rc.OnUnregister != nil {
			rc.OnUnregister(ev.TaskID)
		}
	}
}

// validateTaskProviders inspects every EnvEntry whose From has the
// "task:" prefix and confirms the referenced provider task is already
// registered. Issue #119: a misspelled provider must not silently fall
// through to a runtime spawn failure on every consumer launch.
//
// Order dependency: provider tasks must reconcile before their consumers.
// The buildin source registers providers first because they live under
// tasks/buildin/secret-providers/* and the taskset.yaml entry order is
// preserved. For multi-source setups, a transient miss on first
// reconciler pass causes the consumer to be skipped; the next sync (30s
// later, or on the source's next event) retries.
func (rc *Reconciler) validateTaskProviders(spec *task.Spec) error {
	for _, e := range spec.Permissions.Env {
		kind, target := parseFromForReconciler(e.From)
		if kind != fromKindTaskRC {
			continue
		}
		if target == "" {
			return fmt.Errorf("env entry %q: from: task: target is empty", e.Name)
		}
		if _, ok := rc.registry.Get(target); !ok {
			return fmt.Errorf("env entry %q: provider task %q not registered", e.Name, target)
		}
	}
	return nil
}
```

(Note: we cannot import `pkg/task` private symbols. Add the helper alias inline.)

- [ ] **Step 4: Add a tiny exported parser entry on the task package**

Add to `pkg/task/envparse.go` (append at end of file):

```go
// ParseFrom is the exported counterpart of parseFrom for callers outside
// pkg/task (e.g. the reconciler validates from: task:<id> references).
func ParseFrom(s string) (FromKind, string) { return parseFrom(s) }
```

…and update the reconciler helper to call it instead of `parseFromForReconciler`/`fromKindTaskRC`. Replace the `validateTaskProviders` body with:

```go
func (rc *Reconciler) validateTaskProviders(spec *task.Spec) error {
	for _, e := range spec.Permissions.Env {
		kind, target := task.ParseFrom(e.From)
		if kind != task.FromKindTask {
			continue
		}
		if target == "" {
			return fmt.Errorf("env entry %q: from: task: target is empty", e.Name)
		}
		if _, ok := rc.registry.Get(target); !ok {
			return fmt.Errorf("env entry %q: provider task %q not registered", e.Name, target)
		}
	}
	return nil
}
```

- [ ] **Step 5: Run tests to confirm pass**

Run: `go test ./pkg/registry/... ./pkg/task/... -timeout 60s`

Expected: all PASS, including `TestReconciler_RejectsUnknownTaskProvider`.

- [ ] **Step 6: Commit**

```bash
git add pkg/task/envparse.go pkg/registry/reconciler.go pkg/registry/reconciler_test.go
git commit -m "feat(reconciler): reject from: task:<id> references to unknown tasks"
```

---

### Task 5: Provider TTL cache — write failing test first

**Files:**
- Create: `pkg/runtime/envresolve/cache_test.go`

- [ ] **Step 1: Write the failing test**

```go
package envresolve

import (
	"testing"
	"time"
)

func TestCache_HitMissTTL(t *testing.T) {
	now := time.Now()
	c := newCache()

	// Miss on empty cache.
	if _, ok := c.get("doppler", "PG_URL", "hashA", now); ok {
		t.Fatalf("expected miss on empty cache")
	}

	// Put + hit within TTL.
	c.put("doppler", "PG_URL", "hashA", "postgres://x", 5*time.Second, now)
	if v, ok := c.get("doppler", "PG_URL", "hashA", now.Add(2*time.Second)); !ok || v != "postgres://x" {
		t.Fatalf("expected hit, got (%q, %v)", v, ok)
	}

	// Expire after TTL.
	if _, ok := c.get("doppler", "PG_URL", "hashA", now.Add(6*time.Second)); ok {
		t.Fatalf("expected expiry after TTL")
	}
}

func TestCache_BustOnHashChange(t *testing.T) {
	now := time.Now()
	c := newCache()
	c.put("doppler", "PG_URL", "hashA", "v1", time.Minute, now)

	// Same provider, different hash → miss (and old entry purged).
	if _, ok := c.get("doppler", "PG_URL", "hashB", now); ok {
		t.Fatalf("expected miss after content-hash change")
	}
	// Original key (hashA) also gone.
	if _, ok := c.get("doppler", "PG_URL", "hashA", now); ok {
		t.Fatalf("expected old hash entries purged")
	}
}

func TestCache_TTLZeroDisablesCaching(t *testing.T) {
	now := time.Now()
	c := newCache()
	c.put("doppler", "PG_URL", "hashA", "v1", 0, now)
	if _, ok := c.get("doppler", "PG_URL", "hashA", now); ok {
		t.Fatalf("ttl=0 must not cache")
	}
}
```

- [ ] **Step 2: Run test to verify it fails (package missing)**

Run: `go test ./pkg/runtime/envresolve/... -v`

Expected: `no Go files in .../envresolve` — the package does not exist yet.

- [ ] **Step 3: Commit (test only)**

```bash
git add pkg/runtime/envresolve/cache_test.go
git commit -m "test(envresolve): TTL cache contract tests"
```

---

### Task 6: Implement the provider TTL cache

**Files:**
- Create: `pkg/runtime/envresolve/cache.go`

- [ ] **Step 1: Implement**

```go
// Package envresolve resolves task env entries by walking permissions.env
// and dispatching to host env, secrets store, or provider tasks. Used by
// the Deno and Python runtimes before spawning the consumer process.
package envresolve

import (
	"sync"
	"time"
)

// cacheKey indexes a single resolved secret value.
type cacheKey struct {
	providerID string
	secretName string
}

type cacheEntry struct {
	value       string
	providerHash string
	expiresAt   time.Time
}

// cache is an in-memory TTL store for provider-task results. Not persisted:
// daemon restart re-fetches everything (issue #119: not worth the encryption
// complexity for cached upstream values).
//
// Concurrent access is guarded by a single RWMutex — provider hits are rare
// (per consumer launch) compared to in-process IPC, so contention is fine.
type cache struct {
	mu sync.RWMutex
	m  map[cacheKey]cacheEntry
}

func newCache() *cache {
	return &cache{m: make(map[cacheKey]cacheEntry)}
}

// get returns the cached value if (a) the entry exists, (b) the provider's
// content hash matches the stored one (otherwise the entry is purged), and
// (c) the TTL has not expired. now is the caller-supplied wall-clock so
// tests can drive the timeline deterministically.
func (c *cache) get(providerID, secretName, providerHash string, now time.Time) (string, bool) {
	k := cacheKey{providerID, secretName}

	c.mu.RLock()
	e, ok := c.m[k]
	c.mu.RUnlock()
	if !ok {
		return "", false
	}
	if e.providerHash != providerHash {
		// Content changed — purge ALL entries for this provider, not just
		// the one we just looked at. A new task hash means the operator
		// edited the provider task; old cached values from the previous
		// version may have been written under a different upstream policy.
		c.bustProvider(providerID)
		return "", false
	}
	if !now.Before(e.expiresAt) {
		return "", false
	}
	return e.value, true
}

// put writes a value with a TTL. ttl=0 is a no-op so providers can declare
// "never cache" by omitting cache_ttl from their task.yaml.
func (c *cache) put(providerID, secretName, providerHash, value string, ttl time.Duration, now time.Time) {
	if ttl <= 0 {
		return
	}
	k := cacheKey{providerID, secretName}
	c.mu.Lock()
	c.m[k] = cacheEntry{value: value, providerHash: providerHash, expiresAt: now.Add(ttl)}
	c.mu.Unlock()
}

// bustProvider drops every cached entry for providerID. Called on
// content-hash mismatch (see get) and exposed for the reconciler to call
// on EventUpdated/EventRemoved if needed in a follow-up.
func (c *cache) bustProvider(providerID string) {
	c.mu.Lock()
	for k := range c.m {
		if k.providerID == providerID {
			delete(c.m, k)
		}
	}
	c.mu.Unlock()
}
```

- [ ] **Step 2: Run cache tests**

Run: `go test ./pkg/runtime/envresolve/... -run TestCache -v`

Expected:
```
--- PASS: TestCache_HitMissTTL
--- PASS: TestCache_BustOnHashChange
--- PASS: TestCache_TTLZeroDisablesCaching
PASS
```

- [ ] **Step 3: Commit**

```bash
git add pkg/runtime/envresolve/cache.go
git commit -m "feat(envresolve): in-memory TTL cache with content-hash invalidation"
```

---

### Task 7: Add Provider config block to task.Spec

**Files:**
- Modify: `pkg/task/spec.go:280-303`
- Test: `pkg/task/spec.go` parsing path (existing tests cover round-trip).

- [ ] **Step 1: Add the type**

Insert just above the `Spec` struct in `pkg/task/spec.go`:

```go
// ProviderConfig declares secret-provider settings on a task that
// implements the issue #119 provider contract (calls dicode.output(map,
// { secret: true }) with a flat Record<string,string>).
//
// CacheTTL controls how long resolved values are cached. Zero (the
// default) disables caching. Cache key is (provider-task-id,
// secret-name); entries are busted when the task content hash changes.
type ProviderConfig struct {
	CacheTTL time.Duration `yaml:"cache_ttl,omitempty" json:"cache_ttl,omitempty"`
}
```

Then add a field to `Spec` between `OnFailureChain` and `TaskDir`:

```go
	// Provider declares this task as a secret provider implementing the
	// issue #119 contract. nil = not a provider; non-nil = provider with
	// the given config. The reconciler uses this to gate cache_ttl
	// validation; the resolver uses it to look up the TTL.
	Provider *ProviderConfig `yaml:"provider,omitempty" json:"provider,omitempty"`
```

- [ ] **Step 2: Add a unit test confirming round-trip**

Append to `pkg/task/spec_test.go` (create the file if it doesn't exist; check `ls pkg/task/`):

```go
package task

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestSpec_ProviderBlockRoundTrip(t *testing.T) {
	src := strings.TrimSpace(`
name: doppler
runtime: deno
trigger:
  manual: true
provider:
  cache_ttl: 5m
`)
	var s Spec
	if err := yaml.NewDecoder(strings.NewReader(src)).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.Provider == nil {
		t.Fatalf("Provider was nil")
	}
	if s.Provider.CacheTTL != 5*time.Minute {
		t.Fatalf("CacheTTL = %v, want 5m", s.Provider.CacheTTL)
	}
}
```

- [ ] **Step 3: Run tests**

Run: `go test ./pkg/task/... -timeout 60s`

Expected: all pass including the new round-trip test.

- [ ] **Step 4: Commit**

```bash
git add pkg/task/spec.go pkg/task/spec_test.go
git commit -m "feat(task): add Spec.Provider config block (cache_ttl)"
```

---

### Task 8: Define typed resolver errors

**Files:**
- Create: `pkg/runtime/envresolve/errors.go`

- [ ] **Step 1: Implement**

```go
package envresolve

import "fmt"

// ErrProviderUnavailable is returned when a provider task spawn / IPC /
// timeout / non-zero exit prevents the resolver from collecting the map.
// The trigger engine renders this as a typed failure reason
// "provider_unavailable: <providerID>".
type ErrProviderUnavailable struct {
	ProviderID string
	Cause      error
}

func (e *ErrProviderUnavailable) Error() string {
	return fmt.Sprintf("provider %q unavailable: %v", e.ProviderID, e.Cause)
}

func (e *ErrProviderUnavailable) Unwrap() error { return e.Cause }

// ErrRequiredSecretMissing is returned when a provider task returned its
// secret map but a non-optional key the consumer requested is absent.
// The trigger engine renders this as a typed failure reason
// "required_secret_missing: <Key> from <ProviderID>".
type ErrRequiredSecretMissing struct {
	ProviderID string
	Key        string
}

func (e *ErrRequiredSecretMissing) Error() string {
	return fmt.Sprintf("required secret %q missing from provider %q", e.Key, e.ProviderID)
}

// ErrProviderMisconfigured is returned when a task referenced via
// from: task:<id> exists but did not call dicode.output(map, { secret:
// true }) — i.e. it is not actually a provider. Surfaced as a startup
// validation hint to the operator.
type ErrProviderMisconfigured struct {
	ProviderID string
	Reason     string
}

func (e *ErrProviderMisconfigured) Error() string {
	return fmt.Sprintf("provider %q misconfigured: %s", e.ProviderID, e.Reason)
}
```

- [ ] **Step 2: Sanity-compile**

Run: `go build ./pkg/runtime/envresolve/...`

Expected: no output (success).

- [ ] **Step 3: Commit**

```bash
git add pkg/runtime/envresolve/errors.go
git commit -m "feat(envresolve): typed errors (provider_unavailable, required_secret_missing)"
```

---

### Task 9: Define ProviderRunner interface and Resolve signature

**Files:**
- Create: `pkg/runtime/envresolve/resolver.go`

- [ ] **Step 1: Write the skeleton**

```go
package envresolve

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
)

// ProviderRequest is one entry the consumer needs from a provider.
type ProviderRequest struct {
	Name     string `json:"name"`
	Optional bool   `json:"optional"`
}

// ProviderResult is the map a provider task returned via
// dicode.output(map, { secret: true }).
type ProviderResult struct {
	Values map[string]string
}

// ProviderRunner is the dependency through which the resolver invokes a
// provider task. The real implementation is the trigger engine, but the
// resolver tests inject a fake.
//
// Run must spawn the provider task with params {"requests": [...]} and
// block until it returns. A non-nil error means the run did not complete
// successfully (timeout, crash, missing secret: true flag, etc.).
type ProviderRunner interface {
	Run(ctx context.Context, providerID string, reqs []ProviderRequest) (*ProviderResult, error)
}

// Registry is the subset of *registry.Registry the resolver needs. Tests
// pass a minimal fake.
type Registry interface {
	Get(id string) (*task.Spec, bool)
}

// Resolver resolves an env permissions block.
type Resolver struct {
	Runner   ProviderRunner
	Registry Registry
	Secrets  secrets.Chain
	Cache    *cache
	// Now defaults to time.Now if nil; tests inject a stable clock.
	Now func() time.Time
}

// New constructs a Resolver. cacheImpl may be nil; a fresh cache is
// allocated. Wired this way so each runtime constructs its own Resolver
// (cache is shared via the *cache pointer if the runtime chooses).
func New(reg Registry, sc secrets.Chain, runner ProviderRunner) *Resolver {
	return &Resolver{
		Runner:   runner,
		Registry: reg,
		Secrets:  sc,
		Cache:    newCache(),
		Now:      time.Now,
	}
}

// Resolved is the output of a resolution pass.
type Resolved struct {
	// Env is the variable name → value map to inject into the consumer
	// process environment.
	Env map[string]string
	// Secrets is the subset of Env whose values were sourced from a
	// secrets store or a provider task. Caller feeds this to the run-log
	// redactor (pkg/secrets.NewRedactor).
	Secrets map[string]string
}

// Resolve walks spec.Permissions.Env and produces a Resolved. Errors are
// returned typed (ErrProviderUnavailable / ErrRequiredSecretMissing) so
// the trigger engine can categorize the failure for the run log.
//
// The consumer's RunStatus is the caller's responsibility: when Resolve
// errors, the caller marks the consumer run failed with the typed
// reason BEFORE spawning the consumer process.
func (r *Resolver) Resolve(ctx context.Context, spec *task.Spec) (*Resolved, error) {
	out := &Resolved{
		Env:     make(map[string]string, len(spec.Permissions.Env)),
		Secrets: make(map[string]string),
	}

	// 1. Group `from: task:` entries by provider ID.
	type taskEntry struct {
		envName  string
		secretKey string
		optional bool
	}
	byProvider := make(map[string][]taskEntry)
	for _, e := range spec.Permissions.Env {
		// secret:/value:/bare paths share the legacy semantics.
		if e.Value != "" {
			out.Env[e.Name] = e.Value
			continue
		}
		if e.Secret != "" {
			val, err := r.Secrets.Resolve(ctx, e.Secret)
			if err != nil {
				var notFound *secrets.NotFoundError
				if e.Optional && asSecretNotFound(err, &notFound) {
					out.Env[e.Name] = ""
					continue
				}
				return nil, fmt.Errorf("resolve secret %q for env %q: %w", e.Secret, e.Name, err)
			}
			out.Env[e.Name] = val
			out.Secrets[e.Name] = val
			continue
		}
		kind, target := task.ParseFrom(e.From)
		switch kind {
		case task.FromKindTask:
			byProvider[target] = append(byProvider[target], taskEntry{
				envName:  e.Name,
				secretKey: e.Name,
				optional: e.Optional,
			})
		case task.FromKindEnv:
			if target != "" {
				out.Env[e.Name] = os.Getenv(target)
			}
			// fully bare → no injection (allowlist only); leave unset.
		}
	}

	// 2. For each provider, check cache, then batch-spawn for the rest.
	for providerID, entries := range byProvider {
		if err := r.resolveProvider(ctx, providerID, entries, out); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// asSecretNotFound is a tiny shim so we don't import errors here just for
// errors.As.
func asSecretNotFound(err error, target **secrets.NotFoundError) bool {
	if e, ok := err.(*secrets.NotFoundError); ok {
		*target = e
		return true
	}
	return false
}

// resolveProvider handles one provider's batch.
func (r *Resolver) resolveProvider(
	ctx context.Context,
	providerID string,
	entries []taskEntryAlias, // alias defined below
	out *Resolved,
) error {
	spec, ok := r.Registry.Get(providerID)
	if !ok {
		return &ErrProviderUnavailable{
			ProviderID: providerID,
			Cause:      fmt.Errorf("provider task not registered"),
		}
	}

	providerHash, _ := contentHashOf(spec)
	now := r.Now()

	// Sort entries so cache-miss request ordering is deterministic for tests.
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].envName < entries[j].envName })

	misses := make([]ProviderRequest, 0, len(entries))
	cached := make(map[string]string)
	for _, e := range entries {
		if v, ok := r.Cache.get(providerID, e.secretKey, providerHash, now); ok {
			cached[e.envName] = v
			continue
		}
		misses = append(misses, ProviderRequest{Name: e.secretKey, Optional: e.optional})
	}

	var fetched map[string]string
	if len(misses) > 0 {
		res, err := r.Runner.Run(ctx, providerID, misses)
		if err != nil {
			return &ErrProviderUnavailable{ProviderID: providerID, Cause: err}
		}
		if res == nil || res.Values == nil {
			return &ErrProviderMisconfigured{
				ProviderID: providerID,
				Reason:     "task returned no secret map (did it call dicode.output(..., { secret: true })?)",
			}
		}
		fetched = res.Values

		ttl := time.Duration(0)
		if spec.Provider != nil {
			ttl = spec.Provider.CacheTTL
		}
		for _, m := range misses {
			if v, present := fetched[m.Name]; present {
				r.Cache.put(providerID, m.Name, providerHash, v, ttl, now)
			}
		}
	}

	for _, e := range entries {
		val, fromCache := cached[e.envName]
		if !fromCache {
			v, present := fetched[e.secretKey]
			if !present {
				if e.optional {
					out.Env[e.envName] = ""
					continue
				}
				return &ErrRequiredSecretMissing{ProviderID: providerID, Key: e.secretKey}
			}
			val = v
		}
		out.Env[e.envName] = val
		out.Secrets[e.envName] = val
	}
	return nil
}

// taskEntryAlias is the unexported helper alias used by resolveProvider.
// It mirrors the inline struct in Resolve so the helper signature stays
// readable without leaking the struct into the public API.
type taskEntryAlias = struct {
	envName   string
	secretKey string
	optional  bool
}

// contentHashOf returns the task content hash. Wraps task.Hash so the
// resolver can cache by it without depending on the loader directly.
// The hash is computed lazily every cache lookup; since cache lookups are
// per consumer launch (rare relative to task code), the I/O cost is
// acceptable.
//
// Returns ("", err) on read failure; callers treat empty hash as "always
// invalidate" via the cache's content-hash mismatch path.
func contentHashOf(spec *task.Spec) (string, error) {
	if spec.TaskDir == "" {
		return "", nil
	}
	h, err := task.Hash(spec.TaskDir)
	if err != nil {
		return "", err
	}
	return h, nil
}

// _ ensures registry compiles even if unused above.
var _ = registry.StatusSuccess
```

(Note: the `taskEntry` declared inside `Resolve` and the `taskEntryAlias` at file scope must agree. Adjust `Resolve` to use `taskEntryAlias` directly — replace the inner `type taskEntry struct{...}` with `taskEntryAlias` references and rename the local map type accordingly.)

Reconcile by replacing the body of `Resolve` so the inner-struct mismatch goes away. Final `Resolve` body:

```go
func (r *Resolver) Resolve(ctx context.Context, spec *task.Spec) (*Resolved, error) {
	out := &Resolved{
		Env:     make(map[string]string, len(spec.Permissions.Env)),
		Secrets: make(map[string]string),
	}
	byProvider := make(map[string][]taskEntryAlias)
	for _, e := range spec.Permissions.Env {
		if e.Value != "" {
			out.Env[e.Name] = e.Value
			continue
		}
		if e.Secret != "" {
			val, err := r.Secrets.Resolve(ctx, e.Secret)
			if err != nil {
				var notFound *secrets.NotFoundError
				if e.Optional && asSecretNotFound(err, &notFound) {
					out.Env[e.Name] = ""
					continue
				}
				return nil, fmt.Errorf("resolve secret %q for env %q: %w", e.Secret, e.Name, err)
			}
			out.Env[e.Name] = val
			out.Secrets[e.Name] = val
			continue
		}
		kind, target := task.ParseFrom(e.From)
		switch kind {
		case task.FromKindTask:
			byProvider[target] = append(byProvider[target], taskEntryAlias{
				envName:   e.Name,
				secretKey: e.Name,
				optional:  e.Optional,
			})
		case task.FromKindEnv:
			if target != "" {
				out.Env[e.Name] = os.Getenv(target)
			}
		}
	}
	for providerID, entries := range byProvider {
		if err := r.resolveProvider(ctx, providerID, entries, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}
```

- [ ] **Step 2: Sanity-compile**

Run: `go build ./pkg/runtime/envresolve/...`

Expected: build succeeds.

- [ ] **Step 3: Commit**

```bash
git add pkg/runtime/envresolve/resolver.go
git commit -m "feat(envresolve): Resolver with provider grouping + batched spawn"
```

---

### Task 10: Resolver unit tests — happy path, cache hit, batched spawn

**Files:**
- Create: `pkg/runtime/envresolve/resolver_test.go`

- [ ] **Step 1: Write the failing test**

```go
package envresolve

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
)

// fakeRegistry stores task specs by id.
type fakeRegistry struct{ specs map[string]*task.Spec }

func (f *fakeRegistry) Get(id string) (*task.Spec, bool) {
	s, ok := f.specs[id]
	return s, ok
}

// fakeRunner records calls and returns canned values.
type fakeRunner struct {
	calls    int
	lastReqs []ProviderRequest
	values   map[string]string
	err      error
}

func (f *fakeRunner) Run(ctx context.Context, providerID string, reqs []ProviderRequest) (*ProviderResult, error) {
	f.calls++
	f.lastReqs = reqs
	if f.err != nil {
		return nil, f.err
	}
	return &ProviderResult{Values: f.values}, nil
}

func newSpec(id string, env []task.EnvEntry) *task.Spec {
	return &task.Spec{
		ID:          id,
		Name:        id,
		Permissions: task.Permissions{Env: env},
	}
}

func newProviderSpec(id string, ttl time.Duration) *task.Spec {
	return &task.Spec{
		ID:       id,
		Name:     id,
		Provider: &task.ProviderConfig{CacheTTL: ttl},
	}
}

func TestResolve_BatchesProviderSpawnPerLaunch(t *testing.T) {
	reg := &fakeRegistry{specs: map[string]*task.Spec{
		"doppler": newProviderSpec("doppler", 5*time.Minute),
	}}
	runner := &fakeRunner{values: map[string]string{
		"PG_URL":    "postgres://x",
		"REDIS_URL": "redis://y",
	}}
	r := New(reg, secrets.Chain{}, runner)
	r.Now = func() time.Time { return time.Unix(0, 0) }

	consumer := newSpec("consumer", []task.EnvEntry{
		{Name: "PG_URL", From: "task:doppler"},
		{Name: "REDIS_URL", From: "task:doppler"},
	})
	got, err := r.Resolve(context.Background(), consumer)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("expected 1 spawn (batched), got %d", runner.calls)
	}
	if got.Env["PG_URL"] != "postgres://x" || got.Env["REDIS_URL"] != "redis://y" {
		t.Errorf("env = %#v", got.Env)
	}
	if got.Secrets["PG_URL"] != "postgres://x" {
		t.Errorf("PG_URL not flagged as secret: %#v", got.Secrets)
	}
}

func TestResolve_CacheHitSkipsSpawn(t *testing.T) {
	reg := &fakeRegistry{specs: map[string]*task.Spec{
		"doppler": newProviderSpec("doppler", 5*time.Minute),
	}}
	runner := &fakeRunner{values: map[string]string{"PG_URL": "v1"}}
	r := New(reg, secrets.Chain{}, runner)
	r.Now = func() time.Time { return time.Unix(0, 0) }

	consumer := newSpec("consumer", []task.EnvEntry{
		{Name: "PG_URL", From: "task:doppler"},
	})

	if _, err := r.Resolve(context.Background(), consumer); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if _, err := r.Resolve(context.Background(), consumer); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("expected exactly one spawn (second hit cache), got %d", runner.calls)
	}
}

func TestResolve_RequiredKeyMissing(t *testing.T) {
	reg := &fakeRegistry{specs: map[string]*task.Spec{
		"doppler": newProviderSpec("doppler", 0),
	}}
	runner := &fakeRunner{values: map[string]string{}} // returns empty map
	r := New(reg, secrets.Chain{}, runner)
	r.Now = func() time.Time { return time.Unix(0, 0) }

	consumer := newSpec("consumer", []task.EnvEntry{
		{Name: "PG_URL", From: "task:doppler"},
	})
	_, err := r.Resolve(context.Background(), consumer)
	var miss *ErrRequiredSecretMissing
	if !errors.As(err, &miss) {
		t.Fatalf("expected ErrRequiredSecretMissing, got %T %v", err, err)
	}
	if miss.Key != "PG_URL" || miss.ProviderID != "doppler" {
		t.Errorf("err = %+v", miss)
	}
}

func TestResolve_OptionalKeyMissingIsEmpty(t *testing.T) {
	reg := &fakeRegistry{specs: map[string]*task.Spec{
		"doppler": newProviderSpec("doppler", 0),
	}}
	runner := &fakeRunner{values: map[string]string{}}
	r := New(reg, secrets.Chain{}, runner)
	r.Now = func() time.Time { return time.Unix(0, 0) }

	consumer := newSpec("consumer", []task.EnvEntry{
		{Name: "OPTIONAL", From: "task:doppler", Optional: true},
	})
	got, err := r.Resolve(context.Background(), consumer)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Env["OPTIONAL"] != "" {
		t.Errorf("expected empty, got %q", got.Env["OPTIONAL"])
	}
}

func TestResolve_ProviderUnavailable(t *testing.T) {
	reg := &fakeRegistry{specs: map[string]*task.Spec{
		"doppler": newProviderSpec("doppler", 0),
	}}
	runner := &fakeRunner{err: errors.New("spawn failed")}
	r := New(reg, secrets.Chain{}, runner)
	r.Now = func() time.Time { return time.Unix(0, 0) }

	consumer := newSpec("consumer", []task.EnvEntry{
		{Name: "PG_URL", From: "task:doppler"},
	})
	_, err := r.Resolve(context.Background(), consumer)
	var pu *ErrProviderUnavailable
	if !errors.As(err, &pu) {
		t.Fatalf("expected ErrProviderUnavailable, got %T %v", err, err)
	}
}

func TestResolve_BarePrefixIsHostEnv(t *testing.T) {
	t.Setenv("FOO_FROM_HOST", "hello")
	r := New(&fakeRegistry{}, secrets.Chain{}, nil)
	consumer := newSpec("consumer", []task.EnvEntry{
		{Name: "FOO", From: "FOO_FROM_HOST"},
		{Name: "BAR", From: "env:FOO_FROM_HOST"},
	})
	got, err := r.Resolve(context.Background(), consumer)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Env["FOO"] != "hello" || got.Env["BAR"] != "hello" {
		t.Errorf("env = %#v", got.Env)
	}
	if _, ok := got.Secrets["FOO"]; ok {
		t.Errorf("host-env values must NOT be flagged secret")
	}
}
```

- [ ] **Step 2: Run tests**

Run: `go test ./pkg/runtime/envresolve/... -timeout 60s -v`

Expected: all PASS (cache + resolver).

- [ ] **Step 3: Commit**

```bash
git add pkg/runtime/envresolve/resolver_test.go
git commit -m "test(envresolve): batched spawn, cache hit, typed error coverage"
```

---

### Task 11: Resolver test — provider misconfigured (no secret: true)

**Files:**
- Modify: `pkg/runtime/envresolve/resolver_test.go`

- [ ] **Step 1: Append the failing test**

```go
func TestResolve_ProviderReturnsNilMap(t *testing.T) {
	reg := &fakeRegistry{specs: map[string]*task.Spec{
		"misconfigured": newProviderSpec("misconfigured", 0),
	}}
	// runner returns a successful result but with Values=nil — simulates a
	// provider task that called dicode.output without { secret: true }.
	runner := &fakeRunnerNilMap{}
	r := New(reg, secrets.Chain{}, runner)
	r.Now = func() time.Time { return time.Unix(0, 0) }

	consumer := newSpec("consumer", []task.EnvEntry{
		{Name: "PG_URL", From: "task:misconfigured"},
	})
	_, err := r.Resolve(context.Background(), consumer)
	var mis *ErrProviderMisconfigured
	if !errors.As(err, &mis) {
		t.Fatalf("expected ErrProviderMisconfigured, got %T %v", err, err)
	}
}

type fakeRunnerNilMap struct{}

func (fakeRunnerNilMap) Run(ctx context.Context, providerID string, reqs []ProviderRequest) (*ProviderResult, error) {
	return &ProviderResult{Values: nil}, nil
}
```

- [ ] **Step 2: Run**

Run: `go test ./pkg/runtime/envresolve/... -run TestResolve_ProviderReturnsNilMap -v`

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add pkg/runtime/envresolve/resolver_test.go
git commit -m "test(envresolve): ErrProviderMisconfigured when Values=nil"
```

---

### Task 12: Add IPC capability + Request fields for secret output

**Files:**
- Modify: `pkg/ipc/capability.go`
- Modify: `pkg/ipc/message.go:40-92`

- [ ] **Step 1: Add capability constant**

In `pkg/ipc/capability.go`, alongside the existing task-shim caps, add:

```go
	// CapOutputSecret allows a task to call dicode.output(map, {
	// secret: true }) — flagging values for daemon-side redaction and
	// (for provider tasks) routing to the env-resolver waiting on the
	// caller side. Granted to every task token by default; the cap exists
	// only so future denial policies can revoke it.
	CapOutputSecret = "output.secret"
```

…and append `CapOutputSecret` to `defaultTaskCaps()`'s return slice.

- [ ] **Step 2: Add Request fields**

Append to the `Request` struct in `pkg/ipc/message.go` (insert just before the `// dicode.*` block at line 57):

```go
	// output (issue #119): when Secret is true, ContentType/Content are
	// ignored and SecretMap (a flat map[string]string) carries the
	// resolved provider response. The map values feed the run-log
	// redactor + the resolver awaiting the provider's run.
	Secret    bool            `json:"secret,omitempty"`
	SecretMap json.RawMessage `json:"secretMap,omitempty"`
```

- [ ] **Step 3: Sanity-compile**

Run: `go build ./pkg/ipc/...`

Expected: builds clean.

- [ ] **Step 4: Commit**

```bash
git add pkg/ipc/capability.go pkg/ipc/message.go
git commit -m "feat(ipc): CapOutputSecret cap + Request.Secret/SecretMap fields"
```

---

### Task 13: Define server-side secret-output callback

**Files:**
- Modify: `pkg/ipc/server.go:40-92` (Server struct), and the `output` case at `pkg/ipc/server.go:399-413`.

- [ ] **Step 1: Add fields and a setter**

Append fields to the `Server` struct (between `output *OutputResult` and `retCh chan any` at `pkg/ipc/server.go:83-84`):

```go
	// secretOut, when non-nil, receives the flat map produced by a
	// provider task calling dicode.output(map, { secret: true }). The
	// resolver waiting on the consumer's launch sets this via
	// SetSecretOutput; once received, the same values are also fed into
	// s.redactor for run-log scrubbing and the run log records key
	// names with [redacted] placeholders only.
	secretOut chan map[string]string
```

Add a setter (anywhere in the file alongside `SetRedactor`):

```go
// SetSecretOutput wires the channel that receives a provider task's
// secret map. Call BEFORE Start. Buffer >=1 is required so the IPC
// goroutine does not block on the channel send.
func (s *Server) SetSecretOutput(ch chan map[string]string) {
	s.secretOut = ch
}
```

- [ ] **Step 2: Replace the `output` handler**

At `pkg/ipc/server.go:399-413`, replace the case body with:

```go
		case "output":
			if !hasCap(caps, CapOutputWrite) {
				continue
			}
			if req.Secret {
				if !hasCap(caps, CapOutputSecret) {
					continue
				}
				// Flat map: decode SecretMap as map[string]string. Reject
				// nested objects per issue #119.
				var sm map[string]string
				if err := json.Unmarshal(req.SecretMap, &sm); err != nil {
					s.log.Warn("ipc: secret output: not a flat string map",
						zap.String("run", s.runID),
						zap.Error(err),
					)
					continue
				}
				// Feed values into the redactor so any later log line
				// containing them is scrubbed before persistence. Use a
				// new redactor merged with the existing one to preserve
				// already-resolved secrets.
				s.mu.Lock()
				existing := map[string]string{}
				if s.redactor != nil {
					// merge by re-keying values of `sm` under any key —
					// NewRedactor only inspects values.
					for k, v := range sm {
						existing[k] = v
					}
				} else {
					for k, v := range sm {
						existing[k] = v
					}
				}
				s.redactor = secrets.NewRedactor(existing)
				s.mu.Unlock()

				// Persist key names + [redacted] placeholders to the run
				// log so operators can audit which secrets the provider
				// returned without leaking values.
				keys := make([]string, 0, len(sm))
				for k := range sm {
					keys = append(keys, k)
				}
				_ = s.registry.AppendLog(context.Background(), s.runID, "info",
					fmt.Sprintf("[dicode] secret output: %v = [redacted]", keys))

				if s.secretOut != nil {
					select {
					case s.secretOut <- sm:
					default:
						s.log.Warn("ipc: secretOut channel full or unread",
							zap.String("run", s.runID))
					}
				}
				continue
			}
			var data any
			if len(req.Data) > 0 {
				_ = json.Unmarshal(req.Data, &data)
			}
			s.mu.Lock()
			s.output = &OutputResult{
				ContentType: req.ContentType,
				Content:     req.Content,
				Data:        data,
			}
			s.mu.Unlock()
```

- [ ] **Step 3: Sanity-compile**

Run: `go build ./pkg/ipc/...`

Expected: clean build.

- [ ] **Step 4: Commit**

```bash
git add pkg/ipc/server.go
git commit -m "feat(ipc): handle output { secret: true } — redact + route to resolver"
```

---

### Task 14: IPC server test — secret output redacts and routes

**Files:**
- Modify: `pkg/ipc/server_test.go`

- [ ] **Step 1: Add the failing test (append at end of file)**

```go
func TestServer_SecretOutputRoutedAndRedacted(t *testing.T) {
	srv, conn, cleanup := newTestServer(t)
	defer cleanup()

	out := make(chan map[string]string, 1)
	srv.SetSecretOutput(out)

	// Send: { method: "output", secret: true, secretMap: {"PG_URL":"postgres://x"} }
	writeFrame(t, conn, map[string]any{
		"method":    "output",
		"secret":    true,
		"secretMap": map[string]string{"PG_URL": "postgres://x"},
	})

	select {
	case got := <-out:
		if got["PG_URL"] != "postgres://x" {
			t.Errorf("got = %#v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("secret map not routed to channel")
	}

	// Confirm the run log contains "[redacted]" not the value.
	logs, _ := srv.registry.GetRunLogs(context.Background(), srv.runID)
	found := false
	for _, l := range logs {
		if strings.Contains(l.Message, "[redacted]") {
			found = true
			if strings.Contains(l.Message, "postgres://x") {
				t.Errorf("plaintext leaked into log: %q", l.Message)
			}
		}
	}
	if !found {
		t.Error("expected [redacted] log entry")
	}
}
```

(`newTestServer`, `writeFrame` are existing test helpers in `pkg/ipc/server_test.go`. If a needed helper signature differs, adapt the call but keep the test intent identical. Verify by reading `pkg/ipc/server_test.go` if it does not compile.)

- [ ] **Step 2: Run**

Run: `go test ./pkg/ipc/... -run TestServer_SecretOutputRoutedAndRedacted -v`

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add pkg/ipc/server_test.go
git commit -m "test(ipc): secret output routes to channel and redacts in log"
```

---

### Task 15: IPC server rejects nested-object secret maps

**Files:**
- Modify: `pkg/ipc/server_test.go`

- [ ] **Step 1: Append failing test**

```go
func TestServer_SecretOutputRejectsNestedMap(t *testing.T) {
	srv, conn, cleanup := newTestServer(t)
	defer cleanup()

	out := make(chan map[string]string, 1)
	srv.SetSecretOutput(out)

	writeFrame(t, conn, map[string]any{
		"method":    "output",
		"secret":    true,
		// nested object — not a Record<string,string>.
		"secretMap": map[string]any{"PG": map[string]string{"URL": "x"}},
	})

	select {
	case got := <-out:
		t.Fatalf("nested map was accepted: %#v", got)
	case <-time.After(200 * time.Millisecond):
		// success — server logged-and-dropped.
	}
}
```

- [ ] **Step 2: Run**

Run: `go test ./pkg/ipc/... -run TestServer_SecretOutputRejectsNestedMap -v`

Expected: PASS (the existing `json.Unmarshal` into `map[string]string` already rejects nested values; the test confirms it).

- [ ] **Step 3: Commit**

```bash
git add pkg/ipc/server_test.go
git commit -m "test(ipc): reject nested-object secret maps"
```

---

### Task 16: Extend Deno SDK output API to accept `{ secret: true }`

**Files:**
- Modify: `pkg/runtime/deno/sdk/shim.ts:52-61, 239-246, 263-279`
- Modify: `pkg/runtime/deno/sdk/sdk.d.ts` (mirror declared types — find the `Output` interface and update; if not present, skip).

- [ ] **Step 1: Update `Output` interface**

Replace lines 52-61 (`OutputOptions` + `Output` interface) with:

```typescript
export interface OutputOptions {
  data?: Record<string, unknown> | null;
}

// Secret output flag — when true, the daemon treats `value` as a flat
// Record<string, string> and routes it to the resolver awaiting this
// task. Values are also fed to the run-log redactor and the run log
// records keys with [redacted] placeholders only. Issue #119.
export interface SecretOutputOptions {
  secret: true;
}

export interface Output {
  html:  (content: string, opts?: OutputOptions) => Promise<void>;
  text:  (content: string)                        => Promise<void>;
  image: (mime: string | null, content: string)   => Promise<void>;
  file:  (name: string, content: string, mime?: string) => Promise<void>;
  // Provider-task entry point (issue #119). Throws synchronously if
  // `value` is not a flat Record<string,string>.
  (value: Record<string, string>, opts: SecretOutputOptions): Promise<void>;
}
```

- [ ] **Step 2: Replace the output-object construction at line 241-246**

```typescript
function __outputCallable__(value: Record<string, string>, opts: SecretOutputOptions): Promise<void> {
  // Validate flat string map up front so the failure surface is the
  // SDK call site, not "the daemon dropped it silently".
  for (const [k, v] of Object.entries(value)) {
    if (typeof v !== "string") {
      return Promise.reject(new Error(
        `dicode.output(map, { secret: true }): value for key ${JSON.stringify(k)} is not a string`));
    }
  }
  return __fire__({ method: "output", secret: true, secretMap: value });
}

const __outputObj__ = {
  html:  (content: string, opts?: OutputOptions) => __fire__({ method: "output", contentType: "text/html",                     content, data: opts?.data ?? null }),
  text:  (content: string)                       => __fire__({ method: "output", contentType: "text/plain",                    content }),
  image: (mime: string | null, content: string)  => __fire__({ method: "output", contentType: mime ?? "image/png",             content }),
  file:  (name: string, content: string, mime?: string) => __fire__({ method: "output", contentType: mime ?? "application/octet-stream", content, data: { filename: name } }),
};

// Synthesize a callable+method object. JavaScript functions ARE objects,
// so attach the four methods as properties on the function.
const output: Output = Object.assign(__outputCallable__, __outputObj__) as unknown as Output;
```

- [ ] **Step 3: Sanity-check the export line at 288 still works (it does — unchanged).**

- [ ] **Step 4: Run existing Deno SDK tests if any**

Run: `go test ./pkg/runtime/deno/... -timeout 60s`

Expected: all existing tests continue to pass (no test exercises the new path yet).

- [ ] **Step 5: Commit**

```bash
git add pkg/runtime/deno/sdk/shim.ts
git commit -m "feat(deno-sdk): output(map, { secret: true }) for provider tasks"
```

---

### Task 17: Deno integration test — output as provider routes via IPC

**Files:**
- Create: `pkg/runtime/deno/secret_output_test.go`

- [ ] **Step 1: Write the failing test**

```go
package deno

import (
	"context"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
)

// TestRun_SecretOutputRoutedToChannel runs a real Deno task that calls
// `dicode.output({PG:"x"}, { secret: true })` and asserts the daemon's
// secret-output channel sees the map.
//
// Skipped in -short mode because it spawns an actual Deno subprocess.
func TestRun_SecretOutputRoutedToChannel(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt, reg, cleanup := newTestRuntime(t)
	defer cleanup()

	spec := writeProviderTask(t, "doppler", `
export default async function main({ output }) {
  await output({ PG_URL: "postgres://x" }, { secret: true });
}
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}

	out := make(chan map[string]string, 1)
	rt.SetSecretOutputChannel(out) // see Task 22

	res, err := rt.Run(ctx, spec, RunOptions{RunID: "run-1"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Error != nil {
		t.Fatalf("run error: %v", res.Error)
	}

	select {
	case got := <-out:
		if got["PG_URL"] != "postgres://x" {
			t.Errorf("got = %#v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("secret output not routed")
	}

	// also assert no log line contains the plaintext value.
	logs, _ := reg.GetRunLogs(ctx, "run-1")
	for _, l := range logs {
		if strings.Contains(l.Message, "postgres://x") {
			t.Errorf("plaintext leaked: %q", l.Message)
		}
	}

	_ = ipc.CapOutputSecret      // ensure import used
	_ = secrets.NewRedactor(nil) // ensure import used
	_ = registry.StatusSuccess   // ensure import used
	_ = task.RuntimeDeno         // ensure import used
}
```

(`newTestRuntime`, `writeProviderTask` are helpers we'll add in Task 18.)

- [ ] **Step 2: Run — expect failure (helpers + plumbing missing)**

Run: `go test ./pkg/runtime/deno/... -run TestRun_SecretOutputRoutedToChannel -v`

Expected: FAIL — `newTestRuntime`, `writeProviderTask`, `SetSecretOutputChannel` undefined.

- [ ] **Step 3: Commit (failing test only)**

```bash
git add pkg/runtime/deno/secret_output_test.go
git commit -m "test(deno): provider task secret output routes via channel (failing)"
```

---

### Task 18: Add Deno test helpers

**Files:**
- Create: `pkg/runtime/deno/test_helpers_test.go`

- [ ] **Step 1: Implement helpers**

```go
package deno

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap/zaptest"
)

func newTestRuntime(t *testing.T) (*Runtime, *registry.Registry, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	tdb, err := db.NewSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New(tdb)
	rt, err := New(reg, nil, tdb, zaptest.NewLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	return rt, reg, func() { _ = tdb.Close(); _ = os.RemoveAll(tmpDir) }
}

func writeProviderTask(t *testing.T, id, body string) *task.Spec {
	t.Helper()
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "task.yaml")
	tsPath := filepath.Join(dir, "task.ts")
	yamlContent := `apiVersion: dicode/v1
kind: Task
name: ` + id + `
runtime: deno
trigger:
  manual: true
provider:
  cache_ttl: 5m
`
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tsPath, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	spec, err := task.LoadDir(dir)
	if err != nil {
		t.Fatalf("load %s: %v", dir, err)
	}
	spec.ID = id
	return spec
}
```

- [ ] **Step 2: Sanity-compile**

Run: `go vet ./pkg/runtime/deno/...`

Expected: no errors (helpers compile, even though `SetSecretOutputChannel` is still missing — that's the next task).

- [ ] **Step 3: Commit**

```bash
git add pkg/runtime/deno/test_helpers_test.go
git commit -m "test(deno): add newTestRuntime and writeProviderTask helpers"
```

---

### Task 19: Mirror SDK output extension in Python

**Files:**
- Modify: `pkg/runtime/python/sdk/dicode_sdk.py:257-284`

- [ ] **Step 1: Replace the `_Output` class block**

Replace the existing `# ── output ───…` section (lines 257-284) with:

```python
# ── output ────────────────────────────────────────────────────────────────────


class _Output:
    """
    Callable + method object. Calling output({...}, secret=True) flags the
    map for daemon-side redaction + provider-response routing (issue #119).
    Method calls (output.html, output.text, ...) preserve the legacy
    structured-output API.
    """

    def __call__(self, value, secret=False):
        if not secret:
            raise TypeError(
                "output(value) requires secret=True. "
                "Use output.html / output.text / output.image / output.file "
                "for non-secret structured output."
            )
        if not isinstance(value, dict):
            raise TypeError("output(map, secret=True): value must be a dict")
        for k, v in value.items():
            if not isinstance(k, str):
                raise TypeError(
                    f"output(map, secret=True): key {k!r} is not a string"
                )
            if not isinstance(v, str):
                raise TypeError(
                    f"output(map, secret=True): value for {k!r} is not a string"
                )
        _fire({"method": "output", "secret": True, "secretMap": value})

    def html(self, content, data=None):
        _fire({"method": "output", "contentType": "text/html",
               "content": content, "data": data})

    def text(self, content):
        _fire({"method": "output", "contentType": "text/plain",
               "content": content})

    def image(self, mime, content):
        _fire({"method": "output", "contentType": mime or "image/png",
               "content": content})

    def file(self, name, content, mime=None):
        _fire({"method": "output",
               "contentType": mime or "application/octet-stream",
               "content": content, "data": {"filename": name}})

    # Async variants — _fire is non-blocking, no executor needed.
    async def html_async(self, content, data=None):  self.html(content, data)
    async def text_async(self, content):              self.text(content)
    async def image_async(self, mime, content):       self.image(mime, content)
    async def file_async(self, name, content, mime=None): self.file(name, content, mime)


output = _Output()
```

- [ ] **Step 2: Run existing Python runtime tests**

Run: `go test ./pkg/runtime/python/... -timeout 60s`

Expected: all existing tests still pass (no test exercises secret output yet).

- [ ] **Step 3: Commit**

```bash
git add pkg/runtime/python/sdk/dicode_sdk.py
git commit -m "feat(python-sdk): output(map, secret=True) for provider tasks"
```

---

### Task 20: Plumb SecretOutput channel into Deno Runtime

**Files:**
- Modify: `pkg/runtime/deno/runtime.go:70-100, 234-242`

- [ ] **Step 1: Add field + setter on Runtime**

Insert into the `Runtime` struct (after `rotationActiveFn func() bool`):

```go
	// secretOutputCh is opt-in: when set, every Run wires it into the
	// per-run IPC server so a provider task's dicode.output(..., {secret:
	// true}) call is routed to the resolver awaiting it. Nil leaves the
	// path inert (current behavior).
	secretOutputCh chan map[string]string
```

Add the setter near the other Set* methods:

```go
// SetSecretOutputChannel wires the channel that receives provider tasks'
// secret maps. Called by the trigger engine before invoking Run when the
// task is being launched in "provider" mode (see ProviderRunner adapter
// in pkg/trigger).
func (rt *Runtime) SetSecretOutputChannel(ch chan map[string]string) {
	rt.secretOutputCh = ch
}
```

- [ ] **Step 2: Use it in `Run`**

Insert just after `srv.SetRedactor(redactor)` (currently at `pkg/runtime/deno/runtime.go:238`):

```go
	if rt.secretOutputCh != nil {
		srv.SetSecretOutput(rt.secretOutputCh)
	}
```

- [ ] **Step 3: Run integration test from Task 17**

Run: `go test ./pkg/runtime/deno/... -run TestRun_SecretOutputRoutedToChannel -v`

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add pkg/runtime/deno/runtime.go
git commit -m "feat(deno): wire SecretOutput channel into per-run IPC server"
```

---

### Task 21: Same plumbing in Python Runtime executor

**Files:**
- Modify: `pkg/runtime/python/runtime.go:57-147, 222-226`

- [ ] **Step 1: Add field + setter**

Append to the `Runtime` struct (after `gateway *ipc.Gateway`):

```go
	secretOutputCh chan map[string]string
```

Add setter:

```go
// SetSecretOutputChannel — see Deno runtime; identical semantics.
func (rt *Runtime) SetSecretOutputChannel(ch chan map[string]string) {
	rt.secretOutputCh = ch
}
```

In `NewExecutor` (line ~121), pass it through:

```go
func (rt *Runtime) NewExecutor(binaryPath string) pkgruntime.Executor {
	return &executor{
		uvPath:         binaryPath,
		reg:            rt.reg,
		secrets:        rt.secrets,
		secretsManager: rt.secretsManager,
		db:             rt.db,
		log:            rt.log,
		secret:         rt.secret,
		engine:         rt.engine,
		gateway:        rt.gateway,
		secretOutputCh: rt.secretOutputCh,
	}
}
```

Append to `executor` struct:

```go
	secretOutputCh chan map[string]string
```

In `Execute`, just after `srv.SetRedactor(redactor)`:

```go
	if e.secretOutputCh != nil {
		srv.SetSecretOutput(e.secretOutputCh)
	}
```

- [ ] **Step 2: Sanity-compile**

Run: `go build ./pkg/runtime/python/...`

Expected: clean build.

- [ ] **Step 3: Commit**

```bash
git add pkg/runtime/python/runtime.go
git commit -m "feat(python): wire SecretOutput channel into per-run IPC server"
```

---

### Task 22: Replace inline env resolution with envresolve.Resolver — Deno

**Files:**
- Modify: `pkg/runtime/deno/runtime.go:177-214`

- [ ] **Step 1: Replace the inline loop**

Replace lines 177-214 (the entire `resolved`/`resolvedSecrets` for-loop and `redactor := secrets.NewRedactor(...)`) with:

```go
	// Resolve declared env permissions via the shared resolver. Provider
	// tasks (from: task:<id>) are spawned and batched at most once per
	// provider per launch; legacy paths (secret:, env:NAME, bare) are
	// preserved.
	resolvedRes, err := rt.envresolver().Resolve(ctx, spec)
	if err != nil {
		status = registry.StatusFailure
		result.Error = err
		return result, nil
	}
	resolved := resolvedRes.Env
	redactor := secrets.NewRedactor(resolvedRes.Secrets)
```

- [ ] **Step 2: Add the `envresolver` accessor**

Add at the bottom of `pkg/runtime/deno/runtime.go`:

```go
// envresolver lazily constructs the env resolver. Wired with the daemon's
// secret chain + a provider runner that calls back into the trigger
// engine. Nil engine (test harness with no providers) yields a resolver
// that errors on any from: task:<id> entry.
func (rt *Runtime) envresolver() *envresolve.Resolver {
	return envresolve.New(rt.registry, rt.secrets, rt.providerRunner)
}
```

Add fields + setter:

```go
	providerRunner envresolve.ProviderRunner
}

// SetProviderRunner wires the env-resolver's provider invocation. The
// trigger engine implements ProviderRunner and registers itself here at
// daemon startup. Nil disables provider task: lookups.
func (rt *Runtime) SetProviderRunner(p envresolve.ProviderRunner) {
	rt.providerRunner = p
}
```

(Move the closing `}` of the `Runtime` struct accordingly — i.e. the new field belongs INSIDE the struct.)

Add the import: `"github.com/dicode/dicode/pkg/runtime/envresolve"`.

- [ ] **Step 3: Run existing Deno tests**

Run: `go test ./pkg/runtime/deno/... -timeout 60s -short`

Expected: all PASS — the legacy paths (bare/secret/value) remain unchanged because `Resolve` preserves them.

- [ ] **Step 4: Commit**

```bash
git add pkg/runtime/deno/runtime.go
git commit -m "refactor(deno): use envresolve.Resolver for env-permission resolution"
```

---

### Task 23: Same refactor for Python runtime

**Files:**
- Modify: `pkg/runtime/python/runtime.go:161-196`

- [ ] **Step 1: Replace the inline loop**

Replace lines 161-196 of `Execute` (`resolved` / `resolvedSecrets` build + `redactor :=`) with:

```go
	// Shared env-resolution stage — see pkg/runtime/envresolve/resolver.go.
	resolvedRes, err := envresolve.New(e.reg, e.secrets, e.providerRunner).Resolve(ctx, spec)
	if err != nil {
		status = registry.StatusFailure
		result.Error = err
		return result, nil
	}
	resolved := resolvedRes.Env
	redactor := secrets.NewRedactor(resolvedRes.Secrets)
```

Add field on `executor`:

```go
	providerRunner envresolve.ProviderRunner
```

Pass it through in `NewExecutor`:

```go
		providerRunner: rt.providerRunner,
```

Add field + setter on `Runtime`:

```go
	providerRunner envresolve.ProviderRunner
```

```go
func (rt *Runtime) SetProviderRunner(p envresolve.ProviderRunner) {
	rt.providerRunner = p
}
```

Add import: `"github.com/dicode/dicode/pkg/runtime/envresolve"`.

- [ ] **Step 2: Compile + run tests**

Run: `go test ./pkg/runtime/python/... -timeout 60s -short`

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add pkg/runtime/python/runtime.go
git commit -m "refactor(python): use envresolve.Resolver for env-permission resolution"
```

---

### Task 24: Add fail_reason column + FinishRunWithReason

**Files:**
- Modify: `pkg/db/sqlite.go:110-118`
- Modify: `pkg/registry/registry.go:28-42, 132-139`

- [ ] **Step 1: Add migration**

In `pkg/db/sqlite.go` ALTER list (around line 110-118), add:

```go
		`ALTER TABLE runs ADD COLUMN fail_reason TEXT NOT NULL DEFAULT ''`,
```

- [ ] **Step 2: Add field + new method on Registry**

In `pkg/registry/registry.go`, append to the `Run` struct (after `OutputContent`):

```go
	// FailureReason is a typed reason string set when Status == StatusFailure.
	// Format: "<category>: <detail>", e.g. "provider_unavailable: doppler"
	// or "required_secret_missing: PG_URL from doppler". Empty for non-failed
	// runs and for failures from the legacy code path that doesn't set a reason.
	FailureReason string
```

Add a sibling to `FinishRun`:

```go
// FinishRunWithReason updates run status, finished_at, AND fail_reason.
// Used by the trigger engine when env resolution fails with a typed
// envresolve error before the consumer process is even spawned.
func (r *Registry) FinishRunWithReason(ctx context.Context, runID, status, reason string) error {
	now := time.Now().UnixMilli()
	return r.db.Exec(ctx,
		`UPDATE runs SET status = ?, finished_at = ?, fail_reason = ? WHERE id = ?`,
		status, now, reason, runID,
	)
}
```

Update `GetRun` and `ListRuns` SELECTs to include `COALESCE(fail_reason, '')` and scan it into `run.FailureReason`. Concretely, change the SELECT lists from:

```go
`SELECT id, task_id, status, started_at, finished_at, parent_run_id, trigger_source,
        COALESCE(return_value, ''), COALESCE(output_content_type, ''), COALESCE(output_content, '')
 FROM runs ...`
```

to:

```go
`SELECT id, task_id, status, started_at, finished_at, parent_run_id, trigger_source,
        COALESCE(return_value, ''), COALESCE(output_content_type, ''), COALESCE(output_content, ''),
        COALESCE(fail_reason, '')
 FROM runs ...`
```

…and update both `Scan` calls to append `&run.FailureReason` as the final column.

- [ ] **Step 3: Run registry tests**

Run: `go test ./pkg/registry/... ./pkg/db/... -timeout 60s`

Expected: PASS (existing tests don't read FailureReason; migration is additive).

- [ ] **Step 4: Commit**

```bash
git add pkg/db/sqlite.go pkg/registry/registry.go
git commit -m "feat(registry): typed Run.FailureReason + FinishRunWithReason migration"
```

---

### Task 25: Trigger engine — typed pre-launch failure + FireChain

**Files:**
- Modify: `pkg/trigger/engine.go` — engine wiring + a new `dispatchEnv` step.

- [ ] **Step 1: Implement `ProviderRunner` on `*Engine`**

Append to `pkg/trigger/engine.go`:

```go
// Run satisfies envresolve.ProviderRunner. Spawns the provider task
// synchronously and waits for it to finish; the secret map is collected
// over the IPC channel pre-wired into the runtime by SetSecretOutputChannel.
//
// The buffered channel (capacity 1) is created per call and passed
// through a per-launch swap so concurrent launches do not interfere.
//
// Errors:
//   - ctx.Err() if the caller context expires
//   - WaitRun error if the run fails / cancels
//   - errors.New("no secret output") if the run finished without sending
//     a map (provider didn't call output(..., {secret: true}))
func (e *Engine) Run(ctx context.Context, providerID string, reqs []envresolve.ProviderRequest) (*envresolve.ProviderResult, error) {
	spec, ok := e.registry.Get(providerID)
	if !ok {
		return nil, fmt.Errorf("provider task %q not registered", providerID)
	}

	ch := make(chan map[string]string, 1)
	switch spec.Runtime {
	case task.RuntimeDeno, "", "js":
		e.denoRuntime.SetSecretOutputChannel(ch)
		defer e.denoRuntime.SetSecretOutputChannel(nil)
	default:
		e.pythonRuntime.SetSecretOutputChannel(ch)
		defer e.pythonRuntime.SetSecretOutputChannel(nil)
	}

	// Build params: { "requests": [{name,optional}, ...] }
	reqJSON, _ := json.Marshal(map[string]any{"requests": reqs})
	runID, err := e.fireAsync(ctx, spec, pkgruntime.RunOptions{
		Params: map[string]string{"requests": string(reqJSON)},
	}, "provider")
	if err != nil {
		return nil, fmt.Errorf("fire provider: %w", err)
	}
	if _, err := e.WaitRun(ctx, runID); err != nil {
		return nil, fmt.Errorf("wait provider %q: %w", providerID, err)
	}

	select {
	case sm := <-ch:
		return &envresolve.ProviderResult{Values: sm}, nil
	default:
		return nil, fmt.Errorf("provider %q completed without secret output", providerID)
	}
}
```

(Single-runtime swap: declare `denoRuntime` / `pythonRuntime` fields on `*Engine` if not already present; wire them at daemon construction.)

- [ ] **Step 2: At engine init, register itself**

Wherever the engine is constructed (`pkg/trigger/engine.go` `New(...)` or the daemon wiring file) — after the runtimes are passed in — call:

```go
denoRuntime.SetProviderRunner(eng)
pythonRuntime.SetProviderRunner(eng)
```

- [ ] **Step 3: Pre-launch typed failure in `runTask`**

Before `e.dispatch(runCtx, spec, opts)` at line 1184, insert:

```go
	// Issue #119: env-resolution typed-failure short-circuit. The
	// resolver also runs inside dispatch (deno/python), but a failure
	// there surfaces as a generic error. Run the resolution once here
	// so we can categorize provider failures, mark the run with a
	// typed fail_reason, and trigger FireChain BEFORE the consumer
	// process spawns.
	preStatus, preReason := e.preflightEnv(runCtx, spec)
	if preStatus != "" {
		_ = e.registry.FinishRunWithReason(context.Background(), opts.RunID, preStatus, preReason)
		go e.FireChain(context.Background(), spec.ID, opts.RunID, preStatus, nil)
		if h := e.runFinishedHook; h != nil {
			notifyOnSuccess, notifyOnFailure := e.resolveNotify(spec)
			h(spec.ID, opts.RunID, preStatus, source, 0, notifyOnSuccess, notifyOnFailure)
		}
		return preStatus, &pkgruntime.RunResult{RunID: opts.RunID, Error: errors.New(preReason)}
	}
```

Add a helper:

```go
// preflightEnv runs the env resolver once before dispatch so that typed
// provider failures (provider_unavailable / required_secret_missing /
// provider misconfigured) can be recorded as the run's fail_reason
// instead of surfacing as opaque dispatch errors. Returns ("", "") on
// success — the resolver is run again inside the runtime, but cache
// hits make the second call essentially free.
func (e *Engine) preflightEnv(ctx context.Context, spec *task.Spec) (status, reason string) {
	r := envresolve.New(e.registry, e.secrets, e)
	_, err := r.Resolve(ctx, spec)
	if err == nil {
		return "", ""
	}
	var pu *envresolve.ErrProviderUnavailable
	var rsm *envresolve.ErrRequiredSecretMissing
	var mis *envresolve.ErrProviderMisconfigured
	switch {
	case errors.As(err, &pu):
		return registry.StatusFailure, "provider_unavailable: " + pu.ProviderID
	case errors.As(err, &rsm):
		return registry.StatusFailure, "required_secret_missing: " + rsm.Key + " from " + rsm.ProviderID
	case errors.As(err, &mis):
		return registry.StatusFailure, "provider_misconfigured: " + mis.ProviderID
	default:
		// non-typed error: fall through to dispatch so the runtime can
		// report it through its own log path.
		return "", ""
	}
}
```

(Add the imports: `"errors"`, `"github.com/dicode/dicode/pkg/runtime/envresolve"`.)

- [ ] **Step 4: Add `e.secrets` field if not already present on Engine**

If the Engine struct does not already hold a `secrets.Chain`, add:

```go
	secrets secrets.Chain
```

…and a setter:

```go
func (e *Engine) SetSecretsChain(c secrets.Chain) { e.secrets = c }
```

…wired at daemon startup alongside `SetDefaultsOnFailureChain`.

- [ ] **Step 5: Run the trigger engine tests**

Run: `go test ./pkg/trigger/... -timeout 60s`

Expected: existing tests PASS; the new path is exercised by Task 26's reference Doppler test.

- [ ] **Step 6: Commit**

```bash
git add pkg/trigger/engine.go
git commit -m "feat(trigger): preflight env-resolve, typed fail_reason, ProviderRunner impl"
```

---

### Task 26: Reference Doppler provider — task.yaml

**Files:**
- Create: `tasks/buildin/secret-providers/doppler/task.yaml`

- [ ] **Step 1: Write the spec**

```yaml
apiVersion: dicode/v1
kind: Task
name: "Doppler Secret Provider"
description: >
  Resolves secrets from a Doppler workspace by calling the Doppler REST API
  (https://docs.doppler.com/reference/api). Reads requests from
  dicode.params.requests = [{ name, optional }, ...] and emits the resolved
  flat map via dicode.output(map, { secret: true }).

  Bootstrap: set DOPPLER_TOKEN via `dicode secrets set DOPPLER_TOKEN dp.st.xxx`
  (workspace service token). The token is injected via permissions.env so
  daemon redaction strips it from any accidental log line.

runtime: deno

trigger:
  manual: true

params:
  requests:
    type: string
    required: true
    description: >
      JSON-encoded array of {name, optional} request objects. The daemon
      env-resolver passes this automatically; users should not call this
      task directly.

permissions:
  env:
    - name: DOPPLER_TOKEN
      secret: DOPPLER_TOKEN
  net:
    - api.doppler.com

provider:
  cache_ttl: 5m

timeout: 10s

notify:
  on_success: false
  on_failure: true
```

- [ ] **Step 2: Commit**

```bash
git add tasks/buildin/secret-providers/doppler/task.yaml
git commit -m "feat(buildin/doppler): provider task.yaml with cache_ttl=5m"
```

---

### Task 27: Reference Doppler provider — task.ts

**Files:**
- Create: `tasks/buildin/secret-providers/doppler/task.ts`

- [ ] **Step 1: Write the body**

```typescript
// buildin/secret-providers/doppler — Doppler REST API secret provider (issue #119).
//
// Contract:
//   input params:
//     requests: JSON-encoded [{name: string, optional: boolean}, ...]
//   env (declared in permissions.env):
//     DOPPLER_TOKEN: workspace service token (set via `dicode secrets set`)
//   output:
//     dicode.output({ <name>: <value>, ... }, { secret: true })
//
// We hit `GET https://api.doppler.com/v3/configs/config/secrets` with an
// auth header and pluck only the requested keys. Optional misses become
// absent in the output map; required misses surface as a thrown error
// which the trigger engine renders as required_secret_missing.

interface SecretRequest { name: string; optional: boolean; }

interface DopplerSecretsResp {
  secrets: Record<string, { computed: string }>;
}

export default async function main({ params, output }: DicodeSdk) {
  const reqsJSON = (await params.get("requests")) ?? "[]";
  const requests: SecretRequest[] = JSON.parse(reqsJSON);

  const token = Deno.env.get("DOPPLER_TOKEN");
  if (!token) {
    throw new Error("DOPPLER_TOKEN not set; run `dicode secrets set DOPPLER_TOKEN dp.st.xxx`");
  }

  const resp = await fetch("https://api.doppler.com/v3/configs/config/secrets", {
    headers: {
      "Accept": "application/json",
      "Authorization": "Bearer " + token,
    },
  });
  if (!resp.ok) {
    throw new Error(`Doppler API ${resp.status}: ${await resp.text()}`);
  }
  const body = (await resp.json()) as DopplerSecretsResp;

  const out: Record<string, string> = {};
  for (const r of requests) {
    const entry = body.secrets[r.name];
    if (entry && typeof entry.computed === "string") {
      out[r.name] = entry.computed;
    } else if (!r.optional) {
      throw new Error(`required secret ${r.name} not present in Doppler config`);
    }
  }

  await output(out, { secret: true });
}
```

- [ ] **Step 2: Commit**

```bash
git add tasks/buildin/secret-providers/doppler/task.ts
git commit -m "feat(buildin/doppler): Doppler REST API provider task body"
```

---

### Task 28: Doppler provider test (mocked HTTP)

**Files:**
- Create: `tasks/buildin/secret-providers/doppler/task.test.ts`

- [ ] **Step 1: Write the test**

```typescript
// task.test.ts — Doppler provider with mocked fetch.
//
// We monkeypatch globalThis.fetch and exercise the task body's request
// pluck logic. The test runs under `deno test` (the same harness as
// other buildin tasks).

import { assertEquals, assertRejects } from "https://deno.land/std@0.220.0/assert/mod.ts";
import main from "./task.ts";

Deno.test("Doppler provider returns requested secrets", async () => {
  Deno.env.set("DOPPLER_TOKEN", "dp.st.test");
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (input: string | URL | Request) => {
    const url = typeof input === "string" ? input : input.toString();
    if (url.startsWith("https://api.doppler.com/v3/configs/config/secrets")) {
      return new Response(JSON.stringify({
        secrets: {
          PG_URL: { computed: "postgres://example.com/db" },
          REDIS_URL: { computed: "redis://example.com:6379" },
        },
      }), { status: 200 });
    }
    throw new Error("unexpected fetch: " + url);
  };

  let received: { value: Record<string, string>; opts: { secret: true } } | null = null;
  const sdk = {
    params: {
      get: async (k: string) => k === "requests"
        ? JSON.stringify([
          { name: "PG_URL", optional: false },
          { name: "REDIS_URL", optional: true },
          { name: "MISSING_OPT", optional: true },
        ])
        : null,
      all: async () => ({}),
    },
    output: Object.assign(
      async (value: Record<string, string>, opts: { secret: true }) => {
        received = { value, opts };
      },
      { html: async () => {}, text: async () => {}, image: async () => {}, file: async () => {} },
    ),
  } as unknown as Parameters<typeof main>[0];

  await main(sdk);

  globalThis.fetch = originalFetch;
  if (!received) throw new Error("output not called");
  assertEquals((received as any).opts.secret, true);
  assertEquals((received as any).value["PG_URL"], "postgres://example.com/db");
  assertEquals((received as any).value["REDIS_URL"], "redis://example.com:6379");
  assertEquals("MISSING_OPT" in (received as any).value, false); // optional miss is omitted
});

Deno.test("Doppler provider throws on required miss", async () => {
  Deno.env.set("DOPPLER_TOKEN", "dp.st.test");
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () => new Response(JSON.stringify({
    secrets: {}, // all missing
  }), { status: 200 });

  const sdk = {
    params: {
      get: async () => JSON.stringify([{ name: "PG_URL", optional: false }]),
      all: async () => ({}),
    },
    output: Object.assign(async () => {}, { html: async () => {}, text: async () => {}, image: async () => {}, file: async () => {} }),
  } as unknown as Parameters<typeof main>[0];

  await assertRejects(() => main(sdk), Error, "required secret PG_URL");
  globalThis.fetch = originalFetch;
});
```

- [ ] **Step 2: Run the test**

Run: `deno test tasks/buildin/secret-providers/doppler/task.test.ts --allow-net --allow-env`

Expected: 2 passed, 0 failed.

- [ ] **Step 3: Register the provider in buildin taskset**

Edit `tasks/buildin/taskset.yaml` and add (alphabetically near the auth- entries):

```yaml
    secret-providers/doppler:
      ref:
        path: ./secret-providers/doppler/task.yaml
```

- [ ] **Step 4: Run a smoke test loading the buildin source**

Run: `go test ./pkg/registry/... ./pkg/source/... -timeout 60s`

Expected: PASS — the new task.yaml parses and registers without errors.

- [ ] **Step 5: Commit**

```bash
git add tasks/buildin/secret-providers/doppler/task.test.ts tasks/buildin/taskset.yaml
git commit -m "test(buildin/doppler): mocked-HTTP provider test + taskset registration"
```

---

### Task 29: Document `from: task:` in concepts/secrets.md

**Files:**
- Modify: `docs/concepts/secrets.md`

- [ ] **Step 1: Replace the "Future providers" subsection (lines 56-79) with**

```markdown
### Future native providers

Native Go providers (vault, aws-secrets-manager, gcp-secret-manager) remain
on the roadmap but are not the recommended integration path. Most users
should reach for **provider tasks** instead — see below.

---

## Provider tasks (task: prefix)

For Doppler, 1Password, HashiCorp Vault, and other external secret stores,
dicode resolves secrets by spawning a normal task that calls the upstream
API. No daemon release is required to add a new provider — ship a task
folder and reference it from `from: task:<id>`.

### Consumer side — `from: task:<provider-id>`

```yaml
# tasks/my-app/task.yaml
name: "My App"
runtime: deno
trigger:
  cron: "*/5 * * * *"
permissions:
  env:
    - name: PG_URL
      from: task:secret-providers/doppler   # resolved via the doppler provider task
    - name: REDIS_URL
      from: task:secret-providers/doppler   # batched: one spawn for both
      optional: true
    - name: LOG_LEVEL
      from: env:LOG_LEVEL                   # explicit host env
```

The daemon groups every `from: task:<id>` entry by provider, spawns each
provider once per consumer launch, caches the result with the provider's
declared TTL, and merges the resolved values into the consumer's process
environment before launching it.

### Provider side — `dicode.output(map, { secret: true })`

A provider is any task that emits its resolved secrets via the secret-flag
overload of `output`:

```yaml
# tasks/buildin/secret-providers/doppler/task.yaml
name: "Doppler Secret Provider"
runtime: deno
trigger:
  manual: true
permissions:
  env:
    - name: DOPPLER_TOKEN
      secret: DOPPLER_TOKEN     # bootstrap once via `dicode secrets set DOPPLER_TOKEN ...`
  net:
    - api.doppler.com
provider:
  cache_ttl: 5m                  # 0 / omitted = no caching
```

```typescript
// task.ts
export default async function main({ params, output }: DicodeSdk) {
  const reqs = JSON.parse(await params.get("requests") ?? "[]");
  const out: Record<string, string> = {};
  // ... call upstream, populate `out` ...
  await output(out, { secret: true });
}
```

Daemon-side semantics for `secret: true`:

- Run-log records keys with `[redacted]` placeholders only — values never hit disk.
- Values feed the run-log redactor on the consumer launch (so a `console.log`
  of a resolved value is scrubbed).
- The map is returned to the resolver awaiting this provider call.
- Output must be a flat `Record<string, string>` — the daemon refuses
  nested objects.

### Failure modes

| Reason | When it fires |
|---|---|
| `provider_unavailable: <id>` | provider task crashed, timed out, or returned no map |
| `required_secret_missing: <KEY> from <id>` | provider returned a map but a non-optional KEY was absent |
| `provider_misconfigured: <id>` | task referenced via `task:<id>` is not a provider (missing `secret: true` flag) |

All three are recorded as the consumer run's `fail_reason` and trigger
the configured `on_failure_chain`. The provider's own run also fires its
own chain on its own crash.

### Built-in providers

| Path | Upstream | Bootstrap secret |
|---|---|---|
| `buildin/secret-providers/doppler` | Doppler REST API | `DOPPLER_TOKEN` |

More providers (`buildin/secret-providers/onepassword`, `vault`, …) ship
under the same path. Authoring your own takes a `task.yaml` + `task.ts`
pair — see [task-format](task-format.md).
```

- [ ] **Step 2: Commit**

```bash
git add docs/concepts/secrets.md
git commit -m "docs(secrets): document from: task: provider tasks + Doppler example"
```

---

## Self-Review (executed before final push)

**1. Spec coverage:**

| #119 acceptance bullet | Task(s) |
|---|---|
| `EnvEntry.From` parser supports `env:`/`task:` prefixes; bare = `env:` | 1, 2 |
| Reconciler validation: `task:` target must be a registered task | 4 |
| SDK Deno: `output(map, { secret: true })` accepted; flat-map enforced; values flow to redactor + provider channel | 16, 17, 20 |
| SDK Python: mirror Deno semantics | 19, 21 |
| IPC daemon-side: `secret: true` flag handled — feeds redactor, persists `[redacted]`, routes to resolver | 12, 13, 14, 15 |
| Resolver groups by provider, batches one spawn per provider per launch, caches with declared TTL | 5, 6, 7, 9, 10, 11, 22, 23 |
| Failure path: provider crash / required-key missing fails consumer launch with typed reason; both `on_failure_chain` events fire | 8, 24, 25 |
| Reference Doppler provider with mocked-HTTP integration test | 26, 27, 28 |
| Docs `concepts/secrets.md` updated | 29 |
| Existing `secret:` / bare `from:` usages unchanged | 22, 23 (test runs confirm); 10 (`TestResolve_BarePrefixIsHostEnv`) |

**2. Placeholder scan:** Searched for "TBD", "TODO", "later", "similar to", "fill in", "placeholder", "...". The `// ...` markers in the docs example (`docs/concepts/secrets.md` task.ts snippet) are intentional code-comment ellipses inside an illustrative snippet, not plan-level placeholders.

**3. Type / name consistency:**
- `parseFrom` (private) + `ParseFrom` (exported) — both used. Consistent.
- `FromKindEnv` / `FromKindTask` — used in Tasks 1, 4, 9.
- `envresolve.Resolver`, `envresolve.New`, `envresolve.ProviderRunner`, `envresolve.ProviderRequest`, `envresolve.ProviderResult`, `envresolve.Resolved` — defined Task 9, consumed Tasks 10, 11, 22, 23, 25.
- `envresolve.ErrProviderUnavailable` / `ErrRequiredSecretMissing` / `ErrProviderMisconfigured` — defined Task 8, consumed Tasks 9, 10, 11, 25.
- `cache` (lowercase, internal) with methods `get`/`put`/`bustProvider` — defined Task 6, consumed Task 9.
- `Server.SetSecretOutput(chan map[string]string)` — defined Task 13, consumed Tasks 20, 21.
- `Runtime.SetSecretOutputChannel` / `SetProviderRunner` — defined Tasks 20, 22 (Deno) and 21, 23 (Python), consumed Task 25.
- `Registry.FinishRunWithReason(ctx, runID, status, reason)` — defined Task 24, consumed Task 25.
- `Run.FailureReason` field — defined Task 24, surfaced via Task 25.
- `task.Spec.Provider *ProviderConfig` with `CacheTTL` — defined Task 7, consumed Tasks 9, 18, 26.
- IPC: `Request.Secret`, `Request.SecretMap` — defined Task 12, consumed Task 13.
- `CapOutputSecret` — defined Task 12, consumed Task 13 (gate inside `output`).

**Deferred items (intentional):**
- 1Password and HashiCorp Vault provider tasks — explicitly out of #119 scope; the Doppler provider serves as the reference, and follow-up issues are mentioned in #119 itself.
- Persistent (on-disk) cache — explicitly out of scope per #119.
- `IfMissing` deprecation in favor of `from: task:` — explicitly out of scope per #119.
- WebUI surfacing of typed `fail_reason` — the column lands in the DB but rendering it in the run-detail view is left to a follow-up alongside #116 (`parent_run_id` + `group` columns); the spec says "Once #116 lands, the UI can group these visually."

**Open questions for the implementing engineer:**
- Task 25 assumes `*Engine` already holds typed references to the Deno and Python runtimes. If the daemon currently passes them only via interfaces, add concrete fields in the daemon wiring file (`pkg/daemon/daemon.go`) and pass them through.
- Task 18's `task.Hash(spec.TaskDir)` is called per cache lookup; if a future profile shows it on the hot path, snapshot the hash once at provider-task registration via the reconciler instead.
