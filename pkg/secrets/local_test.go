package secrets

import (
	"bytes"
	"encoding/base64"
	"os"
	"testing"
)

// newTestLocalProvider builds a LocalProvider against a temp data dir.
// loadOrCreateMasterKey + loadOrCreateSalt auto-generate fixtures on first call.
func newTestLocalProvider(t *testing.T) *LocalProvider {
	t.Helper()
	dataDir := t.TempDir()
	sdb := newTestSecretDB(t) // helper from localdb_test.go in the same package
	p, err := NewLocalProvider(dataDir, sdb)
	if err != nil {
		t.Fatalf("NewLocalProvider: %v", err)
	}
	return p
}

func TestLocalProvider_DeriveSubKey_SameContextDeterministic(t *testing.T) {
	p := newTestLocalProvider(t)
	k1, err := p.DeriveSubKey("dicode/run-inputs/v1")
	if err != nil {
		t.Fatal(err)
	}
	k2, err := p.DeriveSubKey("dicode/run-inputs/v1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1, k2) {
		t.Error("same context should yield same key")
	}
	if len(k1) != 32 {
		t.Errorf("expected 32-byte key, got %d", len(k1))
	}
}

func TestLocalProvider_DeriveSubKey_DifferentContextsDistinct(t *testing.T) {
	p := newTestLocalProvider(t)
	k1, err := p.DeriveSubKey("dicode/run-inputs/v1")
	if err != nil {
		t.Fatal(err)
	}
	k2, err := p.DeriveSubKey("dicode/other-purpose/v1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(k1, k2) {
		t.Error("different contexts must yield different keys")
	}
}

func TestLocalProvider_DeriveSubKey_DistinctFromPrimaryKey(t *testing.T) {
	p := newTestLocalProvider(t)
	k, err := p.DeriveSubKey("dicode/run-inputs/v1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(k, p.key) {
		t.Error("sub-key must differ from the primary secrets-table derived key")
	}
}

func TestLocalProvider_DeriveSubKey_RejectsEmptyContext(t *testing.T) {
	p := newTestLocalProvider(t)
	_, err := p.DeriveSubKey("")
	if err == nil {
		t.Error("expected error for empty context")
	}
}

func TestLocalProvider_DeriveSubKeyHKDF_SameContextDeterministic(t *testing.T) {
	p := newTestLocalProvider(t)
	k1, err := p.DeriveSubKeyHKDF("dicode/run-inputs/v1")
	if err != nil {
		t.Fatal(err)
	}
	k2, err := p.DeriveSubKeyHKDF("dicode/run-inputs/v1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1, k2) {
		t.Error("same context should yield same key")
	}
	if len(k1) != 32 {
		t.Errorf("expected 32-byte key, got %d", len(k1))
	}
}

func TestLocalProvider_DeriveSubKeyHKDF_DifferentContextsDistinct(t *testing.T) {
	p := newTestLocalProvider(t)
	k1, err := p.DeriveSubKeyHKDF("dicode/run-inputs/v1")
	if err != nil {
		t.Fatal(err)
	}
	k2, err := p.DeriveSubKeyHKDF("dicode/other-purpose/v1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(k1, k2) {
		t.Error("different contexts must yield different keys")
	}
}

func TestLocalProvider_DeriveSubKeyHKDF_RejectsEmptyContext(t *testing.T) {
	p := newTestLocalProvider(t)
	_, err := p.DeriveSubKeyHKDF("")
	if err == nil {
		t.Error("expected error for empty context")
	}
}

// TestLocalProvider_DeriveSubKeyHKDF_DiffersFromArgon2id is the core
// regression for #607: the same context must derive to two different keys
// depending on which method is called, since they use unrelated KDFs. This
// is what makes Decrypt's HKDF-first/Argon2id-fallback pattern in
// pkg/ipc/crypto.go actually distinguish pre- and post-migration blobs.
func TestLocalProvider_DeriveSubKeyHKDF_DiffersFromArgon2id(t *testing.T) {
	p := newTestLocalProvider(t)
	hkdfKey, err := p.DeriveSubKeyHKDF("dicode/run-inputs/v1")
	if err != nil {
		t.Fatal(err)
	}
	argonKey, err := p.DeriveSubKey("dicode/run-inputs/v1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(hkdfKey, argonKey) {
		t.Error("HKDF- and Argon2id-derived keys for the same context must differ")
	}
}

func TestNewLocalProvider_UnsetsMasterKeyEnvVar(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	t.Setenv("DICODE_MASTER_KEY", base64.StdEncoding.EncodeToString(key))

	p, err := NewLocalProvider(t.TempDir(), newTestSecretDB(t))
	if err != nil {
		t.Fatalf("NewLocalProvider: %v", err)
	}

	if v, ok := os.LookupEnv("DICODE_MASTER_KEY"); ok {
		t.Errorf("DICODE_MASTER_KEY still in process env after provider init: %q", v)
	}
	if !bytes.Equal(p.masterKey, key) {
		t.Error("master key bytes not retained after env unset")
	}
	if _, err := p.DeriveSubKey("dicode/run-inputs/v1"); err != nil {
		t.Errorf("DeriveSubKey after env unset: %v", err)
	}
}

func TestSubKeyDeriver_TypeAssertion(t *testing.T) {
	var p Provider = newTestLocalProvider(t)
	deriver, ok := p.(SubKeyDeriver)
	if !ok {
		t.Fatal("LocalProvider should implement SubKeyDeriver")
	}
	k, err := deriver.DeriveSubKey("dicode/test/v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(k) != 32 {
		t.Errorf("len = %d, want 32", len(k))
	}
	hk, err := deriver.DeriveSubKeyHKDF("dicode/test/v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(hk) != 32 {
		t.Errorf("len = %d, want 32", len(hk))
	}
}
