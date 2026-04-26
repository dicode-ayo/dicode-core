# Auth providers dashboard — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `buildin/auth-providers` task that surfaces OAuth connection state for every supported provider and orchestrates Connect via the existing `buildin/auth-start` task. Adds a permission-gated `dicode.oauth.list_status()` IPC primitive so the dashboard can introspect token metadata without ever touching plaintext credentials. Renames OpenRouter's stored secret to align with the OAuth naming convention.

**Architecture:** A new IPC handler `listOAuthStatus` reads `<P>_ACCESS_TOKEN`, `<P>_EXPIRES_AT`, `<P>_SCOPE`, `<P>_TOKEN_TYPE` via `secrets.Chain.Resolve` (env-fallback included) and returns metadata only — never plaintext. A new built-in webhook task ships an SPA at `/hooks/auth-providers` (built with Lit, mirroring `tasks/buildin/webui`'s pattern) that calls the primitive, renders cards per provider, and on Connect either dispatches `dicode.run_task("buildin/auth-start", { provider })` (14 broker-backed providers) or opens `/hooks/openrouter-oauth` (the one standalone). All wired through the existing trigger-engine SPA-static-asset path.

**Tech Stack:** Go 1.23 (daemon, IPC), Deno (task runtime, tests via `make test-tasks`), Lit 3 web components for the SPA (auto-escapes interpolated values), Playwright for e2e.

**Spec:** [`docs/superpowers/specs/2026-04-26-auth-providers-design.md`](../specs/2026-04-26-auth-providers-design.md)

**Worktree:** `/workspaces/dicode-core-worktrees/auth-providers-spec` on branch `docs/auth-providers-spec`. The implementation extends this branch.

---

## File structure

### New files

| Path | Responsibility |
|---|---|
| `pkg/ipc/oauth_status.go` | `listOAuthStatus` + `resolveOrEmpty` helpers. |
| `pkg/ipc/oauth_status_test.go` | Unit tests for the handler — empty, populated, malformed, oversize, plaintext-non-leakage. |
| `tasks/buildin/auth-providers/task.yaml` | Webhook spec, params, permissions. |
| `tasks/buildin/auth-providers/task.ts` | Action handler: `list` + `connect`. |
| `tasks/buildin/auth-providers/task.test.ts` | Deno unit tests. |
| `tasks/buildin/auth-providers/index.html` | SPA shell (auto-served by trigger engine). |
| `tasks/buildin/auth-providers/app/app.js` | SPA entry: mounts `<dc-providers-page>`. |
| `tasks/buildin/auth-providers/app/components/dc-providers-page.js` | Lit element: list + connect handler + 5 s polling. |
| `tasks/buildin/auth-providers/app/components/dc-provider-card.js` | Lit element for one provider card. |
| `tasks/buildin/auth-providers/app/lib/api.js` | Thin fetch wrapper with JSON helpers. |
| `tasks/buildin/auth-providers/app/theme.css` | Local copy of webui CSS variables. |
| `tests/e2e/auth-providers.spec.ts` | Playwright e2e against the live SPA. |

### Modified files

| Path | Change |
|---|---|
| `pkg/task/spec.go` | Add `OAuthStatus bool` to `DicodePermissions` (next to `OAuthInit` / `OAuthStore`). |
| `pkg/ipc/capability.go` | Add `CapOAuthStatus = "oauth.status"` constant. |
| `pkg/ipc/server.go` | Map `dp.OAuthStatus → CapOAuthStatus`; add `secretsChain` field + `SetSecretsChain` setter; add `dicode.oauth.list_status` dispatch case. |
| `pkg/ipc/message.go` | Add `Providers []string` request field for the new method. |
| `pkg/ipc/server_oauth_test.go` | Add a denial test (no `OAuthStatus` permission → method rejected) + happy-path test. |
| `pkg/runtime/deno/runtime.go` | Call `srv.SetSecretsChain(rt.secrets)` next to the existing `SetSecrets` call. |
| `pkg/runtime/deno/sdk/shim.ts` | Add `list_status` to the `dicode.oauth` shim. |
| `pkg/runtime/deno/sdk/sdk.d.ts` | Add typed `list_status` + `ProviderStatus` interface. |
| `tasks/buildin/taskset.yaml` | Register `auth-providers` entry. |
| `tasks/auth/openrouter-oauth/task.yaml` | `OPENROUTER_API_KEY` → `OPENROUTER_ACCESS_TOKEN` in `permissions.env` and `env`. |
| `tasks/auth/openrouter-oauth/task.ts` | Same rename in code (`SECRET_KEY` constant, log strings, downstream-doc example). |

---

## Phase 1 — Go primitive (TDD)

### Task 1: Add `OAuthStatus` permission field

**Files:**
- Modify: `pkg/task/spec.go:267-273`

- [ ] **Step 1: Open the file and add the new field**

Insert into the `DicodePermissions` struct, immediately after the existing `OAuthStore` field:

```go
	// OAuthStatus enables dicode.oauth.list_status().
	// Returns connection-state metadata (presence flag, expiry, scope) for the
	// provider names the caller passes — never plaintext tokens.
	OAuthStatus bool `yaml:"oauth_status,omitempty" json:"oauth_status,omitempty"`
```

- [ ] **Step 2: Confirm the package still builds**

Run: `cd /workspaces/dicode-core-worktrees/auth-providers-spec && go build ./pkg/task/...`
Expected: no output, exit 0.

- [ ] **Step 3: Commit**

```bash
git add pkg/task/spec.go
git commit -m "feat(task): add OAuthStatus permission field

Sibling of OAuthInit/OAuthStore. Gates the upcoming
dicode.oauth.list_status() IPC primitive."
```

---

### Task 2: Add `CapOAuthStatus` constant

**Files:**
- Modify: `pkg/ipc/capability.go:40-41`

- [ ] **Step 1: Add the constant**

Insert immediately below `CapOAuthStore` (which is at line 41):

```go
	CapOAuthStatus = "oauth.status"  // dicode.oauth.list_status — for the auth-providers built-in task
```

- [ ] **Step 2: Confirm package compiles**

Run: `go build ./pkg/ipc/...`
Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add pkg/ipc/capability.go
git commit -m "feat(ipc): add CapOAuthStatus capability constant"
```

---

### Task 3: Wire the new capability into the token claims

**Files:**
- Modify: `pkg/ipc/server.go:209-215`

- [ ] **Step 1: Add the OAuthStatus → CapOAuthStatus mapping**

Append immediately after the existing `OAuthStore` block (server.go:212-214). The full sub-block must read:

```go
		if dp.OAuthInit {
			caps = append(caps, CapOAuthInit)
		}
		if dp.OAuthStore {
			caps = append(caps, CapOAuthStore)
		}
		if dp.OAuthStatus {
			caps = append(caps, CapOAuthStatus)
		}
```

- [ ] **Step 2: Build**

Run: `go build ./pkg/ipc/...`
Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add pkg/ipc/server.go
git commit -m "feat(ipc): grant CapOAuthStatus when permissions.dicode.oauth_status is set"
```

---

### Task 4: Add `secretsChain` field + setter on the IPC server

**Files:**
- Modify: `pkg/ipc/server.go:55` (struct field), `pkg/ipc/server.go:135` (setter)

- [ ] **Step 1: Confirm `secrets` is already imported**

Run: `grep -n "github.com/dicode/dicode/pkg/secrets" pkg/ipc/server.go`
Expected: at least one match (the existing `secrets.Manager` field uses it).

- [ ] **Step 2: Add the field**

Insert immediately below the existing `secrets secrets.Manager` field at line 55:

```go
	// secretsChain (read path) is used by dicode.oauth.list_status to walk
	// the env-fallback chain. SetSecretsChain wires it; nil means the
	// daemon has no chain configured (tests with read-only flows).
	secretsChain secrets.Chain
```

- [ ] **Step 3: Add the setter**

Insert immediately below the existing `SetSecrets` method (at line 135):

```go
func (s *Server) SetSecretsChain(c secrets.Chain) { s.secretsChain = c }
```

- [ ] **Step 4: Build**

Run: `go build ./pkg/ipc/...`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add pkg/ipc/server.go
git commit -m "feat(ipc): add SetSecretsChain setter on Server

The IPC dispatcher needs read access to the secrets chain (with env
fallback) to serve dicode.oauth.list_status. Manager (write-only CRUD)
is insufficient on its own; both setters coexist."
```

---

### Task 5: Wire `SetSecretsChain` from the deno runtime

**Files:**
- Modify: `pkg/runtime/deno/runtime.go:236`

- [ ] **Step 1: Add the setter call**

Find this code in `runtime.go`:

```go
	srv := ipc.New(runID, spec.ID, rt.secret, rt.registry, rt.db, mergedParams, opts.Input, rt.log, spec, rt.engine)
	srv.SetGateway(rt.gateway)
	srv.SetSecrets(rt.secretsManager)
```

Insert one new line:

```go
	srv := ipc.New(runID, spec.ID, rt.secret, rt.registry, rt.db, mergedParams, opts.Input, rt.log, spec, rt.engine)
	srv.SetGateway(rt.gateway)
	srv.SetSecrets(rt.secretsManager)
	srv.SetSecretsChain(rt.secrets)
```

- [ ] **Step 2: Build**

Run: `go build ./pkg/runtime/...`
Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add pkg/runtime/deno/runtime.go
git commit -m "feat(runtime/deno): wire secrets.Chain into IPC server"
```

---

### Task 6: Add `Providers []string` to the IPC `Request` struct

**Files:**
- Modify: `pkg/ipc/message.go:68-71`

- [ ] **Step 1: Add the field**

Insert immediately after the existing `Envelope json.RawMessage` field at line 71:

```go
	Providers []string `json:"providers,omitempty"` // oauth.list_status — caller-supplied provider keys
```

The block must read:

```go
	// dicode.oauth.* — exposed only to the auth-relay/auth-start/auth-providers built-in tasks
	Provider  string          `json:"provider,omitempty"`  // oauth.build_auth_url
	Scope     string          `json:"scope,omitempty"`     // oauth.build_auth_url — optional scope override
	Envelope  json.RawMessage `json:"envelope,omitempty"`  // oauth.store_token — OAuthTokenDeliveryPayload JSON
	Providers []string        `json:"providers,omitempty"` // oauth.list_status — caller-supplied provider keys
```

(Update the leading comment to mention auth-providers as another consumer, exactly as shown.)

- [ ] **Step 2: Build**

Run: `go build ./pkg/ipc/...`
Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add pkg/ipc/message.go
git commit -m "feat(ipc): add Request.Providers field for oauth.list_status"
```

---

### Task 7: Write the failing test for `listOAuthStatus`

**Files:**
- Create: `pkg/ipc/oauth_status_test.go`

- [ ] **Step 1: Write the test file**

```go
package ipc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/secrets"
)

func TestListOAuthStatus_EmptyInput(t *testing.T) {
	chain := chainFromMem(newMemSecrets())

	out, err := listOAuthStatus(context.Background(), chain, []string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected empty result, got %d entries", len(out))
	}
}

func TestListOAuthStatus_FullBundle(t *testing.T) {
	ms := newMemSecrets()
	_ = ms.Set(context.Background(), "GITHUB_ACCESS_TOKEN", "ghp_xxx")
	_ = ms.Set(context.Background(), "GITHUB_EXPIRES_AT", "2026-12-31T00:00:00Z")
	_ = ms.Set(context.Background(), "GITHUB_SCOPE", "user repo")
	_ = ms.Set(context.Background(), "GITHUB_TOKEN_TYPE", "Bearer")
	chain := chainFromMem(ms)

	out, err := listOAuthStatus(context.Background(), chain, []string{"github"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out))
	}
	got := out[0]
	if got.Provider != "github" || !got.HasToken {
		t.Fatalf("provider/has_token wrong: %+v", got)
	}
	if got.ExpiresAt == nil || *got.ExpiresAt != "2026-12-31T00:00:00Z" {
		t.Fatalf("expires_at wrong: %v", got.ExpiresAt)
	}
	if got.Scope == nil || *got.Scope != "user repo" {
		t.Fatalf("scope wrong: %v", got.Scope)
	}
	if got.TokenType == nil || *got.TokenType != "Bearer" {
		t.Fatalf("token_type wrong: %v", got.TokenType)
	}
}

func TestListOAuthStatus_AccessTokenOnly(t *testing.T) {
	ms := newMemSecrets()
	_ = ms.Set(context.Background(), "OPENROUTER_ACCESS_TOKEN", "sk-or-xxx")
	chain := chainFromMem(ms)

	out, err := listOAuthStatus(context.Background(), chain, []string{"openrouter"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out[0]
	if !got.HasToken {
		t.Fatalf("expected has_token=true")
	}
	if got.ExpiresAt != nil || got.Scope != nil || got.TokenType != nil {
		t.Fatalf("expected metadata pointers nil; got %+v", got)
	}
}

func TestListOAuthStatus_NoTokenAtAll(t *testing.T) {
	chain := chainFromMem(newMemSecrets())

	out, err := listOAuthStatus(context.Background(), chain, []string{"slack"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out[0]
	if got.HasToken {
		t.Fatalf("expected has_token=false")
	}
}

func TestListOAuthStatus_MalformedName(t *testing.T) {
	chain := chainFromMem(newMemSecrets())

	for _, bad := range []string{"a", "_x", "x_", "X;Y", ""} {
		_, err := listOAuthStatus(context.Background(), chain, []string{bad})
		if err == nil {
			t.Fatalf("expected error for malformed %q, got nil", bad)
		}
	}
}

func TestListOAuthStatus_OversizeBatch(t *testing.T) {
	chain := chainFromMem(newMemSecrets())

	big := make([]string, maxStatusBatchSize+1)
	for i := range big {
		big[i] = "github"
	}
	_, err := listOAuthStatus(context.Background(), chain, big)
	if err == nil {
		t.Fatalf("expected oversize error, got nil")
	}
}

// TestListOAuthStatus_PlaintextNonLeakage confirms the access-token plaintext
// is read only to set HasToken and never appears in the marshalled response.
func TestListOAuthStatus_PlaintextNonLeakage(t *testing.T) {
	const sentinel = "SENTINEL_PLAINTEXT_TOKEN_aaaaaaaa"
	ms := newMemSecrets()
	_ = ms.Set(context.Background(), "FOO_ACCESS_TOKEN", sentinel)
	chain := chainFromMem(ms)

	out, err := listOAuthStatus(context.Background(), chain, []string{"foo"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), sentinel) {
		t.Fatalf("sentinel leaked into response: %s", body)
	}
	if !out[0].HasToken {
		t.Fatalf("expected has_token=true")
	}
}

// chainFromMem wraps memSecrets as a single-element secrets.Chain.
func chainFromMem(ms *memSecrets) secrets.Chain {
	return secrets.Chain{memProvider{ms}}
}

// memProvider adapts memSecrets to secrets.Provider (Get + Name).
type memProvider struct{ ms *memSecrets }

func (m memProvider) Name() string { return "memProvider" }
func (m memProvider) Get(ctx context.Context, key string) (string, error) {
	return m.ms.Get(ctx, key)
}
```

- [ ] **Step 2: Run the test — expect failure**

Run: `go test ./pkg/ipc/ -run TestListOAuthStatus -count=1`
Expected: FAIL with `undefined: listOAuthStatus` (the handler hasn't been written yet).

---

### Task 8: Implement `listOAuthStatus`

**Files:**
- Create: `pkg/ipc/oauth_status.go`

- [ ] **Step 1: Write the handler**

```go
package ipc

import (
	"context"
	"errors"
	"fmt"

	"github.com/dicode/dicode/pkg/secrets"
)

// maxStatusBatchSize bounds the per-call work of listOAuthStatus. Each entry
// performs up to four secret-store reads, so a hostile caller without this
// cap could amplify into an arbitrary number of provider lookups. The limit
// is generous enough to cover any realistic dashboard.
const maxStatusBatchSize = 64

// ProviderStatus is the per-provider response shape for dicode.oauth.list_status.
// HasToken is the only field derived from <P>_ACCESS_TOKEN — its plaintext
// is never surfaced to callers.
type ProviderStatus struct {
	Provider  string  `json:"provider"`             // lowercase, as supplied by the caller
	HasToken  bool    `json:"has_token"`
	ExpiresAt *string `json:"expires_at,omitempty"` // RFC3339 string or absent
	Scope     *string `json:"scope,omitempty"`
	TokenType *string `json:"token_type,omitempty"`
}

// listOAuthStatus reads OAuth status metadata for each provider key the caller
// supplies. Plaintext access/refresh tokens are never read into the response;
// only the presence flag and the three metadata strings (expiry, scope,
// token type) are surfaced.
//
// Each provider name passes through sanitizeProviderPrefix (shared with
// storeOAuthToken) so a malicious caller cannot escape into arbitrary
// secret-key namespaces.
func listOAuthStatus(ctx context.Context, chain secrets.Chain, providers []string) ([]ProviderStatus, error) {
	if len(providers) > maxStatusBatchSize {
		return nil, fmt.Errorf("too many providers: %d > %d", len(providers), maxStatusBatchSize)
	}
	out := make([]ProviderStatus, 0, len(providers))
	for _, p := range providers {
		prefix, err := sanitizeProviderPrefix(p)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", p, err)
		}
		access := resolveOrEmpty(ctx, chain, prefix+"_ACCESS_TOKEN")
		entry := ProviderStatus{
			Provider: p,
			HasToken: access != "",
		}
		if v := resolveOrEmpty(ctx, chain, prefix+"_EXPIRES_AT"); v != "" {
			entry.ExpiresAt = &v
		}
		if v := resolveOrEmpty(ctx, chain, prefix+"_SCOPE"); v != "" {
			entry.Scope = &v
		}
		if v := resolveOrEmpty(ctx, chain, prefix+"_TOKEN_TYPE"); v != "" {
			entry.TokenType = &v
		}
		out = append(out, entry)
	}
	return out, nil
}

// resolveOrEmpty wraps Chain.Resolve so a NotFoundError becomes empty string.
// Provider-error cases (network down, etc.) are also tolerated as empty for
// status-reporting purposes — the caller only needs presence/absence, and a
// transient backend hiccup should not fail the whole dashboard.
func resolveOrEmpty(ctx context.Context, chain secrets.Chain, key string) string {
	if chain == nil {
		return ""
	}
	v, err := chain.Resolve(ctx, key)
	if err != nil {
		var notFound *secrets.NotFoundError
		if errors.As(err, &notFound) {
			return ""
		}
		return ""
	}
	return v
}
```

- [ ] **Step 2: Run the tests — expect pass**

Run: `go test ./pkg/ipc/ -run TestListOAuthStatus -count=1 -v`
Expected: all 7 tests PASS.

- [ ] **Step 3: Commit**

```bash
git add pkg/ipc/oauth_status.go pkg/ipc/oauth_status_test.go
git commit -m "feat(ipc): add listOAuthStatus handler

Reads <PROVIDER>_ACCESS_TOKEN/_EXPIRES_AT/_SCOPE/_TOKEN_TYPE via the
secrets chain (env-fallback aware) and returns metadata only — never
the access-token plaintext. Provider names sanitised before lookup;
batch size capped at 64.

The 7 unit tests cover empty input, full bundle, access-token-only,
no-token, malformed name, oversize batch, and a sentinel-byte scan
that proves access-token plaintext never reaches the marshalled
response."
```

---

### Task 9: Add the `dicode.oauth.list_status` IPC dispatch case

**Files:**
- Modify: `pkg/ipc/server.go` — append a new `case` after the existing `dicode.oauth.store_token` case (around line 740).

- [ ] **Step 1: Find the insertion point**

Run: `grep -n 'case "dicode.oauth.store_token":' pkg/ipc/server.go`
Note the line number; the new case goes immediately after the `case` block ends — insert before the next `case` statement (or before the `default:` / `}` closing the outer switch).

- [ ] **Step 2: Insert the new dispatch case**

```go
		case "dicode.oauth.list_status":
			if !hasCap(caps, CapOAuthStatus) {
				reply(req.ID, nil, "ipc: permission denied (oauth.status)")
				continue
			}
			if s.secretsChain == nil {
				reply(req.ID, nil, "ipc: secrets chain not configured")
				continue
			}
			out, err := listOAuthStatus(context.Background(), s.secretsChain, req.Providers)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, out, "")
```

- [ ] **Step 3: Build**

Run: `go build ./pkg/ipc/...`
Expected: no output.

- [ ] **Step 4: Run all pkg/ipc tests**

Run: `go test ./pkg/ipc/ -count=1`
Expected: PASS (no existing tests should regress).

- [ ] **Step 5: Commit**

```bash
git add pkg/ipc/server.go
git commit -m "feat(ipc): dispatch dicode.oauth.list_status

Gated by CapOAuthStatus; refuses when secretsChain is unset; returns
the listOAuthStatus result as the reply payload."
```

---

### Task 10: Permission-denial + happy-path tests

**Files:**
- Modify: `pkg/ipc/server_oauth_test.go` — append two tests at the end.

- [ ] **Step 1: Add the test functions**

```go
// TestServer_OAuth_ListStatus_DeniedWithoutPermission proves the dispatcher
// rejects oauth.list_status when the calling spec lacks OAuthStatus, mirroring
// the existing OAuthInit/OAuthStore denial tests.
func TestServer_OAuth_ListStatus_DeniedWithoutPermission(t *testing.T) {
	env := newTestEnv(t)
	// Spec has OAuthStore (so something dicode.oauth.* is allowed) but NOT
	// OAuthStatus — list_status must be rejected.
	spec := specWithDicode("leaky", &task.DicodePermissions{OAuthStore: true})
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, spec.ID, env.secret, env.reg, env.db, nil, nil, zap.NewNop(), spec, nil)
	srv.SetSecretsChain(secrets.Chain{}) // empty chain is fine; the cap check fires first
	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)

	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	_ = doHandshake(t, conn, token)

	sendMsg(t, conn, map[string]any{
		"id":        "1",
		"method":    "dicode.oauth.list_status",
		"providers": []string{"github"},
	})
	resp := recvMsg(t, conn)
	errMsg, _ := resp["error"].(string)
	if errMsg == "" || !strings.Contains(errMsg, "permission denied") {
		t.Fatalf("expected permission denied error, got %v", resp)
	}
}

// TestServer_OAuth_ListStatus_HappyPath spins up a server with OAuthStatus
// granted and a populated chain; verifies a single provider returns has_token=true.
func TestServer_OAuth_ListStatus_HappyPath(t *testing.T) {
	env := newTestEnv(t)
	spec := specWithDicode("auth-providers", &task.DicodePermissions{OAuthStatus: true})
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, spec.ID, env.secret, env.reg, env.db, nil, nil, zap.NewNop(), spec, nil)

	ms := newMemSecrets()
	_ = ms.Set(context.Background(), "GITHUB_ACCESS_TOKEN", "x")
	srv.SetSecretsChain(secrets.Chain{memProvider{ms}})

	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)

	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	_ = doHandshake(t, conn, token)

	sendMsg(t, conn, map[string]any{
		"id":        "1",
		"method":    "dicode.oauth.list_status",
		"providers": []string{"github"},
	})
	resp := recvMsg(t, conn)
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	arr, ok := resp["result"].([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("bad result: %T %v", resp["result"], resp["result"])
	}
	first := arr[0].(map[string]any)
	if first["provider"] != "github" || first["has_token"] != true {
		t.Fatalf("bad entry: %+v", first)
	}
}
```

- [ ] **Step 2: Run the new tests**

Run: `go test ./pkg/ipc/ -run "TestServer_OAuth_ListStatus" -count=1 -v`
Expected: both new tests PASS. Then run the full pkg/ipc suite to ensure nothing regressed: `go test ./pkg/ipc/ -count=1`.

- [ ] **Step 3: Commit**

```bash
git add pkg/ipc/server_oauth_test.go
git commit -m "test(ipc): cover oauth.list_status permission gate + happy path"
```

---

## Phase 2 — SDK extension

### Task 11: Extend the Deno SDK type definitions

**Files:**
- Modify: `pkg/runtime/deno/sdk/sdk.d.ts` (around lines 35-53)

- [ ] **Step 1: Add `ProviderStatus` interface**

Insert immediately after the existing `OAuthStoreResult` interface (line 46):

```ts
declare interface ProviderStatus {
  provider:    string;
  has_token:   boolean;
  expires_at?: string;
  scope?:      string;
  token_type?: string;
}
```

- [ ] **Step 2: Update `DicodeOAuth` to add `list_status`**

Replace the existing `DicodeOAuth` interface (lines 48-53) with:

```ts
declare interface DicodeOAuth {
  /** Requires permissions.dicode.oauth_init. Signs the daemon's side of a /auth/:provider URL via the relay broker. */
  build_auth_url: (provider: string, scope?: string) => Promise<OAuthAuthURL>;
  /** Requires permissions.dicode.oauth_store. Decrypts an incoming token envelope and writes the resulting credentials to secrets. Plaintext never crosses the IPC boundary. */
  store_token:    (envelope: unknown)                => Promise<OAuthStoreResult>;
  /** Requires permissions.dicode.oauth_status. Returns connection-state metadata (presence, expiry, scope) for the provider names supplied. Plaintext access/refresh tokens are never returned. */
  list_status:    (providers: string[])              => Promise<ProviderStatus[]>;
}
```

- [ ] **Step 3: Commit**

```bash
git add pkg/runtime/deno/sdk/sdk.d.ts
git commit -m "feat(sdk/deno): type dicode.oauth.list_status + ProviderStatus"
```

---

### Task 12: Wire `list_status` into the SDK shim

**Files:**
- Modify: `pkg/runtime/deno/sdk/shim.ts`

- [ ] **Step 1: Add the runtime interface**

Insert immediately after the existing `OAuthStoreResult` interface (around line 79):

```ts
export interface ProviderStatus {
  provider:    string;
  has_token:   boolean;
  expires_at?: string;
  scope?:      string;
  token_type?: string;
}
```

- [ ] **Step 2: Update `DicodeOAuth` to include `list_status`**

Replace the existing `DicodeOAuth` interface (lines 85-88) with:

```ts
export interface DicodeOAuth {
  build_auth_url: (provider: string, scope?: string) => Promise<OAuthAuthURL>;
  store_token:    (envelope: unknown)                => Promise<OAuthStoreResult>;
  list_status:    (providers: string[])              => Promise<ProviderStatus[]>;
}
```

- [ ] **Step 3: Add the implementation in the `oauth:` block**

Replace the existing `oauth:` object inside `const dicode` (lines 256-261) with:

```ts
  oauth: {
    build_auth_url: (provider, scope) =>
      __call__({ method: "dicode.oauth.build_auth_url", provider, scope: scope ?? "" }) as Promise<OAuthAuthURL>,
    store_token: (envelope) =>
      __call__({ method: "dicode.oauth.store_token", envelope }) as Promise<OAuthStoreResult>,
    list_status: (providers) =>
      __call__({ method: "dicode.oauth.list_status", providers }) as Promise<ProviderStatus[]>,
  },
```

- [ ] **Step 4: Build the daemon (the shim is embedded into the binary)**

Run: `go build ./...`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add pkg/runtime/deno/sdk/shim.ts
git commit -m "feat(sdk/deno): implement dicode.oauth.list_status shim"
```

---

## Phase 3 — auth-providers task

### Task 13: Create `task.yaml`

**Files:**
- Create: `tasks/buildin/auth-providers/task.yaml`

- [ ] **Step 1: Write the file**

```yaml
apiVersion: dicode/v1
kind: Task
name: "Auth Providers"
description: |
  Dashboard listing every OAuth provider known to this dicode instance,
  with connection state and a Connect button. Connect for relay-broker
  providers (github, google, slack, ...) calls buildin/auth-start to
  obtain a signed /auth/:provider URL. Connect for OpenRouter (the only
  standalone PKCE provider) opens /hooks/openrouter-oauth directly,
  which renders an "Authorize with OpenRouter" page.

  Open in a browser via the Tasks list → Auth Providers → "open
  webhook UI" — or directly at /hooks/auth-providers.

runtime: deno

trigger:
  webhook: /hooks/auth-providers
  auth: true

params:
  providers:
    type: string
    default: "github,google,slack,spotify,linear,discord,gitlab,airtable,notion,confluence,salesforce,stripe,office365,azure,openrouter"
    description: |
      Comma-separated provider keys to display. Override to subset.
      Each key must satisfy [a-z0-9_]{2,}.

permissions:
  env:
    - DICODE_BASE_URL  # used to build the standalone openrouter URL
  dicode:
    oauth_status: true
    tasks:
      # Only auth-start is callable. The per-provider auth/<p>-oauth tasks
      # return HTML (not a JSON {url} contract) so they cannot be invoked
      # programmatically; auth-start is the canonical "give me a signed
      # /auth/:provider URL" task. OpenRouter is the standalone exception
      # and is handled with a direct webhook URL, not run_task.
      - "buildin/auth-start"

timeout: 30s

notify:
  on_success: false
  on_failure: false
```

- [ ] **Step 2: Commit**

```bash
git add tasks/buildin/auth-providers/task.yaml
git commit -m "feat(buildin/auth-providers): task.yaml

Webhook /hooks/auth-providers, auth: true, single allowed sub-task
buildin/auth-start, oauth_status permission for the dashboard read."
```

---

### Task 14: Write `task.test.ts` (failing tests first)

**Files:**
- Create: `tasks/buildin/auth-providers/task.test.ts`

- [ ] **Step 1: Write the test file**

```ts
/**
 * task.test.ts — unit tests for the Auth Providers dashboard task.
 *
 * Run with:  make test-tasks
 *
 * Each test() runs in its own isolated runtime; mocks (params, env,
 * dicode.*) reset between tests automatically.
 */
import { setupHarness } from "../../sdk-test.ts";
await setupHarness(import.meta.url);

test("list action returns merged status + meta for each provider", async () => {
  params.set("providers", "github,openrouter");

  let calledWith: string[] | null = null;
  dicode.oauth = {
    list_status: async (arr: string[]) => {
      calledWith = arr;
      return [
        { provider: "github",     has_token: true,  expires_at: "2026-12-31T00:00:00Z", scope: "user repo" },
        { provider: "openrouter", has_token: false },
      ];
    },
  };

  const result = await runTask() as Array<Record<string, unknown>>;

  assert.equal(JSON.stringify(calledWith), JSON.stringify(["github", "openrouter"]));
  assert.equal(result.length, 2);
  assert.equal(result[0].provider, "github");
  assert.equal(result[0].has_token, true);
  const meta0 = result[0].meta as Record<string, unknown>;
  assert.equal(meta0.label, "GitHub");
  const meta1 = result[1].meta as Record<string, unknown>;
  assert.equal(meta1.label, "OpenRouter");
  assert.ok((meta1.standalone as Record<string, unknown>)?.webhookPath);
});

test("connect for a relay-broker provider calls auth-start and returns its url", async () => {
  params.set("providers", "github");
  globalThis.input = { action: "connect", provider: "github" };

  const runTaskCalls: Array<{ id: string; params?: Record<string, string> }> = [];
  dicode.run_task = async (id: string, p?: Record<string, string>) => {
    runTaskCalls.push({ id, params: p });
    return { returnValue: { url: "https://relay.example/auth/github?...", session_id: "sess-1" } };
  };

  const result = await runTask() as Record<string, unknown>;

  assert.equal(runTaskCalls.length, 1);
  assert.equal(runTaskCalls[0].id, "buildin/auth-start");
  assert.equal(runTaskCalls[0].params?.provider, "github");
  assert.equal(result.provider, "github");
  assert.equal(result.url, "https://relay.example/auth/github?...");
  assert.equal(result.session_id, "sess-1");
});

test("connect for openrouter (standalone) does NOT call run_task", async () => {
  params.set("providers", "openrouter");
  globalThis.input = { action: "connect", provider: "openrouter" };
  env.set("DICODE_BASE_URL", "http://localhost:8080");

  let runTaskCalls = 0;
  dicode.run_task = async () => { runTaskCalls += 1; return {}; };

  const result = await runTask() as Record<string, unknown>;

  assert.equal(runTaskCalls, 0);
  assert.equal(result.provider, "openrouter");
  assert.equal(result.url, "http://localhost:8080/hooks/openrouter-oauth");
});

test("connect for an unknown provider throws", async () => {
  params.set("providers", "github");
  globalThis.input = { action: "connect", provider: "no-such-provider" };

  await assert.throws(() => runTask(), /unknown provider/);
});

test("connect when auth-start returns no url throws", async () => {
  params.set("providers", "github");
  globalThis.input = { action: "connect", provider: "github" };
  dicode.run_task = async () => ({ returnValue: {} });

  await assert.throws(() => runTask(), /did not return a url/);
});

test("empty providers param yields empty list (no list_status call)", async () => {
  params.set("providers", "");
  let called = 0;
  dicode.oauth = {
    list_status: async () => { called += 1; return []; },
  };

  const result = await runTask() as unknown[];

  assert.equal(called, 0);
  assert.equal(result.length, 0);
});

test("more than 64 providers throws before any IPC call", async () => {
  const big = Array.from({ length: 65 }, (_, i) => `p${i}`).join(",");
  params.set("providers", big);
  let called = 0;
  dicode.oauth = {
    list_status: async () => { called += 1; return []; },
  };

  await assert.throws(() => runTask(), /at most 64 providers/);
  assert.equal(called, 0);
});
```

- [ ] **Step 2: Run the tests — expect failure**

Run: `make test-tasks 2>&1 | grep -A2 -i auth-providers`
Expected: tests fail because `task.ts` does not exist.

---

### Task 15: Implement `task.ts` to satisfy the tests

**Files:**
- Create: `tasks/buildin/auth-providers/task.ts`

- [ ] **Step 1: Write the task script**

```ts
import type { DicodeSdk } from "../../sdk.ts";

interface ProviderMeta {
  key:        string;
  label:      string;
  color:      string;
  // standalone === true means the provider is NOT relay-broker-backed.
  // The Connect button opens the webhook URL directly; the per-provider
  // task renders an "Authorize with X" page. Currently only OpenRouter.
  standalone?: { webhookPath: string };
}

const KNOWN: ProviderMeta[] = [
  { key: "github",     label: "GitHub",     color: "#24292e" },
  { key: "google",     label: "Google",     color: "#4285f4" },
  { key: "slack",      label: "Slack",      color: "#4a154b" },
  { key: "spotify",    label: "Spotify",    color: "#1db954" },
  { key: "linear",     label: "Linear",     color: "#5e6ad2" },
  { key: "discord",    label: "Discord",    color: "#5865f2" },
  { key: "gitlab",     label: "GitLab",     color: "#fc6d26" },
  { key: "airtable",   label: "Airtable",   color: "#fcb400" },
  { key: "notion",     label: "Notion",     color: "#000000" },
  { key: "confluence", label: "Confluence", color: "#0052cc" },
  { key: "salesforce", label: "Salesforce", color: "#00a1e0" },
  { key: "stripe",     label: "Stripe",     color: "#635bff" },
  { key: "office365",  label: "Office365",  color: "#d83b01" },
  { key: "azure",      label: "Azure",      color: "#0078d4" },
  { key: "openrouter", label: "OpenRouter", color: "#6467f2",
    standalone: { webhookPath: "/hooks/openrouter-oauth" } },
];

const MAX_PROVIDERS = 64;

export default async function main({ params, input, dicode }: DicodeSdk) {
  const requested = ((await params.get("providers")) ?? "")
    .split(",").map(s => s.trim()).filter(Boolean);
  if (requested.length > MAX_PROVIDERS) {
    throw new Error(`at most ${MAX_PROVIDERS} providers`);
  }

  const inp = (input ?? null) as Record<string, unknown> | null;
  const action = (inp?.action ?? "list") as string;

  if (action === "list") {
    if (requested.length === 0) return [];
    const statuses = await dicode.oauth.list_status(requested);
    const meta = new Map(KNOWN.map(m => [m.key, m]));
    return statuses.map(s => ({ ...s, meta: meta.get(s.provider) ?? null }));
  }

  if (action === "connect") {
    const p = String(inp?.provider ?? "");
    const m = KNOWN.find(k => k.key === p);
    if (!m) throw new Error(`unknown provider: ${p}`);

    if (m.standalone) {
      const baseURL = (Deno.env.get("DICODE_BASE_URL") ?? "http://localhost:8080").replace(/\/$/, "");
      return { provider: p, url: `${baseURL}${m.standalone.webhookPath}` };
    }

    const run = await dicode.run_task("buildin/auth-start", { provider: p });
    const ret = (run as { returnValue?: { url?: string; session_id?: string } })?.returnValue;
    if (!ret?.url) throw new Error(`buildin/auth-start did not return a url for ${p}`);
    return { provider: p, url: ret.url, session_id: ret.session_id };
  }

  throw new Error(`unknown action: ${action}`);
}
```

- [ ] **Step 2: Run the tests — expect pass**

Run: `make test-tasks 2>&1 | tail -40`
Expected: all 7 new test cases under `tasks/buildin/auth-providers/task.test.ts` PASS. No regressions in other built-in task tests.

- [ ] **Step 3: Commit**

```bash
git add tasks/buildin/auth-providers/task.ts tasks/buildin/auth-providers/task.test.ts
git commit -m "feat(buildin/auth-providers): task.ts + unit tests

Implements list and connect actions. The 15-row KNOWN table is the
single source of provider metadata (label, color, standalone-ness).
Connect for the 14 broker-backed providers calls buildin/auth-start;
the OpenRouter card opens its task webhook directly."
```

---

### Task 16: Create the SPA shell `index.html`

**Files:**
- Create: `tasks/buildin/auth-providers/index.html`

- [ ] **Step 1: Write the file**

```html
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Auth Providers — dicode</title>
  <script>
    (function () {
      try {
        var stored = localStorage.getItem('dicode-theme');
        var theme =
          stored === 'light' || stored === 'dark'
            ? stored
            : (window.matchMedia && window.matchMedia('(prefers-color-scheme: light)').matches)
              ? 'light'
              : 'dark';
        document.documentElement.setAttribute('data-theme', theme);
        document.documentElement.style.colorScheme = theme;
      } catch (e) {
        document.documentElement.setAttribute('data-theme', 'dark');
      }
    })();
  </script>
  <link rel="stylesheet" href="app/theme.css">
</head>
<body>
  <dc-providers-page></dc-providers-page>
  <script type="module" src="app/app.js"></script>
</body>
</html>
```

- [ ] **Step 2: Commit**

```bash
git add tasks/buildin/auth-providers/index.html
git commit -m "feat(buildin/auth-providers): index.html SPA shell"
```

---

### Task 17: Create `theme.css`

**Files:**
- Create: `tasks/buildin/auth-providers/app/theme.css`

- [ ] **Step 1: Write the file**

```css
/*
 * theme.css — local copy of the webui theme tokens so the auth-providers
 * page can stand alone (no shared asset path between built-in tasks).
 */
:root[data-theme="dark"] {
  --bg:        #0f1115;
  --surface:   #181b22;
  --border:    #2a2e38;
  --text:      #e6e8ee;
  --muted:     #a1a8b6;
  --accent:    #4f8cff;
  --pill-ok:   #1a7f37;
  --pill-warn: #b88200;
  --pill-err:  #b3261e;
  --space-sm:  .5rem;
  --space-md:  1rem;
}

:root[data-theme="light"] {
  --bg:        #ffffff;
  --surface:   #f6f7f9;
  --border:    #d8dde5;
  --text:      #14171c;
  --muted:     #5a6473;
  --accent:    #0a66ff;
  --pill-ok:   #1a7f37;
  --pill-warn: #b88200;
  --pill-err:  #b3261e;
  --space-sm:  .5rem;
  --space-md:  1rem;
}

html, body {
  margin: 0;
  padding: 0;
  font-family: system-ui, -apple-system, "Segoe UI", sans-serif;
  background: var(--bg);
  color: var(--text);
}

dc-providers-page {
  display: block;
  padding: 1.5rem 2rem;
}

dc-provider-card {
  display: block;
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 1rem;
}

.providers-grid {
  list-style: none;
  margin: 0;
  padding: 0;
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
  gap: 1rem;
}

.pill {
  display: inline-block;
  color: white;
  border-radius: 999px;
  padding: .1rem .55rem;
  font-size: .75em;
  font-weight: 600;
  margin-left: .5rem;
}

.color-dot {
  display: inline-block;
  width: 14px;
  height: 14px;
  border-radius: 50%;
  vertical-align: middle;
  margin-right: .35rem;
}

.btn {
  background: var(--accent);
  color: white;
  border: 0;
  border-radius: 6px;
  padding: .4rem .8rem;
  cursor: pointer;
  font-weight: 600;
}
.btn:hover { filter: brightness(1.1); }
```

- [ ] **Step 2: Commit**

```bash
git add tasks/buildin/auth-providers/app/theme.css
git commit -m "feat(buildin/auth-providers): theme.css

Local copy of the webui theme tokens; data-theme on <html> drives
light/dark."
```

---

### Task 18: Create `app/lib/api.js`

**Files:**
- Create: `tasks/buildin/auth-providers/app/lib/api.js`

- [ ] **Step 1: Write the file**

```js
// api.js — thin wrappers around the auth-providers webhook endpoint.
//   - list()                       → GET  ?action=list  → ProviderRow[]
//   - connect(provider)            → POST { action: "connect", provider }
//                                    → { provider, url, session_id? }
// Errors throw with the daemon's error string.

const ENDPOINT = window.location.pathname.replace(/\/$/, "") || "/hooks/auth-providers";

async function fetchJson(method, body) {
  const url = method === "GET"
    ? `${ENDPOINT}?action=${encodeURIComponent(body.action)}`
    : ENDPOINT;
  const init = {
    method,
    headers: { "Accept": "application/json" },
  };
  if (method === "POST") {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  const res = await fetch(url, init);
  if (!res.ok) {
    const text = await res.text().catch(() => "");
    throw new Error(`HTTP ${res.status}: ${text || res.statusText}`);
  }
  // The trigger engine wraps task return values in { result, ... }; the
  // task return is the value we want.
  const envelope = await res.json();
  return envelope?.result ?? envelope;
}

export const api = {
  list:    ()                => fetchJson("GET",  { action: "list" }),
  connect: (provider)        => fetchJson("POST", { action: "connect", provider }),
};
```

- [ ] **Step 2: Commit**

```bash
git add tasks/buildin/auth-providers/app/lib/api.js
git commit -m "feat(buildin/auth-providers): api.js fetch wrapper"
```

---

### Task 19: Create `dc-provider-card.js` (Lit element)

**Files:**
- Create: `tasks/buildin/auth-providers/app/components/dc-provider-card.js`

- [ ] **Step 1: Write the file**

```js
import { LitElement, html } from "https://esm.sh/lit@3";

// dc-provider-card — one row in the providers list. Renders label,
// color dot, status pill, expiry, scope, and a Connect/Reconnect button.
// On Connect click, dispatches a "connect" CustomEvent with detail.provider.
//
// Lit's html`` template literal auto-escapes interpolated values, so
// arbitrary strings inside ${row.scope}, ${meta.label}, etc. cannot
// inject HTML or script content.
class DcProviderCard extends LitElement {
  // Render into the light DOM so theme.css variables apply directly.
  createRenderRoot() { return this; }

  static properties = {
    row:    { attribute: false },
    error:  { state: true },
  };

  constructor() {
    super();
    this.row = null;
    this.error = "";
  }

  setError(msg) { this.error = String(msg || ""); }

  _onConnect() {
    this.error = "";
    this.dispatchEvent(new CustomEvent("connect", {
      bubbles: true,
      detail: { provider: this.row?.provider },
    }));
  }

  _pill(row) {
    if (!row.has_token) {
      return html`<span class="pill" style="background:var(--pill-err)">Not connected</span>`;
    }
    if (!row.expires_at) {
      return html`<span class="pill" style="background:var(--pill-ok)">Connected</span>`;
    }
    const ms = Date.parse(row.expires_at) - Date.now();
    if (Number.isNaN(ms)) {
      return html`<span class="pill" style="background:var(--pill-ok)">Connected</span>`;
    }
    if (ms <= 0) {
      return html`<span class="pill" style="background:var(--pill-err)">Expired</span>`;
    }
    const color = ms < 24 * 3600_000 ? "var(--pill-warn)" : "var(--pill-ok)";
    return html`<span class="pill" style="background:${color}">Expires ${humanDelta(ms)}</span>`;
  }

  render() {
    const row = this.row;
    if (!row) return html``;
    const meta = row.meta || { label: row.provider, color: "#888" };
    const buttonLabel = row.has_token ? "Reconnect" : "Connect";
    return html`
      <div style="display:flex;align-items:center;gap:.5rem;margin-bottom:.5rem">
        <span class="color-dot" style="background:${meta.color}"></span>
        <strong>${meta.label}</strong>
        ${this._pill(row)}
      </div>
      ${row.scope ? html`
        <p style="color:var(--muted);margin:.25rem 0;font-size:.85em">scope: <code>${row.scope}</code></p>
      ` : ""}
      <button class="btn" @click=${() => this._onConnect()}>${buttonLabel}</button>
      ${this.error ? html`
        <p style="color:var(--pill-err);font-size:.85em;margin:.5rem 0 0">${this.error}</p>
      ` : ""}
    `;
  }
}

function humanDelta(ms) {
  const sec = Math.floor(ms / 1000);
  if (sec < 60)    return `in ${sec}s`;
  if (sec < 3600)  return `in ${Math.floor(sec / 60)}m`;
  if (sec < 86400) return `in ${Math.floor(sec / 3600)}h`;
  return `in ${Math.floor(sec / 86400)}d`;
}

customElements.define("dc-provider-card", DcProviderCard);
```

- [ ] **Step 2: Commit**

```bash
git add tasks/buildin/auth-providers/app/components/dc-provider-card.js
git commit -m "feat(buildin/auth-providers): dc-provider-card Lit element

Renders one provider row with status pill, expiry-based color
(green/yellow/red), and a Connect/Reconnect button that dispatches a
'connect' custom event. Lit's html template auto-escapes interpolated
values; the static color and meta values come from a hardcoded
KNOWN table in the task."
```

---

### Task 20: Create `dc-providers-page.js` (page-level Lit element)

**Files:**
- Create: `tasks/buildin/auth-providers/app/components/dc-providers-page.js`

- [ ] **Step 1: Write the file**

```js
import { LitElement, html } from "https://esm.sh/lit@3";
import { api } from "../lib/api.js";
import "./dc-provider-card.js";

const POLL_INTERVAL_MS = 5_000;

class DcProvidersPage extends LitElement {
  createRenderRoot() { return this; }

  static properties = {
    _rows:   { state: true },
    _status: { state: true },
  };

  constructor() {
    super();
    this._rows = null;
    this._status = "loading";
    this._timer = null;
  }

  connectedCallback() {
    super.connectedCallback();
    this._refresh();
    this._timer = setInterval(() => this._refresh(), POLL_INTERVAL_MS);
    this.addEventListener("connect", (e) => this._onConnect(e));
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    if (this._timer) clearInterval(this._timer);
  }

  async _refresh() {
    try {
      const rows = await api.list();
      this._rows = Array.isArray(rows) ? rows : [];
      this._status = "ready";
    } catch (err) {
      this._status = `error: ${err.message || err}`;
    }
  }

  async _onConnect(e) {
    const provider = e.detail?.provider;
    if (!provider) return;
    const card = e.target;
    try {
      const out = await api.connect(provider);
      if (!out?.url) throw new Error("provider task did not return a url");
      window.open(out.url, "_blank", "noopener");
    } catch (err) {
      if (typeof card.setError === "function") {
        card.setError(err.message || String(err));
      }
    }
  }

  _renderBody() {
    if (this._status === "loading") {
      return html`<p style="color:var(--muted)">Loading…</p>`;
    }
    if (this._status.startsWith("error:")) {
      return html`<p style="color:var(--pill-err)">${this._status}</p>`;
    }
    const rows = this._rows ?? [];
    if (rows.length === 0) {
      return html`<p style="color:var(--muted)">No providers configured. Set the <code>providers</code> param on this task to enable some.</p>`;
    }
    return html`
      <ul class="providers-grid">
        ${rows.map(row => html`
          <li><dc-provider-card .row=${row}></dc-provider-card></li>
        `)}
      </ul>
    `;
  }

  render() {
    return html`
      <header>
        <h1>OAuth providers</h1>
        <p style="color:var(--muted);margin:0;max-width:640px">
          Click <strong>Connect</strong> on a provider to start an authorisation flow.
          The card flips to <strong>Connected</strong> automatically once the token lands in the secrets store.
        </p>
      </header>
      <main style="margin-top:1.25rem">
        ${this._renderBody()}
      </main>
    `;
  }
}

customElements.define("dc-providers-page", DcProvidersPage);
```

- [ ] **Step 2: Commit**

```bash
git add tasks/buildin/auth-providers/app/components/dc-providers-page.js
git commit -m "feat(buildin/auth-providers): dc-providers-page

Page-level Lit element: fetches the provider list, renders dc-provider-card
per row, polls every 5 s, and on Connect opens the auth URL in a new tab.
Errors surface inline on the originating card via setError()."
```

---

### Task 21: Create `app.js` entry

**Files:**
- Create: `tasks/buildin/auth-providers/app/app.js`

- [ ] **Step 1: Write the file**

```js
// app.js — the only purpose of this module is to import the page
// component, which registers the custom element and triggers all
// downstream imports (api.js, dc-provider-card.js).
import "./components/dc-providers-page.js";
```

- [ ] **Step 2: Commit**

```bash
git add tasks/buildin/auth-providers/app/app.js
git commit -m "feat(buildin/auth-providers): app.js entry"
```

---

### Task 22: Register the task in the buildin taskset

**Files:**
- Modify: `tasks/buildin/taskset.yaml`

- [ ] **Step 1: Append the entry**

Insert at the end of the `spec.entries` map (after the last existing entry — `auth-relay`):

```yaml
    auth-providers:
      ref:
        path: ./auth-providers/task.yaml
```

- [ ] **Step 2: Smoke check that the daemon parses it**

Run: `make build`
Expected: builds with no errors. (The taskset is parsed at daemon boot, not at compile time, so this only confirms the file syntax is valid YAML — the e2e in Task 24 actually exercises load + serve.)

- [ ] **Step 3: Commit**

```bash
git add tasks/buildin/taskset.yaml
git commit -m "feat(buildin): register auth-providers task"
```

---

## Phase 4 — OpenRouter rename

### Task 23: Rename `OPENROUTER_API_KEY` → `OPENROUTER_ACCESS_TOKEN`

**Files:**
- Modify: `tasks/auth/openrouter-oauth/task.ts`
- Modify: `tasks/auth/openrouter-oauth/task.yaml`

- [ ] **Step 1: Rename the constant in `task.ts`**

Open `tasks/auth/openrouter-oauth/task.ts`. Find:

```ts
const SECRET_KEY       = "OPENROUTER_API_KEY";
```

Replace with:

```ts
const SECRET_KEY       = "OPENROUTER_ACCESS_TOKEN";
```

- [ ] **Step 2: Update the documented downstream-usage example**

In the same file (or in the description block of `task.yaml`), find the documented example referencing `OPENROUTER_API_KEY`:

```ts
// Use the stored key in other tasks via:
//     env: [{ name: OPENAI_API_KEY, secret: OPENROUTER_API_KEY }]
```

Replace with:

```ts
// Use the stored key in other tasks via:
//     env: [{ name: OPENAI_API_KEY, secret: OPENROUTER_ACCESS_TOKEN }]
```

Also update any `console.log("[OpenRouter] stored …")` lines or other references to the old name in this file.

- [ ] **Step 3: Update `task.yaml`**

Replace `OPENROUTER_API_KEY` with `OPENROUTER_ACCESS_TOKEN` in:
- the `permissions.env` entry
- the `env` block (the optional secret declaration)
- the description prose, if it mentions the old name

Run a sanity grep:

```bash
grep -rn "OPENROUTER_API_KEY" tasks/auth/openrouter-oauth/
```

Expected: empty after the rename.

- [ ] **Step 4: Run any openrouter task tests if they exist**

Run: `make test-tasks 2>&1 | grep -A2 -i openrouter`
If there are no openrouter tests, skip; the e2e (Task 24) covers the rename indirectly.

- [ ] **Step 5: Commit**

```bash
git add tasks/auth/openrouter-oauth/
git commit -m "refactor(openrouter-oauth): rename OPENROUTER_API_KEY → OPENROUTER_ACCESS_TOKEN

Aligns the standalone OpenRouter PKCE task with the broker-delivered
provider naming convention used by buildin/auth-relay (<P>_ACCESS_TOKEN).
This makes dicode.oauth.list_status report a uniform secret-key shape
across all 15 providers; the auth-providers dashboard sees OpenRouter
identically to GitHub/Slack/etc.

No backwards-compat shim — alpha (v0.1.0) has no production users
relying on the old name."
```

---

## Phase 5 — End-to-end test

### Task 24: Playwright e2e

**Files:**
- Create: `tests/e2e/auth-providers.spec.ts`

- [ ] **Step 1: Write the spec**

```ts
/**
 * auth-providers.spec.ts — e2e tests for the buildin/auth-providers
 * dashboard task.
 */
import { test, expect } from '@playwright/test';
import { TEST_PASSPHRASE, login } from './helpers/auth';

test.describe('Auth Providers dashboard', () => {

  test('GET /hooks/auth-providers serves index.html with SDK injected', async ({ request }) => {
    await login(request, TEST_PASSPHRASE);
    const res = await request.get('/hooks/auth-providers');
    expect(res.ok()).toBe(true);
    const html = await res.text();
    expect(html).toContain('<title>Auth Providers');
    // The dicode.js SDK is auto-injected by the trigger engine.
    expect(html.toLowerCase()).toContain('dicode');
  });

  test('list action returns provider rows with has_token false by default', async ({ request }) => {
    await login(request, TEST_PASSPHRASE);
    const res = await request.get('/hooks/auth-providers?action=list');
    expect(res.ok()).toBe(true);
    const body = await res.json() as { result: Array<Record<string, unknown>> } | Array<Record<string, unknown>>;
    const rows = Array.isArray(body) ? body : body.result;
    expect(Array.isArray(rows)).toBe(true);
    expect(rows.length).toBeGreaterThan(0);
    for (const row of rows) {
      expect(row.has_token).toBe(false);
    }
    const openrouter = rows.find(r => r.provider === 'openrouter');
    expect(openrouter).toBeDefined();
  });

  test('list reports has_token=true when an ACCESS_TOKEN secret is set', async ({ request }) => {
    await login(request, TEST_PASSPHRASE);
    const setRes = await request.post('/api/secrets', {
      headers: { 'Content-Type': 'application/json' },
      data: { key: 'OPENROUTER_ACCESS_TOKEN', value: 'sk-or-test-12345' },
    });
    expect(setRes.ok()).toBe(true);

    const listRes = await request.get('/hooks/auth-providers?action=list');
    expect(listRes.ok()).toBe(true);
    const body = await listRes.json() as { result: Array<Record<string, unknown>> } | Array<Record<string, unknown>>;
    const rows = Array.isArray(body) ? body : body.result;
    const openrouter = rows.find(r => r.provider === 'openrouter');
    expect(openrouter?.has_token).toBe(true);

    // Cleanup so subsequent tests are not polluted.
    await request.delete('/api/secrets/OPENROUTER_ACCESS_TOKEN');
  });

  test('connect with standalone openrouter returns the webhook URL', async ({ request }) => {
    await login(request, TEST_PASSPHRASE);
    const res = await request.post('/hooks/auth-providers', {
      headers: { 'Content-Type': 'application/json' },
      data: { action: 'connect', provider: 'openrouter' },
    });
    expect(res.ok()).toBe(true);
    const body = await res.json() as { result: { url?: string } } | { url?: string };
    const out = (body as { result: { url?: string } }).result ?? body as { url?: string };
    expect(out.url).toContain('/hooks/openrouter-oauth');
  });

  test('connect with unknown provider returns 5xx with error message', async ({ request }) => {
    await login(request, TEST_PASSPHRASE);
    const res = await request.post('/hooks/auth-providers', {
      headers: { 'Content-Type': 'application/json' },
      data: { action: 'connect', provider: 'no-such-provider' },
    });
    expect(res.ok()).toBe(false);
    const text = await res.text();
    expect(text.toLowerCase()).toContain('unknown provider');
  });
});
```

- [ ] **Step 2: Run the e2e**

Run: `npx playwright test tests/e2e/auth-providers.spec.ts --reporter=list`
Expected: all 5 cases PASS.

- [ ] **Step 3: Commit**

```bash
git add tests/e2e/auth-providers.spec.ts
git commit -m "test(e2e): playwright suite for buildin/auth-providers"
```

---

## Phase 6 — Final integration & PR

### Task 25: Run the full test suite

- [ ] **Step 1: Go tests**

Run: `make test`
Expected: all packages PASS.

- [ ] **Step 2: Lint**

Run: `make lint`
Expected: no warnings.

- [ ] **Step 3: Deno task tests**

Run: `make test-tasks`
Expected: all tests PASS.

- [ ] **Step 4: Playwright e2e**

Run: `npx playwright test --reporter=list`
Expected: all suites PASS.

- [ ] **Step 5: Build**

Run: `make build`
Expected: produces `./dicode` with no errors.

If any step fails, stop and fix before opening the PR.

---

### Task 26: Push branch and open the PR

- [ ] **Step 1: Push**

```bash
git push -u origin docs/auth-providers-spec
```

- [ ] **Step 2: Open the PR via gh**

```bash
gh pr create --title "feat(buildin): auth-providers dashboard + dicode.oauth.list_status" --body "$(cat <<'EOF'
## Summary
- New built-in webhook task **buildin/auth-providers** at `/hooks/auth-providers`: dashboard listing 15 OAuth providers with connection state, expiry, scope, and a Connect button that runs `buildin/auth-start` (or opens `/hooks/openrouter-oauth` for the standalone OpenRouter flow).
- New permission-gated IPC primitive **`dicode.oauth.list_status(providers)`** that reads `<P>_ACCESS_TOKEN`/`_EXPIRES_AT`/`_SCOPE`/`_TOKEN_TYPE` via the secrets chain and returns metadata only — never plaintext credentials.
- OpenRouter rename: `OPENROUTER_API_KEY` → `OPENROUTER_ACCESS_TOKEN` for naming-convention parity with the broker-delivered providers.
- Spec: docs/superpowers/specs/2026-04-26-auth-providers-design.md. Follow-up (generic "task contributes a webui sub-page" mechanism) tracked at docs/followups/auth-providers-webui-nav.md.

## Test plan
- [ ] `make test` — Go unit tests, including new pkg/ipc/oauth_status_test.go (7 cases) + extended pkg/ipc/server_oauth_test.go (denial + happy-path)
- [ ] `make lint` — clean
- [ ] `make test-tasks` — Deno task tests, including new tasks/buildin/auth-providers/task.test.ts (7 cases)
- [ ] `npx playwright test tests/e2e/auth-providers.spec.ts` — 5 e2e cases
- [ ] Manual smoke: boot daemon, navigate to /hooks/auth-providers, verify all 15 cards render and a Connect click on OpenRouter opens the right URL in a new tab

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

- [ ] **Step 3: Capture the PR URL** for the review cycle below.

---

### Task 27: Review cycle — `/review` + `/security-review` until clean

Per project convention (memory: PR review loop): after `gh pr create`, run `/review` and `/security-review` against the new PR and iterate inline until both reviewers report no remaining concerns.

- [ ] **Step 1: Dispatch `/review`**

For each inline comment, decide: address it inline (push a fix commit to the same branch — never force-push) or push back with rationale.

- [ ] **Step 2: Dispatch `/security-review`**

Particular focus areas (already called out in the spec):
1. `listOAuthStatus` never marshals access-token plaintext — Task 7's plaintext-non-leakage test is the explicit assertion.
2. `sanitizeProviderPrefix` rejects malformed names before any secret read — Task 7's malformed-name test covers.
3. `permissions.dicode.oauth_status` defaults to denied and is checked at dispatch — Task 10's denial test covers.
4. `tasks/buildin/auth-providers/task.yaml` declares `trigger.auth: true`.
5. The Connect flow's `run_task` is constrained to a single allowed target — `buildin/auth-start`.

- [ ] **Step 3: Iterate** until both reviewers are clean.

- [ ] **Step 4: Stop and request human approval** before merging.

Per project memory: "PR merges require explicit approval — never merge PRs without a clear go-ahead from the user in the current conversation."

---

## Self-review (post-plan)

The following gaps were checked against the spec. None remain open.

| Spec section | Plan task(s) |
|---|---|
| `pkg/ipc/oauth_status.go` (new handler) | Tasks 7, 8 |
| `pkg/ipc/oauth_status_test.go` (7 unit cases) | Task 7 |
| Permission gate `OAuthStatus` field | Task 1 |
| `CapOAuthStatus` constant + capability mapping | Tasks 2, 3 |
| `secretsChain` field + `SetSecretsChain` setter | Task 4 |
| Wiring chain from deno runtime | Task 5 |
| `Request.Providers` field | Task 6 |
| IPC dispatch case for `dicode.oauth.list_status` | Task 9 |
| Permission-denial + happy-path tests | Task 10 |
| SDK shim + d.ts | Tasks 11, 12 |
| `tasks/buildin/auth-providers/{task.yaml,task.ts,task.test.ts}` | Tasks 13, 14, 15 |
| `index.html` + `app/{theme.css, lib/api.js, components/*, app.js}` | Tasks 16-21 |
| Register in `tasks/buildin/taskset.yaml` | Task 22 |
| OpenRouter rename | Task 23 |
| Playwright e2e | Task 24 |
| Final test gate | Task 25 |
| PR + review cycle | Tasks 26, 27 |

Type/method consistency check: `dicode.run_task` in Task 15 → `RunResult.returnValue` matches what `pkg/ipc/message.go:165` defines and what `buildin/auth-start/task.ts` actually returns. `secrets.Chain` in Tasks 4, 5, 7, 8, 9, 10 is consistent. `MAX_PROVIDERS = 64` (task.ts) matches `maxStatusBatchSize = 64` (oauth_status.go) — both sides reject the same boundary.

No "TBD", "implement later", or "similar to Task N" placeholders remain in the plan body.
