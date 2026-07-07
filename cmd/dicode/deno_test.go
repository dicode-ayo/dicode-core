package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
