package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindTaskEntrypoints(t *testing.T) {
	root := t.TempDir()
	// Two task.ts (one nested), plus decoys that must be ignored.
	mustWrite(t, filepath.Join(root, "a", "task.ts"), "")
	mustWrite(t, filepath.Join(root, "b", "nested", "task.ts"), "")
	mustWrite(t, filepath.Join(root, "b", "task.test.ts"), "") // not an entrypoint
	mustWrite(t, filepath.Join(root, "c", "task.py"), "")      // other runtime

	got, err := findTaskEntrypoints(root)
	if err != nil {
		t.Fatalf("findTaskEntrypoints: %v", err)
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

// The early validation paths must fail before provisioning Deno, so they are
// testable without the toolchain or network.
func TestCmdDenoRelock_EarlyErrors(t *testing.T) {
	empty := t.TempDir() // exists, but contains no task.ts and no deno.lock
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unknown flag", []string{"--nope"}, "unknown flag"},
		{"two dirs", []string{"a", "b"}, "only one dir"},
		{"check without lock", []string{"--check", empty}, "no deno.lock"},
		{"no entrypoints", []string{empty}, "no task.ts entrypoints"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := cmdDenoRelock(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
