package relay

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

// TestIdentity_KeysAreDistinct locks in the post-#104 invariant: a freshly
// generated identity never reuses the same key for both roles. If the two
// fields aliased the same *ecdsa.PrivateKey the whole point of the split
// (domain separation between ECDSA sign and ECDH decrypt) would be lost.
func TestIdentity_KeysAreDistinct(t *testing.T) {
	ctx := context.Background()
	database := openTestDB(t)

	id, err := LoadOrGenerateIdentity(ctx, database, zap.NewNop())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if id.SignKey == id.DecryptKey {
		t.Fatal("SignKey and DecryptKey are the same *ecdsa.PrivateKey pointer")
	}
	if id.SignKey.D.Cmp(id.DecryptKey.D) == 0 {
		t.Fatal("SignKey.D == DecryptKey.D — keys are not independently generated")
	}
	if id.SignKey.PublicKey.X.Cmp(id.DecryptKey.PublicKey.X) == 0 &&
		id.SignKey.PublicKey.Y.Cmp(id.DecryptKey.PublicKey.Y) == 0 {
		t.Fatal("SignKey public point == DecryptKey public point")
	}
}

// TestMigration_OldKeyBecomesSignKey exercises the upgrade path: a daemon
// booting on post-#104 code against a database that only has the legacy
// relay.private_key row must reuse that key as the SignKey (preserving the
// UUID and therefore the shared webhook URLs) and generate a fresh
// DecryptKey, persisting it in a new relay.decrypt_private_key row.
func TestMigration_OldKeyBecomesSignKey(t *testing.T) {
	ctx := context.Background()
	database := openTestDB(t)

	// Simulate a pre-#104 daemon: generate an identity and keep only the
	// sign key row. (LoadOrGenerateIdentity on the new code always writes
	// both rows, so we seed manually by deleting the decrypt row it wrote.)
	seeded, err := LoadOrGenerateIdentity(ctx, database, zap.NewNop())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	originalUUID := seeded.UUID
	originalSignD := seeded.SignKey.D.String()
	if err := database.Exec(ctx,
		`DELETE FROM kv WHERE key = ?`, kvKeyRelayDecryptPrivateKey,
	); err != nil {
		t.Fatalf("delete decrypt row (pre-104 seeding): %v", err)
	}
	// Sanity: the decrypt row should now be absent.
	if v, _ := loadKVKey(ctx, database, kvKeyRelayDecryptPrivateKey); v != "" {
		t.Fatalf("precondition: decrypt row should be empty after seeding, got %q", v)
	}

	// Run the loader again — the migration path should kick in.
	migrated, err := LoadOrGenerateIdentity(ctx, database, zap.NewNop())
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 1. SignKey must be byte-identical to the seeded one.
	if migrated.SignKey.D.String() != originalSignD {
		t.Fatalf("SignKey changed across migration: want D=%s got D=%s",
			originalSignD, migrated.SignKey.D.String())
	}
	// 2. UUID must be stable (webhook URL invariant).
	if migrated.UUID != originalUUID {
		t.Fatalf("UUID changed across migration: want %s got %s", originalUUID, migrated.UUID)
	}
	// 3. The KV row for the decrypt key must now exist.
	decryptPEM, err := loadKVKey(ctx, database, kvKeyRelayDecryptPrivateKey)
	if err != nil {
		t.Fatalf("load decrypt row after migration: %v", err)
	}
	if decryptPEM == "" {
		t.Fatal("migration did not insert relay.decrypt_private_key row")
	}
	// 4. The new DecryptKey must differ from SignKey (distinctness invariant).
	if migrated.DecryptKey.D.Cmp(migrated.SignKey.D) == 0 {
		t.Fatal("migration reused SignKey as DecryptKey — split is defeated")
	}
}

// TestMigration_Idempotent verifies that running the loader twice on a
// migrated database is a no-op on the DB side and returns identical keys.
// This guards against a regression where the decrypt-key row is
// accidentally rewritten (and therefore rotated) on every daemon start.
func TestMigration_Idempotent(t *testing.T) {
	ctx := context.Background()
	database := openTestDB(t)

	first, err := LoadOrGenerateIdentity(ctx, database, zap.NewNop())
	if err != nil {
		t.Fatalf("first load: %v", err)
	}

	// Capture the raw PEM rows so we can compare byte-for-byte.
	firstSignPEM, err := loadKVKey(ctx, database, kvKeyRelayPrivateKey)
	if err != nil {
		t.Fatalf("read sign row: %v", err)
	}
	firstDecryptPEM, err := loadKVKey(ctx, database, kvKeyRelayDecryptPrivateKey)
	if err != nil {
		t.Fatalf("read decrypt row: %v", err)
	}

	second, err := LoadOrGenerateIdentity(ctx, database, zap.NewNop())
	if err != nil {
		t.Fatalf("second load: %v", err)
	}

	if first.SignKey.D.Cmp(second.SignKey.D) != 0 {
		t.Fatal("SignKey.D changed on second load — migration is not idempotent")
	}
	if first.DecryptKey.D.Cmp(second.DecryptKey.D) != 0 {
		t.Fatal("DecryptKey.D changed on second load — migration is not idempotent")
	}

	secondSignPEM, _ := loadKVKey(ctx, database, kvKeyRelayPrivateKey)
	secondDecryptPEM, _ := loadKVKey(ctx, database, kvKeyRelayDecryptPrivateKey)
	if firstSignPEM != secondSignPEM {
		t.Fatal("relay.private_key PEM rewritten on second load")
	}
	if firstDecryptPEM != secondDecryptPEM {
		t.Fatal("relay.decrypt_private_key PEM rewritten on second load")
	}
}
