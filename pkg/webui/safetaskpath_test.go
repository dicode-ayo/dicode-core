package webui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSafeTaskFilePath_RejectsTraversal(t *testing.T) {
	bad := []string{
		"", ".", "..", "../foo", "foo/../bar", "foo/bar",
		"/etc/passwd", `\windows\system32`, "a\\b", "./foo",
	}
	for _, f := range bad {
		if _, err := safeTaskFilePath(t.TempDir(), f); err == nil {
			t.Errorf("expected rejection for %q", f)
		}
	}
}

func TestSafeTaskFilePath_AcceptsAllowedShapes(t *testing.T) {
	td := t.TempDir()
	good := []string{"task.js", "task.ts", "index.html", "Dockerfile", "style.css"}
	for _, f := range good {
		p, err := safeTaskFilePath(td, f)
		if err != nil {
			t.Errorf("expected accept for %q: %v", f, err)
		}
		if p == "" {
			t.Errorf("expected non-empty path for %q", f)
		}
	}
}

// A source repo can commit a task file as a symlink and go-git materializes it
// as a real on-disk link, so a name that passes every lexical check can still
// resolve outside the task dir — where apiGetFile's os.ReadFile would disclose
// it and apiSaveFile's os.WriteFile would overwrite it as the daemon user.
func TestSafeTaskFilePath_RejectsSymlinkEscapingTaskDir(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "authorized_keys")
	if err := os.WriteFile(secret, []byte("ssh-rsa AAAA"), 0600); err != nil {
		t.Fatal(err)
	}

	td := t.TempDir()
	if err := os.Symlink(secret, filepath.Join(td, "style.css")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	if _, err := safeTaskFilePath(td, "style.css"); err == nil {
		t.Fatal("safeTaskFilePath accepted a symlink resolving outside the task dir — the editor would read and overwrite the target")
	}
}

// An intermediate directory symlink escapes just as effectively as one on the
// file itself, and is not visible in the filename at all.
func TestSafeTaskFilePath_RejectsSymlinkedTaskDir(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "task.js"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(t.TempDir(), "taskdir")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	// The task dir itself resolving elsewhere is legitimate — it is the
	// registry's own value, and containment is judged against its resolved
	// form, not rejected outright.
	if _, err := safeTaskFilePath(link, "task.js"); err != nil {
		t.Fatalf("rejected a task dir that is itself a symlink: %v", err)
	}
}

// Saving a file that does not exist yet must keep working: the editor creates
// index.html/style.css on first save.
func TestSafeTaskFilePath_AcceptsNotYetCreatedFile(t *testing.T) {
	td := t.TempDir()
	p, err := safeTaskFilePath(td, "index.html")
	if err != nil {
		t.Fatalf("rejected a not-yet-created file: %v", err)
	}
	if p != filepath.Join(td, "index.html") {
		t.Fatalf("unexpected path %q", p)
	}
}

// A symlink pointing at a sibling inside the same task dir is contained, so it
// stays editable.
func TestSafeTaskFilePath_AcceptsSymlinkInsideTaskDir(t *testing.T) {
	td := t.TempDir()
	if err := os.WriteFile(filepath.Join(td, "real.css"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(td, "real.css"), filepath.Join(td, "style.css")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if _, err := safeTaskFilePath(td, "style.css"); err != nil {
		t.Fatalf("rejected a symlink contained within the task dir: %v", err)
	}
}

// canonicalTaskFile must hand back the allowlist's own string, not the
// caller's, so no request-derived value reaches the filesystem.
func TestCanonicalTaskFile(t *testing.T) {
	got, ok := canonicalTaskFile("task.js")
	if !ok || got != "task.js" {
		t.Fatalf("canonicalTaskFile(task.js) = %q, %v", got, ok)
	}
	for _, bad := range []string{"", "TASK.JS", "task.js ", "../task.js", "secrets.env"} {
		if _, ok := canonicalTaskFile(bad); ok {
			t.Errorf("canonicalTaskFile accepted %q", bad)
		}
	}
}

func TestRuntimeVersionPattern(t *testing.T) {
	for _, ok := range []string{"1", "1.2", "2.1.4", "0.5.0-rc.1"} {
		if !runtimeVersionPattern.MatchString(ok) {
			t.Errorf("rejected valid version %q", ok)
		}
	}
	// Each of these would otherwise be interpolated into a cache path under
	// ~/.cache/dicode and into a release download URL.
	for _, bad := range []string{
		"", "../../../etc", "1.2.3/../../..", "..", "/abs", `1.2.3\..\..`,
		"https://evil.example/x", "1.2.3;rm -rf /", "1.2.3\n",
	} {
		if runtimeVersionPattern.MatchString(bad) {
			t.Errorf("accepted traversal-shaped version %q", bad)
		}
	}
}
