package onboarding

import (
	"bytes"
	"strings"
	"testing"
)

// TestPrintPINSecret_WritesToTTYWhenAvailable verifies the fallback path
// when /dev/tty is not reachable (e.g., CI runners without a controlling
// terminal): the PIN still needs to go SOMEWHERE so the user isn't stuck.
// Stdout is used as a fallback — its accessibility is the caller's
// responsibility.
func TestPrintPINSecret_FallbackToStdout(t *testing.T) {
	var buf bytes.Buffer
	if err := printPINSecret(&buf, "123456", "http://127.0.0.1:1234/"); err != nil {
		t.Fatalf("printPINSecret: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "123456") {
		t.Errorf("output missing PIN: %q", got)
	}
	if !strings.Contains(got, "http://127.0.0.1:1234/") {
		t.Errorf("output missing URL: %q", got)
	}
}
