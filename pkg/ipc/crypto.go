package ipc

import (
	"crypto/rand"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// SubKeyDeriver mirrors secrets.LocalProvider.DeriveSubKey /
// DeriveSubKeyHKDF — taken as an interface so the IPC layer doesn't depend
// on pkg/secrets directly.
type SubKeyDeriver interface {
	// DeriveSubKey is the legacy Argon2id derivation. Only used here to
	// decrypt blobs written before the HKDF migration (see DeriveSubKeyHKDF).
	DeriveSubKey(context string) ([]byte, error)
	// DeriveSubKeyHKDF derives via HKDF-SHA256, the primitive actually
	// suited to our high-entropy master key. See issue #607.
	DeriveSubKeyHKDF(context string) ([]byte, error)
}

// cryptoHandler implements dicode.crypto.{encrypt, decrypt}. It derives a
// per-context sub-key on each call and uses XChaCha20-Poly1305 with the
// context bytes bound into AAD.
//
// Key derivation: Encrypt always derives via HKDF-SHA256 (DeriveSubKeyHKDF)
// — cheap enough to call on every request, unlike the Argon2id primary/
// sub-key derivation used elsewhere in dicode, which is deliberately
// memory-hard for low-entropy inputs (passwords) and not needed here since
// the master key is already 32 random bytes (issue #607: a task calling
// dicode.crypto in a loop was burning 64 MiB × 4 threads per call). Decrypt
// tries the HKDF key first and, on AEAD-open failure, falls back to the
// legacy Argon2id-derived key — so blobs encrypted before this migration
// keep decrypting with no blob-format change and no forced re-encryption.
//
// Sub-keys are NOT cached across calls: HKDF-SHA256 is fast enough (unlike
// Argon2id) that per-call re-derivation is no longer a meaningful cost, so
// the cache-invalidation complexity (passphrase change, etc.) isn't worth
// taking on.
type cryptoHandler struct {
	deriver SubKeyDeriver
}

func newCryptoHandler(d SubKeyDeriver) *cryptoHandler {
	return &cryptoHandler{deriver: d}
}

const (
	maxCryptoPlaintextBytes  = 64 << 20 // 64 MiB — same cap as InputCrypto
	maxCryptoCiphertextBytes = 64<<20 + chacha20poly1305.NonceSizeX + chacha20poly1305.Overhead
)

// Encrypt seals plaintext under a sub-key derived from context. The context
// bytes are bound into the AEAD AAD so cross-context substitution fails.
//
// Output layout: [24-byte nonce][ciphertext + 16-byte Poly1305 tag]
func (h *cryptoHandler) Encrypt(context string, plaintext []byte) ([]byte, error) {
	if context == "" {
		return nil, fmt.Errorf("context required")
	}
	if len(plaintext) > maxCryptoPlaintextBytes {
		return nil, fmt.Errorf("plaintext too large: %d bytes (cap %d)", len(plaintext), maxCryptoPlaintextBytes)
	}
	key, err := h.deriver.DeriveSubKeyHKDF(context)
	if err != nil {
		return nil, fmt.Errorf("derive: %w", err)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("aead: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	out := make([]byte, 0, len(nonce)+len(plaintext)+aead.Overhead())
	out = append(out, nonce...)
	out = aead.Seal(out, nonce, plaintext, []byte(context))
	return out, nil
}

// Decrypt opens a blob produced by Encrypt under the same context. It tries
// the current HKDF-derived key first, then falls back to the legacy
// Argon2id-derived key — Encrypt has always used HKDF since the #607
// migration, but blobs sealed before that change (or by a caller still
// holding one from before an upgrade) were sealed under the Argon2id key,
// and there's no version tag in the blob to tell the two apart up front.
func (h *cryptoHandler) Decrypt(context string, blob []byte) ([]byte, error) {
	if context == "" {
		return nil, fmt.Errorf("context required")
	}
	if len(blob) > maxCryptoCiphertextBytes {
		return nil, fmt.Errorf("ciphertext too large: %d bytes (cap %d)", len(blob), maxCryptoCiphertextBytes)
	}
	if len(blob) < chacha20poly1305.NonceSizeX+chacha20poly1305.Overhead {
		return nil, fmt.Errorf("blob too short")
	}

	hkdfKey, err := h.deriver.DeriveSubKeyHKDF(context)
	if err != nil {
		return nil, fmt.Errorf("derive: %w", err)
	}
	if pt, err := openWithKey(hkdfKey, context, blob); err == nil {
		return pt, nil
	}

	legacyKey, err := h.deriver.DeriveSubKey(context)
	if err != nil {
		return nil, fmt.Errorf("derive (legacy): %w", err)
	}
	pt, err := openWithKey(legacyKey, context, blob)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	return pt, nil
}

// openWithKey opens blob (nonce prefix + ciphertext) under key, with context
// bound as AAD. Shared by Decrypt's HKDF-first / Argon2id-fallback attempts.
func openWithKey(key []byte, context string, blob []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("aead: %w", err)
	}
	nonce, ct := blob[:aead.NonceSize()], blob[aead.NonceSize():]
	return aead.Open(nil, nonce, ct, []byte(context))
}
