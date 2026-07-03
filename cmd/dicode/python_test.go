package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pep723Block = "# /// script\n# dependencies = [\"httpx>=0.27\"]\n# ///\nimport httpx\n"

func TestFindPythonTaskScripts(t *testing.T) {
	root := t.TempDir()
	// Two task.py (one nested), plus decoys that must be ignored.
	mustWrite(t, filepath.Join(root, "a", "task.py"), "")
	mustWrite(t, filepath.Join(root, "b", "nested", "task.py"), "")
	mustWrite(t, filepath.Join(root, "b", "task.test.py"), "") // not an entrypoint
	mustWrite(t, filepath.Join(root, "c", "task.ts"), "")      // other runtime

	got, err := findPythonTaskScripts(root)
	if err != nil {
		t.Fatalf("findPythonTaskScripts: %v", err)
	}
	want := []string{
		filepath.Join(root, "a", "task.py"),
		filepath.Join(root, "b", "nested", "task.py"),
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

// The early validation paths must fail before provisioning uv, so they are
// testable without the toolchain or network. Mirrors
// TestCmdDenoRelock_EarlyErrors.
func TestCmdPythonRelock_EarlyErrors(t *testing.T) {
	empty := t.TempDir() // exists, but contains no task.py

	// A dep-declaring task with no committed sidecar must fail --check
	// before uv is ever provisioned.
	missingLock := t.TempDir()
	mustWrite(t, filepath.Join(missingLock, "t", "task.py"), pep723Block)

	// A sidecar whose script no longer has a PEP 723 block is drift too:
	// the pin no longer governs anything.
	orphan := t.TempDir()
	mustWrite(t, filepath.Join(orphan, "t", "task.py"), "print('no deps')\n")
	mustWrite(t, filepath.Join(orphan, "t", "task.py.lock"), "version = 1\n")

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unknown flag", []string{"--nope"}, "unknown flag"},
		{"two dirs", []string{"a", "b"}, "only one dir"},
		{"no scripts", []string{empty}, "no task.py scripts"},
		{"check without sidecar", []string{"--check", missingLock}, "no lock sidecar"},
		{"check with orphaned sidecar", []string{"--check", orphan}, "orphaned lock sidecar"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := cmdPythonRelock(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

// Scripts without a PEP 723 block have nothing to pin: both modes must
// succeed without touching uv (hermetic — no toolchain, no network), so a
// tree of dependency-free Python tasks never fails CI.
func TestCmdPythonRelock_NoInlineMetadata(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "t", "task.py"), "print('no deps')\n")

	if err := cmdPythonRelock([]string{root}); err != nil {
		t.Fatalf("write mode: %v", err)
	}
	if err := cmdPythonRelock([]string{"--check", root}); err != nil {
		t.Fatalf("check mode: %v", err)
	}
}

// Write mode removes a sidecar whose script lost its PEP 723 block, instead
// of leaving a dead pin around for --check to trip on forever. Runs before
// uv provisioning (no lockable scripts remain), so it is hermetic.
func TestCmdPythonRelock_RemovesOrphanedSidecar(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "t", "task.py")
	sidecar := script + ".lock"
	mustWrite(t, script, "print('no deps')\n")
	mustWrite(t, sidecar, "version = 1\n")

	if err := cmdPythonRelock([]string{root}); err != nil {
		t.Fatalf("write mode: %v", err)
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Fatalf("orphaned sidecar still present (stat err=%v)", err)
	}
}
