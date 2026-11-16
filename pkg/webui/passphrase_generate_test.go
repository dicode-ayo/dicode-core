package webui

import (
	"bytes"
	"context"
	"io"
	"os"
	"regexp"
	"testing"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/trigger"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
)

// printedPassphrase matches the boxed plaintext line ensurePassphrase prints.
var printedPassphrase = regexp.MustCompile(`(?m)^║\s+([A-Za-z0-9_-]+)\s+║$`)

func TestEnsurePassphrase_GeneratesCanonicalLength(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{Auth: true, BcryptCost: bcrypt.MinCost}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	ensureErr := srv.ensurePassphrase(context.Background())
	os.Stdout = stdout
	w.Close()
	var out bytes.Buffer
	if _, err := io.Copy(&out, r); err != nil {
		t.Fatal(err)
	}
	if ensureErr != nil {
		t.Fatalf("ensurePassphrase: %v", ensureErr)
	}

	m := printedPassphrase.FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("no passphrase line in output:\n%s", out.String())
	}
	plaintext := m[1]
	if len(plaintext) != 43 {
		t.Errorf("generated passphrase len = %d; want 43 (got %q)", len(plaintext), plaintext)
	}

	stored, _ := srv.passphraseStore.get(context.Background())
	if err := bcrypt.CompareHashAndPassword([]byte(stored), []byte(plaintext)); err != nil {
		t.Errorf("printed passphrase does not verify against stored hash: %v", err)
	}
}
