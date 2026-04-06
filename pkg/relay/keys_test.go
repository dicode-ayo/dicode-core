package relay

import (
	"context"
	"testing"

	"github.com/dicode/dicode/pkg/db"
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

	id, err := LoadOrGenerateIdentity(ctx, database)
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}

	if id.PrivateKey == nil {
		t.Fatal("nil private key")
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

	id1, err := LoadOrGenerateIdentity(ctx, database)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}

	// Load again — must return the same UUID.
	id2, err := LoadOrGenerateIdentity(ctx, database)
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

	id1, err := LoadOrGenerateIdentity(ctx, database)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	id2, err := LoadOrGenerateIdentity(ctx, database)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	// Public keys must match.
	pub1 := id1.PrivateKey.PublicKey
	pub2 := id2.PrivateKey.PublicKey

	if pub1.X.Cmp(pub2.X) != 0 || pub1.Y.Cmp(pub2.Y) != 0 {
		t.Fatal("public key changed after round-trip")
	}
}

func TestUncompressedPublicKey(t *testing.T) {
	ctx := context.Background()
	database := openTestDB(t)

	id, err := LoadOrGenerateIdentity(ctx, database)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	raw := id.UncompressedPublicKey()
	if len(raw) != 65 {
		t.Fatalf("uncompressed key must be 65 bytes, got %d", len(raw))
	}
	if raw[0] != 0x04 {
		t.Fatalf("uncompressed key must start with 0x04, got 0x%02x", raw[0])
	}

	// Must round-trip through unmarshalUncompressed.
	pub, err := unmarshalUncompressed(raw)
	if err != nil {
		t.Fatalf("unmarshalUncompressed failed: %v", err)
	}
	if pub.X.Cmp(id.PrivateKey.PublicKey.X) != 0 || pub.Y.Cmp(id.PrivateKey.PublicKey.Y) != 0 {
		t.Fatal("round-trip public key mismatch")
	}
}

func TestDeriveUUID(t *testing.T) {
	ctx := context.Background()
	database := openTestDB(t)

	id, err := LoadOrGenerateIdentity(ctx, database)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// Recompute UUID manually.
	expected := deriveUUID(&id.PrivateKey.PublicKey)
	if id.UUID != expected {
		t.Fatalf("UUID mismatch: got %s, want %s", id.UUID, expected)
	}
}
