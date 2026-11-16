package onboarding

import (
	"crypto/rand"
	"encoding/base64"
)

// GeneratePassphrase returns a 43-character URL-safe base64 token (32 random
// bytes, 256 bits of entropy). It is the only passphrase generator: the
// onboarding wizard, silent onboarding, first-boot auto-generation and
// `dicode auth reset-passphrase` all use it.
func GeneratePassphrase() string {
	var b [32]byte
	_, _ = rand.Read(b[:]) // never fails since Go 1.24; it crashes the program instead
	return base64.RawURLEncoding.EncodeToString(b[:])
}
