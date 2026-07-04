package pathguard

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWithin(t *testing.T) {
	sep := string(filepath.Separator)
	cases := []struct {
		name string
		root string
		p    string
		want bool
	}{
		{"equal", "/data/tasks", "/data/tasks", true},
		{"child", "/data/tasks", "/data/tasks/foo", true},
		{"nested child", "/data/tasks", "/data/tasks/a/b/c", true},
		{"unclean child", "/data/tasks/", "/data/tasks/a/./b", true},
		{"sibling", "/data/tasks", "/data/other", false},
		{"parent", "/data/tasks", "/data", false},
		{"prefix confusion", "/etc", "/etc-evil", false},
		{"prefix confusion nested", "/etc", "/etc-evil/passwd", false},
		{"dotdot escape", "/data/tasks", "/data/tasks/../../etc/passwd", false},
		{"dotdot inside", "/data/tasks", "/data/tasks/a/../b", true},
		{"root contains all", "/", "/etc/passwd", true},
		{"root equals root", "/", "/", true},
		{"relative pair", "data/tasks", "data/tasks/foo", true},
		{"relative escape", "data/tasks", "data/other", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Within(tc.root, tc.p)
			if err != nil {
				t.Fatalf("Within(%q, %q) error: %v", tc.root, tc.p, err)
			}
			if got != tc.want {
				t.Errorf("Within(%q, %q) = %v, want %v (sep %q)", tc.root, tc.p, got, tc.want, sep)
			}
		})
	}
}

func TestWithinResolved_Lexical(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "sub", "file.txt")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	ok, err := WithinResolved(root, inside)
	if err != nil || !ok {
		t.Errorf("existing child: got (%v, %v), want (true, nil)", ok, err)
	}

	// Traversal via "..".
	escape := filepath.Join(root, "sub", "..", "..", "elsewhere")
	ok, err = WithinResolved(root, escape)
	if err != nil {
		t.Fatalf("dotdot escape errored: %v", err)
	}
	if ok {
		t.Error("dotdot escape reported within root")
	}

	// Not-yet-existing path under root is still within (tail re-appended).
	ok, err = WithinResolved(root, filepath.Join(root, "does", "not", "exist"))
	if err != nil || !ok {
		t.Errorf("missing child: got (%v, %v), want (true, nil)", ok, err)
	}

	// Root itself counts as within.
	ok, err = WithinResolved(root, root)
	if err != nil || !ok {
		t.Errorf("root itself: got (%v, %v), want (true, nil)", ok, err)
	}

	// Missing root fails closed.
	if _, err := WithinResolved(filepath.Join(root, "no-such-root"), inside); err == nil {
		t.Error("missing root: want error, got nil")
	}
}

func TestWithinResolved_SymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks not reliable on windows")
	}
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Symlink inside root pointing outside: lexically contained, physically not.
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(link, "secret.txt")
	if ok, err := Within(root, target); err != nil || !ok {
		t.Fatalf("lexical Within should accept the unresolved symlink path, got (%v, %v)", ok, err)
	}
	ok, err := WithinResolved(root, target)
	if err != nil {
		t.Fatalf("WithinResolved errored: %v", err)
	}
	if ok {
		t.Error("symlink escape reported within root")
	}

	// Symlinked ancestor of a not-yet-existing path must also be caught.
	ok, err = WithinResolved(root, filepath.Join(link, "new", "file"))
	if err != nil {
		t.Fatalf("WithinResolved (missing tail) errored: %v", err)
	}
	if ok {
		t.Error("symlink escape with missing tail reported within root")
	}

	// A symlinked root resolving to the same physical tree still contains
	// its (resolved) children.
	rootLink := filepath.Join(base, "rootlink")
	if err := os.Symlink(root, rootLink); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "file.txt")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := WithinResolved(rootLink, filepath.Join(rootLink, "file.txt")); err != nil || !ok {
		t.Errorf("symlinked root: got (%v, %v), want (true, nil)", ok, err)
	}
}

func TestWithinResolved_PrefixConfusion(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "etc")
	evil := filepath.Join(base, "etc-evil")
	for _, d := range []string{root, evil} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ok, err := WithinResolved(root, filepath.Join(evil, "passwd"))
	if err != nil {
		t.Fatalf("WithinResolved errored: %v", err)
	}
	if ok {
		t.Errorf("%q reported within %q", evil, root)
	}
}

func TestResolveExisting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks not reliable on windows")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// t.TempDir itself can sit behind a symlink (e.g. /tmp on macOS).
	canonReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}

	got, err := ResolveExisting(filepath.Join(link, "missing", "tail"))
	if err != nil {
		t.Fatalf("ResolveExisting errored: %v", err)
	}
	want := filepath.Join(canonReal, "missing", "tail")
	if got != want {
		t.Errorf("ResolveExisting = %q, want %q", got, want)
	}
}
