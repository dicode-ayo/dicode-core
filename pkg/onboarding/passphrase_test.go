package onboarding

import (
	"regexp"
	"testing"
)

func TestGeneratePassphrase_Length(t *testing.T) {
	got := GeneratePassphrase()
	if len(got) != 24 {
		t.Errorf("len = %d; want 24 (got %q)", len(got), got)
	}
}

// URL-safe base64 (no padding) = [A-Za-z0-9_-].
var urlSafeB64 = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func TestGeneratePassphrase_URLSafeAlphabet(t *testing.T) {
	got := GeneratePassphrase()
	if !urlSafeB64.MatchString(got) {
		t.Errorf("passphrase %q contains non-URL-safe-base64 characters", got)
	}
}

func TestGeneratePassphrase_Uniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		p := GeneratePassphrase()
		if _, dup := seen[p]; dup {
			t.Fatalf("collision after %d iterations: %q", i, p)
		}
		seen[p] = struct{}{}
	}
}
