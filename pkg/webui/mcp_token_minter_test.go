package webui

import (
	"context"
	"testing"

	"github.com/dicode/dicode/pkg/db"
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

	token, err := minter.Mint(ctx, "run-123")
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
