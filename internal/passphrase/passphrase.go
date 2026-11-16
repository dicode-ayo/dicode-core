// Package passphrase generates dicode's auth passphrases.
package passphrase

import (
	"crypto/rand"
	"encoding/base64"
)

// Generate returns a 43-character URL-safe base64 token (32 random bytes,
// 256 bits of entropy).
func Generate() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on modern kernels
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
