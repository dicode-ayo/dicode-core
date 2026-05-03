package ipc

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"
)

// stubDeriver returns a deterministic 32-byte key per context derived from
// SHA-256. Does not use real Argon2id so tests run instantly.
type stubDeriver struct{}

func (stubDeriver) DeriveSubKey(ctx string) ([]byte, error) {
	h := sha256.Sum256([]byte("test-master-key/" + ctx))
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
