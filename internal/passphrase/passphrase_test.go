package passphrase

import (
	"regexp"
	"testing"
)

func TestGenerate_Length(t *testing.T) {
	got := Generate()
	if len(got) != 43 {
		t.Errorf("len = %d; want 43 (got %q)", len(got), got)
	}
}

// URL-safe base64 (no padding) = [A-Za-z0-9_-].
var urlSafeB64 = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func TestGenerate_URLSafeAlphabet(t *testing.T) {
	got := Generate()
	if !urlSafeB64.MatchString(got) {
		t.Errorf("passphrase %q contains non-URL-safe-base64 characters", got)
	}
}

func TestGenerate_Uniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		p := Generate()
		if _, dup := seen[p]; dup {
			t.Fatalf("collision after %d iterations: %q", i, p)
		}
		seen[p] = struct{}{}
	}
}
