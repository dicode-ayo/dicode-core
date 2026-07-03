package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// cmdRelock's routing and early-validation paths are hermetic: runtime
// presence is decided by file discovery, and each core's fail-fast checks run
// before any toolchain is provisioned, so none of these touch the network.
func TestCmdRelock_RoutingAndEarlyErrors(t *testing.T) {
	empty := t.TempDir() // exists, but contains neither task.ts nor task.py

	// Python-only tree whose dep-declaring task has no sidecar: the deno pass
	// must be skipped (no task.ts) and the python pass must fail --check
	// before uv is provisioned.
	pyOnly := t.TempDir()
	mustWrite(t, filepath.Join(pyOnly, "t", "task.py"), pep723Block)

	// Deno-only tree without a committed deno.lock: the deno pass must run
	// (and fail --check on the missing lock, before Deno is provisioned)
	// without the python pass masking or preceding it.
	denoOnly := t.TempDir()
	mustWrite(t, filepath.Join(denoOnly, "t", "task.ts"), "export default () => {};\n")

	// Mixed tree (both runtimes present): the deno pass must NOT be skipped
	// just because task.py files also exist — --check still trips on the
	// missing deno.lock first. The complementary direction (python pass
	// running after a successful deno pass) needs the provisioned toolchains,
	// so it is covered by CI's `dicode relock --check` on the real mixed
	// tasks/ tree rather than here.
	mixed := t.TempDir()
	mustWrite(t, filepath.Join(mixed, "d", "task.ts"), "export default () => {};\n")
	mustWrite(t, filepath.Join(mixed, "p", "task.py"), pep723Block)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unknown flag", []string{"--nope"}, "unknown flag"},
		{"two dirs", []string{"a", "b"}, "only one dir"},
		{"no scripts of either runtime", []string{empty}, "no task.ts or task.py"},
		{"python-only: deno skipped, python enforced", []string{"--check", pyOnly}, "no lock sidecar"},
		{"deno-only: deno enforced", []string{"--check", denoOnly}, "no deno.lock"},
		{"mixed tree: deno pass not skipped", []string{"--check", mixed}, "no deno.lock"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := cmdRelock(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

// A tree whose only Python tasks are dependency-free must pass the unified
// --check without provisioning any toolchain (the deno pass is skipped, the
// python pass has nothing to pin).
func TestCmdRelock_PythonOnlyNoDeps(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "t", "task.py"), "print('no deps')\n")

	if err := cmdRelock([]string{"--check", root}); err != nil {
		t.Fatalf("unified check on dep-free python tree: %v", err)
	}
}
