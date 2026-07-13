package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/db"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
)

// newTestAPIKeyStore builds an apiKeyStore over a fresh in-memory SQLite DB.
func newTestAPIKeyStore(t *testing.T) *apiKeyStore {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return newAPIKeyStore(d)
}

// TestMCPTokenMinter_MintValidateRevoke covers the full lifecycle: a minted
// token validates, and after Revoke it no longer does.
func TestMCPTokenMinter_MintValidateRevoke(t *testing.T) {
	ctx := context.Background()
	keys := newTestAPIKeyStore(t)
	minter := newMCPTokenMinter(keys)

	token, err := minter.Mint(ctx, "run-123", pkgruntime.MCPScope{ListTasks: true, RunTaskIDs: []string{"*"}})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if token == "" {
		t.Fatal("Mint returned empty token")
	}
	if !keys.validate(ctx, token) {
		t.Fatal("minted token does not validate")
	}

	if err := minter.Revoke(ctx, "run-123"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if keys.validate(ctx, token) {
		t.Fatal("token still validates after Revoke")
	}
}

// TestMCPTokenMinter_RevokeIdempotent covers revoking a run ID that was
// never minted (e.g. Mint failed before Revoke ran) or already revoked.
func TestMCPTokenMinter_RevokeIdempotent(t *testing.T) {
	ctx := context.Background()
	minter := newMCPTokenMinter(newTestAPIKeyStore(t))
	if err := minter.Revoke(ctx, "never-minted"); err != nil {
		t.Fatalf("Revoke on unminted run ID should be a no-op, got: %v", err)
	}
}

// TestRevokeByNamePrefix_SweepsEphemeralOnly covers the startup-sweep
// primitive: it must revoke every ephemeral/run/* key and leave CLI-managed
// and operator (dashboard) keys untouched.
func TestRevokeByNamePrefix_SweepsEphemeralOnly(t *testing.T) {
	ctx := context.Background()
	keys := newTestAPIKeyStore(t)

	ephemeralA, _, err := keys.generate(ctx, ephemeralKeyPrefix+"run-a")
	if err != nil {
		t.Fatalf("generate ephemeral A: %v", err)
	}
	ephemeralB, _, err := keys.generate(ctx, ephemeralKeyPrefix+"run-b")
	if err != nil {
		t.Fatalf("generate ephemeral B: %v", err)
	}
	cliKey, _, err := keys.generate(ctx, cliManagedKeyPrefix+"laptop")
	if err != nil {
		t.Fatalf("generate cli key: %v", err)
	}
	dashKey, _, err := keys.generate(ctx, "my dashboard key")
	if err != nil {
		t.Fatalf("generate dashboard key: %v", err)
	}

	if err := keys.revokeByNamePrefix(ctx, ephemeralKeyPrefix); err != nil {
		t.Fatalf("revokeByNamePrefix: %v", err)
	}

	if keys.validate(ctx, ephemeralA) {
		t.Error("ephemeral key A still validates after sweep")
	}
	if keys.validate(ctx, ephemeralB) {
		t.Error("ephemeral key B still validates after sweep")
	}
	if !keys.validate(ctx, cliKey) {
		t.Error("CLI-managed key was swept but should survive")
	}
	if !keys.validate(ctx, dashKey) {
		t.Error("dashboard key was swept but should survive")
	}
}

// TestValidateNonEphemeral covers the governance-endpoint guard: a valid
// ephemeral per-run token is rejected while operator, CLI-managed, and
// dashboard keys still pass. This is what stops a prompt-injected agent from
// approving or pushing the task it just authored with its own run token.
func TestValidateNonEphemeral(t *testing.T) {
	ctx := context.Background()
	keys := newTestAPIKeyStore(t)

	ephemeral, err := newMCPTokenMinter(keys).Mint(ctx, "run-x", pkgruntime.MCPScope{ListTasks: true, RunTaskIDs: []string{"*"}})
	if err != nil {
		t.Fatalf("mint ephemeral: %v", err)
	}
	cliKey, _, err := keys.generate(ctx, cliManagedKeyPrefix+"laptop")
	if err != nil {
		t.Fatalf("generate cli key: %v", err)
	}
	dashKey, _, err := keys.generate(ctx, "my dashboard key")
	if err != nil {
		t.Fatalf("generate dashboard key: %v", err)
	}

	// Ephemeral token is a valid key but must fail the governance check.
	if !keys.validate(ctx, ephemeral) {
		t.Fatal("precondition: ephemeral token should be a valid key")
	}
	if keys.validateNonEphemeral(ctx, ephemeral) {
		t.Error("ephemeral token passed validateNonEphemeral")
	}
	if !keys.validateNonEphemeral(ctx, cliKey) {
		t.Error("CLI-managed key rejected by validateNonEphemeral")
	}
	if !keys.validateNonEphemeral(ctx, dashKey) {
		t.Error("dashboard key rejected by validateNonEphemeral")
	}
	if keys.validateNonEphemeral(ctx, "dck_not-a-real-key") {
		t.Error("bogus key passed validateNonEphemeral")
	}
}

// TestCreateAPIKey_RejectsReservedPrefix covers the dashboard-create guard:
// operator-chosen names must not land in a tool-managed namespace, where they
// would be denied at governance endpoints and swept at startup.
func TestCreateAPIKey_RejectsReservedPrefix(t *testing.T) {
	srv, _, _ := newApprovalTestServer(t, true)
	h := srv.Handler()
	cookie := login(t, h, "hunter2", false)
	if cookie == nil {
		t.Fatal("login failed")
	}

	for _, name := range []string{ephemeralKeyPrefix + "foo", cliManagedKeyPrefix + "bar"} {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/keys", strings.NewReader(`{"name":"`+name+`"}`))
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("name %q: status = %d, want 400: %s", name, w.Code, w.Body.String())
		}
	}

	// A normal name still succeeds.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/keys", strings.NewReader(`{"name":"my key"}`))
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("normal name: status = %d, want 200: %s", w.Code, w.Body.String())
	}
}

// TestRevokeByNamePrefix_RefusesOtherPrefixes guards against the sweep
// primitive being repurposed to bulk-delete keys outside the ephemeral
// namespace.
func TestRevokeByNamePrefix_RefusesOtherPrefixes(t *testing.T) {
	ctx := context.Background()
	keys := newTestAPIKeyStore(t)
	if err := keys.revokeByNamePrefix(ctx, cliManagedKeyPrefix); err == nil {
		t.Fatal("expected revokeByNamePrefix to refuse a non-ephemeral prefix")
	}
	if err := keys.revokeByNamePrefix(ctx, ""); err == nil {
		t.Fatal("expected revokeByNamePrefix to refuse an empty prefix")
	}
}
