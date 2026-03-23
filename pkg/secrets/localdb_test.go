package secrets

import (
	"context"
	"testing"

	"github.com/dicode/dicode/pkg/db"
)

func newTestSecretDB(t *testing.T) *SQLiteSecretDB {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return NewSQLiteSecretDB(d)
}

func TestSQLiteSecretDB_SetGetDelete(t *testing.T) {
	sdb := newTestSecretDB(t)

	ct := []byte("ciphertext")
	nonce := []byte("nonce000000000000000000000")

	if err := sdb.SetEncrypted("MY_KEY", ct, nonce); err != nil {
		t.Fatalf("set: %v", err)
	}

	gotCT, gotNonce, found, err := sdb.GetEncrypted("MY_KEY")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !found {
		t.Fatal("key not found")
	}
	if string(gotCT) != string(ct) {
		t.Errorf("ciphertext mismatch")
	}
	if string(gotNonce) != string(nonce) {
		t.Errorf("nonce mismatch")
	}

	if err := sdb.Delete("MY_KEY"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, _, found, _ = sdb.GetEncrypted("MY_KEY")
	if found {
		t.Fatal("key should be deleted")
	}
}

func TestSQLiteSecretDB_Upsert(t *testing.T) {
	sdb := newTestSecretDB(t)

	_ = sdb.SetEncrypted("K", []byte("v1"), []byte("nonce00000000000000000000000"))
	_ = sdb.SetEncrypted("K", []byte("v2"), []byte("nonce11111111111111111111111"))

	ct, _, found, _ := sdb.GetEncrypted("K")
	if !found || string(ct) != "v2" {
		t.Fatal("upsert did not update value")
	}
}

func TestSQLiteSecretDB_List(t *testing.T) {
	sdb := newTestSecretDB(t)
	n := []byte("nonce00000000000000000000000")

	_ = sdb.SetEncrypted("B", []byte("x"), n)
	_ = sdb.SetEncrypted("A", []byte("y"), n)

	keys, err := sdb.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(keys) != 2 || keys[0] != "A" || keys[1] != "B" {
		t.Fatalf("unexpected keys: %v", keys)
	}
}

func TestLocalProvider_RoundTrip(t *testing.T) {
	sdb := newTestSecretDB(t)
	dir := t.TempDir()

	p, err := NewLocalProvider(dir, sdb)
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	ctx := context.Background()
	if err := p.Set(ctx, "TOKEN", "super-secret"); err != nil {
		t.Fatalf("set: %v", err)
	}

	val, err := p.Get(ctx, "TOKEN")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if val != "super-secret" {
		t.Fatalf("got %q, want %q", val, "super-secret")
	}
}

func TestLocalProvider_Delete(t *testing.T) {
	sdb := newTestSecretDB(t)
	dir := t.TempDir()

	p, _ := NewLocalProvider(dir, sdb)
	ctx := context.Background()

	_ = p.Set(ctx, "K", "v")
	_ = p.Delete(ctx, "K")

	val, err := p.Get(ctx, "K")
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if val != "" {
		t.Fatalf("expected empty, got %q", val)
	}
}

func TestLocalProvider_List(t *testing.T) {
	sdb := newTestSecretDB(t)
	dir := t.TempDir()

	p, _ := NewLocalProvider(dir, sdb)
	ctx := context.Background()

	_ = p.Set(ctx, "A", "1")
	_ = p.Set(ctx, "B", "2")

	keys, err := p.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %v", keys)
	}
}
