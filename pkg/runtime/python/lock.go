package python

import (
	"fmt"
	"os"
)

// LockSidecarPath returns the path of the per-script uv lock sidecar for a
// PEP 723 inline script, as written by `uv lock --script <script>`:
// the script path with ".lock" appended (task.py → task.py.lock).
func LockSidecarPath(scriptPath string) string {
	return scriptPath + ".lock"
}

// HasInlineMetadata reports whether src contains a PEP 723 inline script
// metadata block (`# /// script` … `# ///`). Only scripts with such a block
// have dependencies for uv to resolve, so only they can (and should) carry a
// lock sidecar.
func HasInlineMetadata(src []byte) bool {
	block, _ := extractPEP723(string(src))
	return block != ""
}

// stageLockSidecar makes the task's committed lock sidecar visible to uv for
// a wrapper run. uv discovers a script's lockfile strictly by filename
// (<script>.lock next to the script), and the runtime executes a temporary
// wrapper file rather than task.py itself — so the sidecar is copied to
// <wrapper>.lock. Returns the staged path and true when a sidecar exists;
// ("", false, nil) when the task has no lock (the run then proceeds unlocked,
// matching how the Deno runtime degrades when no deno.lock is present).
// A sidecar that exists but cannot be read/copied is a hard error: a task
// that declares a lock must not silently fall back to unpinned resolution.
func stageLockSidecar(scriptPath, wrapperPath string) (string, bool, error) {
	src := LockSidecarPath(scriptPath)
	data, err := os.ReadFile(src) //nolint:gosec
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read lock sidecar %s: %w", src, err)
	}
	dst := LockSidecarPath(wrapperPath)
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return "", false, fmt.Errorf("stage lock sidecar %s: %w", dst, err)
	}
	return dst, true, nil
}

// buildUvRunArgs assembles the argv (minus the uv binary itself) for a task
// run. When locked is true — a lock sidecar has been staged next to the
// wrapper — `--locked` makes uv fail loudly if the lock is out of date with
// the script's inline metadata, instead of silently re-resolving (and
// rewriting the sidecar). Without a sidecar the flag is omitted so tasks with
// no lock (including ones with no dependencies at all) keep running.
func buildUvRunArgs(wrapperPath string, locked bool) []string {
	args := []string{"run"}
	if locked {
		args = append(args, "--locked")
	}
	return append(args, wrapperPath)
}
