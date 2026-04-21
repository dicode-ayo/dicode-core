package relay

import (
	"context"
	"testing"

	"github.com/dicode/dicode/pkg/db"
	"go.uber.org/zap"
)

func openTestDB(t *testing.T) db.DB {
	t.Helper()
	database, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	// Ensure kv table exists.
	if err := database.Exec(context.Background(), `
		CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value TEXT NOT NULL)
	`); err != nil {
		t.Fatalf("create kv table: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func TestGenerateIdentity(t *testing.T) {
	ctx := context.Background()
	database := openTestDB(t)

	id, err := LoadOrGenerateIdentity(ctx, database, zap.NewNop())
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}

	if id.SignKey == nil {
		t.Fatal("nil sign key")
	}
	if id.DecryptKey == nil {
		t.Fatal("nil decrypt key")
	}
	if id.UUID == "" {
		t.Fatal("empty UUID")
	}
	if len(id.UUID) != 64 {
		t.Fatalf("UUID must be 64 hex chars, got %d", len(id.UUID))
	}
}

func TestUUIDDeterministic(t *testing.T) {
	ctx := context.Background()
	database := openTestDB(t)

	id1, err := LoadOrGenerateIdentity(ctx, database, zap.NewNop())
	if err != nil {
		t.Fatalf("first load: %v", err)
	}

	// Load again — must return the same UUID.
	id2, err := LoadOrGenerateIdentity(ctx, database, zap.NewNop())
	if err != nil {
		t.Fatalf("second load: %v", err)
	}

	if id1.UUID != id2.UUID {
		t.Fatalf("UUID changed across loads: %s vs %s", id1.UUID, id2.UUID)
	}
}

func TestKeyRoundTrip(t *testing.T) {
	ctx := context.Background()
	database := openTestDB(t)

	id1, err := LoadOrGenerateIdentity(ctx, database, zap.NewNop())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	id2, err := LoadOrGenerateIdentity(ctx, database, zap.NewNop())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	// Both public keys (sign and decrypt) must round-trip identically.
	if id1.SignKey.PublicKey.X.Cmp(id2.SignKey.PublicKey.X) != 0 ||
		id1.SignKey.PublicKey.Y.Cmp(id2.SignKey.PublicKey.Y) != 0 {
		t.Fatal("sign public key changed after round-trip")
	}
	if id1.DecryptKey.PublicKey.X.Cmp(id2.DecryptKey.PublicKey.X) != 0 ||
		id1.DecryptKey.PublicKey.Y.Cmp(id2.DecryptKey.PublicKey.Y) != 0 {
		t.Fatal("decrypt public key changed after round-trip")
	}
}

func TestSignPublicKey(t *testing.T) {
	ctx := context.Background()
	database := openTestDB(t)

	id, err := LoadOrGenerateIdentity(ctx, database, zap.NewNop())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	raw := id.SignPublicKey()
	if len(raw) != 65 {
		t.Fatalf("uncompressed key must be 65 bytes, got %d", len(raw))
	}
	if raw[0] != 0x04 {
		t.Fatalf("uncompressed key must start with 0x04, got 0x%02x", raw[0])
	}

	// Must round-trip through unmarshalUncompressed and match SignKey.
	pub, err := unmarshalUncompressed(raw)
	if err != nil {
		t.Fatalf("unmarshalUncompressed failed: %v", err)
	}
	if pub.X.Cmp(id.SignKey.PublicKey.X) != 0 || pub.Y.Cmp(id.SignKey.PublicKey.Y) != 0 {
		t.Fatal("round-trip sign public key mismatch")
	}
}

func TestDecryptPublicKey(t *testing.T) {
	ctx := context.Background()
	database := openTestDB(t)

	id, err := LoadOrGenerateIdentity(ctx, database, zap.NewNop())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	raw := id.DecryptPublicKey()
	if len(raw) != 65 {
		t.Fatalf("uncompressed key must be 65 bytes, got %d", len(raw))
	}
	if raw[0] != 0x04 {
		t.Fatalf("uncompressed key must start with 0x04, got 0x%02x", raw[0])
	}

	pub, err := unmarshalUncompressed(raw)
	if err != nil {
		t.Fatalf("unmarshalUncompressed failed: %v", err)
	}
	if pub.X.Cmp(id.DecryptKey.PublicKey.X) != 0 || pub.Y.Cmp(id.DecryptKey.PublicKey.Y) != 0 {
		t.Fatal("round-trip decrypt public key mismatch")
	}
}

func TestDeriveUUID(t *testing.T) {
	ctx := context.Background()
	database := openTestDB(t)

	id, err := LoadOrGenerateIdentity(ctx, database, zap.NewNop())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// The UUID is derived from the SignKey pubkey only — the DecryptKey
	// has no effect on the URL prefix (issue #104).
	expected := deriveUUID(&id.SignKey.PublicKey)
	if id.UUID != expected {
		t.Fatalf("UUID mismatch: got %s, want %s", id.UUID, expected)
	}
}
