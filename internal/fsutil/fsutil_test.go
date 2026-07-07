package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")

	if err := WriteFileAtomic(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first" {
		t.Errorf("content = %q, want %q", got, "first")
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %o, want %o", fi.Mode().Perm(), 0o600)
		}
	}

	// Overwrite replaces content and mode.
	if err := WriteFileAtomic(path, []byte("second"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic overwrite: %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Errorf("content after overwrite = %q, want %q", got, "second")
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o644 {
			t.Errorf("mode after overwrite = %o, want %o", fi.Mode().Perm(), 0o644)
		}
	}

	// No temp files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("leftover files in dir: %v", names)
	}

	// Missing parent directory fails and creates nothing.
	if err := WriteFileAtomic(filepath.Join(dir, "no-such-dir", "x"), []byte("x"), 0o600); err == nil {
		t.Error("missing parent dir: want error, got nil")
	}
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if Exists(path) {
		t.Error("Exists(missing) = true")
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !Exists(path) {
		t.Error("Exists(file) = false")
	}
	if !Exists(dir) {
		t.Error("Exists(dir) = false")
	}
}

func TestFindUp(t *testing.T) {
	// root/a/b/c with the needle at root/a.
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	needle := filepath.Join(root, "a", "needle.txt")
	if err := os.WriteFile(needle, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Found two levels up.
	got, ok := FindUp(deep, "needle.txt", 2)
	if !ok || got != needle {
		t.Errorf("FindUp(deep, 2) = (%q, %v), want (%q, true)", got, ok, needle)
	}

	// Found in the start dir itself with maxParents=0.
	got, ok = FindUp(filepath.Join(root, "a"), "needle.txt", 0)
	if !ok || got != needle {
		t.Errorf("FindUp(a, 0) = (%q, %v), want (%q, true)", got, ok, needle)
	}

	// maxParents boundary: needle is exactly 2 levels up, so 1 is not enough.
	if got, ok := FindUp(deep, "needle.txt", 1); ok {
		t.Errorf("FindUp(deep, 1) = (%q, true), want not found", got)
	}

	// Not found at all.
	if got, ok := FindUp(deep, "no-such-file", 100); ok {
		t.Errorf("FindUp(no-such-file) = (%q, true), want not found", got)
	}

	// rel may be a relative path, not just a name.
	got, ok = FindUp(deep, filepath.Join("a", "needle.txt"), 3)
	if !ok || got != needle {
		t.Errorf("FindUp(rel path) = (%q, %v), want (%q, true)", got, ok, needle)
	}
}
