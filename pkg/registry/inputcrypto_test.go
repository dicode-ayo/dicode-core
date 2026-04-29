package registry

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"
)

// newTestInputCrypto returns an InputCrypto seeded with a fixed 32-byte test
// key. NEVER used in production.
func newTestInputCrypto(t *testing.T) *InputCrypto {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return NewInputCrypto(key)
}

func TestInputCrypto_RoundTrip(t *testing.T) {
	c := newTestInputCrypto(t)

	runID := uuid.New().String()
	storedAt := time.Now().Unix()
	plaintext := []byte(`{"hello":"world"}`)

	blob, err := c.Encrypt(plaintext, runID, storedAt)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Decrypt(blob, runID, storedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("got %q, want %q", got, plaintext)
	}
}

func TestInputCrypto_AAD_BindsToRunIDAndTimestamp(t *testing.T) {
	c := newTestInputCrypto(t)
	runA := uuid.New().String()
	runB := uuid.New().String()
	now := time.Now().Unix()

	blob, err := c.Encrypt([]byte("data"), runA, now)
	if err != nil {
		t.Fatal(err)
	}
	// Cross-row decrypt with different runID must fail.
	if _, err := c.Decrypt(blob, runB, now); err == nil {
		t.Error("decrypt with different runID should fail")
	}
	// Cross-time decrypt with different stored_at must fail.
	if _, err := c.Decrypt(blob, runA, now+1); err == nil {
		t.Error("decrypt with different stored_at should fail")
	}
}

func TestInputCrypto_NonceUniqueness(t *testing.T) {
	c := newTestInputCrypto(t)
	runID := uuid.New().String()
	now := time.Now().Unix()

	const N = 100
	seen := make(map[string]struct{}, N)
	for i := 0; i < N; i++ {
		blob, err := c.Encrypt([]byte("x"), runID, now)
		if err != nil {
			t.Fatal(err)
		}
		// Nonce is the leading 24 bytes (XChaCha20-Poly1305 NonceSize).
		if len(blob) < 24 {
			t.Fatalf("blob too short: %d", len(blob))
		}
		nonce := string(blob[:24])
		if _, dup := seen[nonce]; dup {
			t.Fatalf("duplicate nonce after %d encryptions", i)
		}
		seen[nonce] = struct{}{}
	}
}

func TestInputCrypto_TamperedCiphertextRejected(t *testing.T) {
	c := newTestInputCrypto(t)
	runID := uuid.New().String()
	now := time.Now().Unix()

	blob, err := c.Encrypt([]byte("secret"), runID, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(blob) < 30 {
		t.Fatalf("blob too short")
	}
	blob[27] ^= 0xff
	if _, err := c.Decrypt(blob, runID, now); err == nil {
		t.Error("decrypt of tampered ciphertext should fail")
	}
}

func TestInputCrypto_RejectsNonUUIDRunID(t *testing.T) {
	c := newTestInputCrypto(t)
	if _, err := c.Encrypt([]byte("x"), "not-a-uuid", 0); err == nil {
		t.Error("expected error for non-UUID runID at encrypt time")
	}
	if _, err := c.Decrypt([]byte("x"), "not-a-uuid", 0); err == nil {
		t.Error("expected error for non-UUID runID at decrypt time")
	}
}

func TestInputCrypto_TooShortBlobRejected(t *testing.T) {
	c := newTestInputCrypto(t)
	runID := uuid.New().String()
	if _, err := c.Decrypt([]byte("short"), runID, 0); err == nil {
		t.Error("expected error for short blob")
	}
}
