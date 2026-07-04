package trigger

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// serveTaskAsset must reject a symlinked asset that escapes the task dir. Task
// dirs come from untrusted git/local sources, so a committed symlink
// (asset.html -> /etc/passwd, which carries an allowed extension) would sail
// through a purely lexical containment check and be followed by os.ReadFile.
// The symlink-resolving guard (pathguard.WithinResolved) must block it.
func TestServeTaskAsset_RejectsSymlinkEscape(t *testing.T) {
	taskDir := t.TempDir()

	// A secret file outside the task dir that the symlink will point at.
	outside := filepath.Join(t.TempDir(), "secret.html")
	if err := os.WriteFile(outside, []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	// asset.html -> ../.../secret.html (allowed extension, escapes taskDir).
	link := filepath.Join(taskDir, "asset.html")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	e := &Engine{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/hooks/t/asset.html", nil)
	e.serveTaskAsset(rec, req, taskDir, "asset.html")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("symlink-escaping asset: got status %d, want 403; body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() == "TOP SECRET" {
		t.Fatal("symlink-escaping asset leaked the out-of-tree file contents")
	}
}

// A legitimate (non-symlink) asset inside the task dir must still be served.
func TestServeTaskAsset_ServesRegularFile(t *testing.T) {
	taskDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(taskDir, "index.html"), []byte("<h1>ok</h1>"), 0o644); err != nil {
		t.Fatalf("write asset: %v", err)
	}

	e := &Engine{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/hooks/t/index.html", nil)
	e.serveTaskAsset(rec, req, taskDir, "index.html")

	if rec.Code != http.StatusOK {
		t.Fatalf("regular asset: got status %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "<h1>ok</h1>" {
		t.Fatalf("regular asset: unexpected body %q", rec.Body.String())
	}
}
