package webui

import "testing"

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
