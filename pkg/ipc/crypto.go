package ipc

import (
	"crypto/rand"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// SubKeyDeriver mirrors secrets.LocalProvider.DeriveSubKey — taken as an
// interface so the IPC layer doesn't depend on pkg/secrets directly.
type SubKeyDeriver interface {
	DeriveSubKey(context string) ([]byte, error)
}

// cryptoHandler implements dicode.crypto.{encrypt, decrypt}. It derives a
// per-context sub-key on each call and uses XChaCha20-Poly1305 with the
// context bytes bound into AAD.
//
// Sub-keys are NOT cached: re-derivation is O(Argon2id) but happens once
// per encrypt/decrypt call (rare, not hot-path). Caching across IPC calls
// would add lifecycle complexity (passphrase change invalidation, etc.)
// for negligible perf gain at our call rates.
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
	key, err := h.deriver.DeriveSubKey(context)
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

// Decrypt opens a blob produced by Encrypt under the same context.
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
	key, err := h.deriver.DeriveSubKey(context)
	if err != nil {
		return nil, fmt.Errorf("derive: %w", err)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("aead: %w", err)
	}
	nonce, ct := blob[:aead.NonceSize()], blob[aead.NonceSize():]
	pt, err := aead.Open(nil, nonce, ct, []byte(context))
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	return pt, nil
}
