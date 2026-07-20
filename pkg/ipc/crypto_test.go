package ipc

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"
)

// stubDeriver returns a deterministic 32-byte key per context derived from
// SHA-256. Does not use real Argon2id/HKDF so tests run instantly. The two
// methods use distinct hash prefixes so tests can tell HKDF-derived and
// legacy Argon2id-derived keys apart, mirroring the real two-KDF split.
type stubDeriver struct{}

func (stubDeriver) DeriveSubKey(ctx string) ([]byte, error) {
	h := sha256.Sum256([]byte("test-master-key/legacy-argon2id/" + ctx))
	return h[:], nil
}

func (stubDeriver) DeriveSubKeyHKDF(ctx string) ([]byte, error) {
	h := sha256.Sum256([]byte("test-master-key/hkdf/" + ctx))
	return h[:], nil
}

func TestCryptoHandler_RoundTrip(t *testing.T) {
	h := newCryptoHandler(stubDeriver{})
	plaintext := []byte("hello, world")
	ct, err := h.Encrypt("test/v1", plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	pt, err := h.Decrypt("test/v1", ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(pt, plaintext) {
		t.Errorf("round-trip mismatch: got %q, want %q", pt, plaintext)
	}
}

func TestCryptoHandler_DifferentContextsRejectsDecrypt(t *testing.T) {
	h := newCryptoHandler(stubDeriver{})
	ct, err := h.Encrypt("foo", []byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := h.Decrypt("bar", ct); err == nil {
		t.Error("expected error decrypting under different context")
	}
}

func TestCryptoHandler_DifferentContextsProduceDifferentSubkeys(t *testing.T) {
	h := newCryptoHandler(stubDeriver{})
	plaintext := []byte("same-plaintext")
	ct1, err := h.Encrypt("foo", plaintext)
	if err != nil {
		t.Fatalf("Encrypt foo: %v", err)
	}
	ct2, err := h.Encrypt("bar", plaintext)
	if err != nil {
		t.Fatalf("Encrypt bar: %v", err)
	}
	// Ciphertexts must differ (different sub-keys + random nonces).
	if bytes.Equal(ct1, ct2) {
		t.Error("ciphertexts from different contexts must not be equal")
	}
	// Cross-context decryption must fail in both directions.
	if _, err := h.Decrypt("bar", ct1); err == nil {
		t.Error("expected error: decrypt foo ciphertext under bar context")
	}
	if _, err := h.Decrypt("foo", ct2); err == nil {
		t.Error("expected error: decrypt bar ciphertext under foo context")
	}
}

func TestCryptoHandler_NonceUniqueness(t *testing.T) {
	h := newCryptoHandler(stubDeriver{})
	plaintext := []byte("nonce-test")
	ct1, err := h.Encrypt("ctx", plaintext)
	if err != nil {
		t.Fatalf("Encrypt 1: %v", err)
	}
	ct2, err := h.Encrypt("ctx", plaintext)
	if err != nil {
		t.Fatalf("Encrypt 2: %v", err)
	}
	nonce1 := ct1[:chacha20poly1305.NonceSizeX]
	nonce2 := ct2[:chacha20poly1305.NonceSizeX]
	if bytes.Equal(nonce1, nonce2) {
		t.Error("nonces must differ across calls")
	}
}

func TestCryptoHandler_TamperedCiphertextRejected(t *testing.T) {
	h := newCryptoHandler(stubDeriver{})
	ct, err := h.Encrypt("ctx", []byte("data"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// Flip one byte in the ciphertext body (past the nonce).
	ct[chacha20poly1305.NonceSizeX] ^= 0xff
	if _, err := h.Decrypt("ctx", ct); err == nil {
		t.Error("expected error for tampered ciphertext")
	}
}

func TestCryptoHandler_TooLargePlaintextRejected(t *testing.T) {
	h := newCryptoHandler(stubDeriver{})
	big := make([]byte, 65<<20) // 65 MiB > 64 MiB cap
	if _, err := h.Encrypt("ctx", big); err == nil {
		t.Error("expected error for oversized plaintext")
	}
}

// TestCryptoHandler_EncryptUsesHKDFNotLegacyKey locks in issue #607: Encrypt
// must derive via DeriveSubKeyHKDF, not the legacy Argon2id DeriveSubKey.
// Before the fix, Encrypt called DeriveSubKey, so a blob it produced would
// open under the legacy key directly (this test's manual openWithKey call
// would succeed); after the fix it must NOT — only the HKDF key opens it.
func TestCryptoHandler_EncryptUsesHKDFNotLegacyKey(t *testing.T) {
	d := stubDeriver{}
	h := newCryptoHandler(d)
	ct, err := h.Encrypt("ctx", []byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	hkdfKey, err := d.DeriveSubKeyHKDF("ctx")
	if err != nil {
		t.Fatalf("DeriveSubKeyHKDF: %v", err)
	}
	if _, err := openWithKey(hkdfKey, "ctx", ct); err != nil {
		t.Errorf("blob from Encrypt must open under the HKDF key: %v", err)
	}

	legacyKey, err := d.DeriveSubKey("ctx")
	if err != nil {
		t.Fatalf("DeriveSubKey: %v", err)
	}
	if _, err := openWithKey(legacyKey, "ctx", ct); err == nil {
		t.Error("blob from Encrypt must NOT open under the legacy Argon2id key (Encrypt regressed to the old KDF)")
	}
}

// TestCryptoHandler_DecryptFallsBackToLegacyKey locks in the backward-compat
// half of #607: a blob sealed under the pre-migration Argon2id-derived key
// (as Encrypt itself would have produced before this change) must still
// decrypt via Decrypt's fallback path, with no blob-format change required.
func TestCryptoHandler_DecryptFallsBackToLegacyKey(t *testing.T) {
	d := stubDeriver{}
	h := newCryptoHandler(d)

	legacyKey, err := d.DeriveSubKey("ctx")
	if err != nil {
		t.Fatalf("DeriveSubKey: %v", err)
	}
	aead, err := chacha20poly1305.NewX(legacyKey)
	if err != nil {
		t.Fatalf("aead: %v", err)
	}
	nonce := make([]byte, aead.NonceSize())
	plaintext := []byte("pre-migration secret")
	legacyBlob := aead.Seal(nonce, nonce, plaintext, []byte("ctx"))

	pt, err := h.Decrypt("ctx", legacyBlob)
	if err != nil {
		t.Fatalf("Decrypt of legacy Argon2id blob must fall back and succeed: %v", err)
	}
	if !bytes.Equal(pt, plaintext) {
		t.Errorf("legacy-blob round-trip mismatch: got %q, want %q", pt, plaintext)
	}
}
