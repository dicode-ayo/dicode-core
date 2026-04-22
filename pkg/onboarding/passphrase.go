package onboarding

import (
	"crypto/rand"
	"encoding/base64"
)

// GeneratePassphrase returns a 24-character URL-safe base64 token
// (18 random bytes → ~108 bits of entropy). Used to seed the dashboard
// login wall during first-run onboarding.
//
// Panics if crypto/rand is unavailable, which only happens on broken
// systems where the daemon wouldn't work anyway.
func GeneratePassphrase() string {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
