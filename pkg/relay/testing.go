package relay

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
)

// NewTestIdentity returns a fresh in-memory *Identity suitable for unit tests
// that need a valid signing and decryption keypair but don't want the weight
// of a SQLite round-trip through LoadOrGenerateIdentity.
//
// The helper exists because the Identity struct layout is an implementation
// detail of the relay package — tests in other packages (e.g. pkg/ipc) would
// otherwise have to know the field names and reach into the struct literally,
// which makes the split-key refactor (issue #104) a mechanical churn across
// the whole repo every time the shape changes.
//
// Two distinct keys are generated to exercise the post-#104 invariant that
// SignKey and DecryptKey are independent. The UUID is derived from the
// SignKey public key exactly as in LoadOrGenerateIdentity.
//
// Naming note: this file lives in the production build (Go test helpers in
// same-package must be in _test.go files; to share with other packages we
// need a regular file). The cost is the stdlib "testing" import linking
// into the daemon binary — acceptable for dicode's scale. See the issue
// #104 design doc §4.1, decision 5, for the rationale.
func NewTestIdentity(t *testing.T) *Identity {
	t.Helper()
	signKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("relay: generate sign key: %v", err)
	}
	decryptKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("relay: generate decrypt key: %v", err)
	}
	return &Identity{
		SignKey:    signKey,
		DecryptKey: decryptKey,
		UUID:       deriveUUID(&signKey.PublicKey),
	}
}
