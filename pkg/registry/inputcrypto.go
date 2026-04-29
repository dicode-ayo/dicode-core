package registry

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"

	"github.com/google/uuid"
	"golang.org/x/crypto/chacha20poly1305"
)

// InputCrypto encrypts run-input blobs with XChaCha20-Poly1305 and a
// fixed-width binary AAD that binds each ciphertext to the runID and
// stored_at timestamp of its row in the runs table.
//
// Blob layout:  [24-byte nonce][N-byte ciphertext + 16-byte Poly1305 tag]
// AAD layout:   [16-byte runID UUID raw bytes][8-byte stored_at uint64 BE]
//
// Cross-row splicing fails decryption: a copy-paste of one row's blob into
// another row's storage handle yields a different AAD, which AEAD-Open
// rejects.
type InputCrypto struct {
	key []byte // 32-byte sub-key from secrets.LocalProvider.DeriveSubKey("dicode/run-inputs/v1")
}

// NewInputCrypto wraps a 32-byte key. The caller is responsible for
// obtaining the key from a SubKeyDeriver — typically
// secrets.LocalProvider.DeriveSubKey("dicode/run-inputs/v1").
func NewInputCrypto(key []byte) *InputCrypto {
	return &InputCrypto{key: key}
}

// makeAAD returns the 24-byte fixed-width AAD that binds a blob to its row.
// The runID must be a valid UUID; non-UUID strings return an error rather
// than being silently mangled.
func makeAAD(runID string, storedAt int64) ([]byte, error) {
	u, err := uuid.Parse(runID)
	if err != nil {
		return nil, fmt.Errorf("runID is not a UUID: %w", err)
	}
	aad := make([]byte, 24)
	copy(aad[0:16], u[:])
	binary.BigEndian.PutUint64(aad[16:24], uint64(storedAt))
	return aad, nil
}

// Encrypt seals plaintext with the run's row identity in the AAD.
func (c *InputCrypto) Encrypt(plaintext []byte, runID string, storedAt int64) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(c.key)
	if err != nil {
		return nil, fmt.Errorf("aead: %w", err)
	}
	aad, err := makeAAD(runID, storedAt)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	out := make([]byte, 0, len(nonce)+len(plaintext)+aead.Overhead())
	out = append(out, nonce...)
	out = aead.Seal(out, nonce, plaintext, aad)
	return out, nil
}

// Decrypt opens a blob produced by Encrypt for the same runID + storedAt.
func (c *InputCrypto) Decrypt(blob []byte, runID string, storedAt int64) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(c.key)
	if err != nil {
		return nil, fmt.Errorf("aead: %w", err)
	}
	aad, err := makeAAD(runID, storedAt)
	if err != nil {
		return nil, err
	}
	if len(blob) < aead.NonceSize()+aead.Overhead() {
		return nil, fmt.Errorf("blob too short: %d bytes", len(blob))
	}
	nonce, ct := blob[:aead.NonceSize()], blob[aead.NonceSize():]
	pt, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return pt, nil
}
