package onboarding

import "github.com/dicode/dicode/internal/passphrase"

// GeneratePassphrase returns a 43-character URL-safe base64 token (32 random
// bytes, 256 bits of entropy).
func GeneratePassphrase() string { return passphrase.Generate() }
