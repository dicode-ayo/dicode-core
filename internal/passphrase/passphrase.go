// Package passphrase generates dicode's auth passphrases.
package passphrase

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
)

// Generate returns a 43-character URL-safe base64 token (32 random bytes,
// 256 bits of entropy).
func Generate() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

const (
	bannerTop    = "╔══════════════════════════════════════════════════════════════╗"
	bannerBottom = "╚══════════════════════════════════════════════════════════════╝"
)

// PrintBanner writes title, value and footer lines to w inside a box. A
// line longer than 60 characters is printed in full and overflows the
// right border; value is never truncated.
func PrintBanner(w io.Writer, title, value string, footer ...string) {
	lines := append([]string{title, "", value, ""}, footer...)
	fmt.Fprintln(w, bannerTop)
	for _, l := range lines {
		fmt.Fprintf(w, "║  %-60s║\n", l) // lgtm[go/clear-text-logging] intentional operator-terminal display, not a log
	}
	fmt.Fprintln(w, bannerBottom)
}
