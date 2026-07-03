package main

import (
	"fmt"
	"os"
	"os/exec"

	pythonpkg "github.com/dicode/dicode/pkg/runtime/python"
	uvpkg "github.com/dicode/dicode/pkg/uv"
)

// cmdPython implements `dicode python <subcommand>` — local, daemon-free
// helpers around the pinned uv toolchain. Mirrors cmdDeno.
func cmdPython(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: dicode python relock [--check] [dir]")
	}
	switch args[0] {
	case "relock":
		return cmdPythonRelock(args[1:])
	default:
		return fmt.Errorf("unknown python subcommand %q — supported: relock", args[0])
	}
}

// cmdPythonRelock regenerates or verifies the per-script uv lock sidecars
// (task.py.lock) for every Python task under dir, using the exact uv version
// dicode itself runs tasks with (provisioned on demand), so locks are
// deterministic across developer machines and CI regardless of any system uv.
// The Deno-parity counterpart of cmdDenoRelock (issue #465).
//
//	dicode python relock [--check] [dir]
//
// dir defaults to "tasks". Unlike Deno's single shared deno.lock, uv locks
// inline PEP 723 scripts individually: `uv lock --script task.py` writes a
// task.py.lock sidecar next to the script. Only scripts that carry a PEP 723
// metadata block are lockable; scripts without one are skipped (nothing to
// pin), and their leftover sidecars are removed (write mode) or flagged
// (--check). With --check the sidecars are verified with `uv lock --check`
// and left untouched; exit is non-zero if any is missing or stale.
func cmdPythonRelock(args []string) error {
	check, dir, showedHelp, err := parseRelockArgs("python", args)
	if err != nil || showedHelp {
		return err
	}

	scripts, err := findPythonTaskScripts(dir)
	if err != nil {
		return err
	}
	if len(scripts) == 0 {
		return fmt.Errorf("no task.py scripts found under %s", dir)
	}

	// Partition: scripts with a PEP 723 block get a lock sidecar; ones
	// without cannot be locked (uv has nothing to resolve) — any sidecar
	// they still carry is a leftover from a removed block.
	var lockable, orphanLocks []string
	for _, p := range scripts {
		src, err := os.ReadFile(p) //nolint:gosec
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		sidecar := pythonpkg.LockSidecarPath(p)
		if pythonpkg.HasInlineMetadata(src) {
			lockable = append(lockable, p)
			if !pythonpkg.HasRequiresPython(src) {
				fmt.Fprintf(os.Stderr, "dicode: warning: %s has no `requires-python` in its PEP 723 block; its lock resolves against the default Python (e.g. >=3.11 locally vs >=3.12 in CI) and may not be reproducible — pin one, e.g. `# requires-python = \">=3.11\"`\n", p)
			}
		} else if fileExists(sidecar) {
			orphanLocks = append(orphanLocks, sidecar)
		}
	}

	// Everything below that can fail fast does so before provisioning uv,
	// so these paths stay testable without the toolchain or network.
	if check {
		for _, p := range lockable {
			if sidecar := pythonpkg.LockSidecarPath(p); !fileExists(sidecar) {
				return fmt.Errorf("no lock sidecar at %s (run `dicode python relock %s` to create it)", sidecar, dir)
			}
		}
		if len(orphanLocks) > 0 {
			return fmt.Errorf("orphaned lock sidecar %s — its script has no PEP 723 block (run `dicode python relock %s` to remove it)", orphanLocks[0], dir)
		}
	} else {
		for _, sidecar := range orphanLocks {
			if err := os.Remove(sidecar); err != nil {
				return fmt.Errorf("remove orphaned lock sidecar %s: %w", sidecar, err)
			}
			fmt.Fprintf(os.Stderr, "dicode: removed orphaned lock sidecar %s\n", sidecar)
		}
	}

	if len(lockable) == 0 {
		fmt.Fprintf(os.Stderr, "dicode: no Python tasks with PEP 723 inline metadata under %s — nothing to lock\n", dir)
		return nil
	}

	uvPath, err := uvpkg.EnsureUv(uvpkg.DefaultVersion)
	if err != nil {
		return fmt.Errorf("provision uv %s: %w", uvpkg.DefaultVersion, err)
	}

	for _, p := range lockable {
		lockArgs := []string{"lock", "--script", p}
		if check {
			lockArgs = append(lockArgs, "--check")
		}
		cmd := exec.Command(uvPath, lockArgs...) // #nosec G204 — argv is the provisioned uv plus discovered task.py paths, no user shell injection.
		// uv's resolution chatter goes to the operator terminal; stdout is
		// kept clean for scripting (same contract as cmdDenoRelock).
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if runErr := cmd.Run(); runErr != nil {
			sidecar := pythonpkg.LockSidecarPath(p)
			if check {
				// uv prints the cause above — a lockfile diff if the sidecar
				// is stale, otherwise a resolution/metadata error. Surface
				// both so a non-lock failure isn't mistaken for drift.
				return fmt.Errorf("uv lock --check failed for %s (see output above); if %s is stale, run `dicode python relock %s`: %w", p, sidecar, dir, runErr)
			}
			return fmt.Errorf("uv lock --script %s: %w", p, runErr)
		}
	}

	if check {
		fmt.Fprintf(os.Stderr, "dicode: Python lock sidecars under %s are up to date (%d scripts)\n", dir, len(lockable))
	} else {
		fmt.Fprintf(os.Stderr, "dicode: regenerated lock sidecars under %s (%d scripts)\n", dir, len(lockable))
	}
	return nil
}

// findPythonTaskScripts returns every task.py under dir, sorted for a stable
// command line and deterministic error ordering. Mirrors findTaskEntrypoints.
func findPythonTaskScripts(dir string) ([]string, error) {
	return findTaskFiles(dir, "task.py")
}
