package deno

import (
	"os"
	"path/filepath"
	"testing"

	pkgruntime "github.com/dicode/dicode/pkg/runtime"
)

// The exact stderr Deno 2.x writes when a `--frozen` run is rejected for lock
// drift. Pins that staleLockSignature keeps matching the real message.
const denoFrozenStaleErr = "error: The lockfile is out of date. Run `deno install --frozen=false`, or rerun with `--frozen=false` to update it.\nchanges:\n"

func TestStaleLockSignature_MatchesDenoFrozenError(t *testing.T) {
	s := pkgruntime.NewLockErrSniffer(staleLockSignature)
	s.Write([]byte(denoFrozenStaleErr)) //nolint:errcheck
	if !s.StaleLock() {
		t.Errorf("staleLockSignature %q did not match Deno's frozen error", staleLockSignature)
	}
}

func TestStaleLockSignature_IgnoresOrdinaryError(t *testing.T) {
	s := pkgruntime.NewLockErrSniffer(staleLockSignature)
	s.Write([]byte("error: Uncaught (in promise) TypeError: fetch failed\n")) //nolint:errcheck
	if s.StaleLock() {
		t.Error("ordinary Deno error must not be treated as a stale lock")
	}
}

func TestFindEntrypoints(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a/task.ts")
	write("b/nested/task.ts")
	write("b/task.test.ts") // not an entrypoint
	write("c/task.py")      // other runtime

	got, err := findEntrypoints(root)
	if err != nil {
		t.Fatalf("findEntrypoints: %v", err)
	}
	want := []string{
		filepath.Join(root, "a", "task.ts"),
		filepath.Join(root, "b", "nested", "task.ts"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] { // also asserts sorted order
			t.Errorf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}
