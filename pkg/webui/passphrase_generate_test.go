package webui

import (
	"bytes"
	"context"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

// printedPassphrase matches the boxed plaintext line ensurePassphrase prints.
var printedPassphrase = regexp.MustCompile(`(?m)^║\s+([A-Za-z0-9_-]+)\s+║$`)

// captureStdout returns everything fn writes to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = stdout })
	done := make(chan []byte)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.Bytes()
	}()
	fn()
	os.Stdout = stdout
	w.Close()
	return string(<-done)
}

func TestEnsurePassphrase_GeneratesCanonicalLength(t *testing.T) {
	srv := newAuthServer(t, "")
	srv.cfg.Server.BcryptCost = bcrypt.MinCost

	var ensureErr error
	out := captureStdout(t, func() { ensureErr = srv.ensurePassphrase(context.Background()) })
	if ensureErr != nil {
		t.Fatalf("ensurePassphrase: %v", ensureErr)
	}

	m := printedPassphrase.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no passphrase line in output:\n%s", out)
	}
	plaintext := m[1]
	if len(plaintext) != 43 {
		t.Errorf("generated passphrase len = %d; want 43 (got %q)", len(plaintext), plaintext)
	}

	stored, err := srv.passphraseStore.get(context.Background())
	if err != nil {
		t.Fatalf("read stored passphrase: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(stored), []byte(plaintext)); err != nil {
		t.Errorf("printed passphrase does not verify against stored hash: %v", err)
	}

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	want := utf8.RuneCountInString(lines[0])
	for _, line := range lines {
		if got := utf8.RuneCountInString(line); got != want {
			t.Errorf("banner line width = %d; want %d (top border): %q", got, want, line)
		}
	}
}
