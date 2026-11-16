package passphrase

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
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

func TestPrintBanner_Width(t *testing.T) {
	var buf bytes.Buffer
	value := strings.Repeat("x", 43)
	PrintBanner(&buf, "title", value, "footer one", "footer two")
	out := buf.String()
	if !strings.Contains(out, value) {
		t.Fatalf("value missing from banner:\n%s", out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	want := utf8.RuneCountInString(lines[0])
	for _, line := range lines {
		if got := utf8.RuneCountInString(line); got != want {
			t.Errorf("banner line width = %d; want %d (top border): %q", got, want, line)
		}
	}

	buf.Reset()
	long := strings.Repeat("y", 75)
	PrintBanner(&buf, "title", long)
	if !strings.Contains(buf.String(), long) {
		t.Errorf("long value truncated:\n%s", buf.String())
	}
}
