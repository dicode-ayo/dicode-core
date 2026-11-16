package webui

import (
	"bytes"
	"context"
	"io"
	"os"
	"regexp"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// printedPassphrase matches the boxed plaintext line ensurePassphrase prints.
var printedPassphrase = regexp.MustCompile(`(?m)^║\s+([A-Za-z0-9_-]+)\s+║$`)

func TestEnsurePassphrase_GeneratesCanonicalLength(t *testing.T) {
	srv := newAuthServer(t, "")
	srv.cfg.Server.BcryptCost = bcrypt.MinCost

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	stdout := os.Stdout
	os.Stdout = w
	func() {
		defer func() {
			os.Stdout = stdout
			_ = w.Close()
		}()
		err = srv.ensurePassphrase(context.Background())
	}()
	out := <-done
	if err != nil {
		t.Fatalf("ensurePassphrase: %v", err)
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
}
