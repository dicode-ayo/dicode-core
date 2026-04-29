package secrets

import (
	"bytes"
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
}
