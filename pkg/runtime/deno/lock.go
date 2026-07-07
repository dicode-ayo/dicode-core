package deno

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/dicode/dicode/internal/fsutil"
	denopkg "github.com/dicode/dicode/pkg/deno"
)

// staleLockSignature is the substring Deno prints to stderr when a `--frozen`
// run is rejected because the dependency graph drifted from the lockfile:
//
//	error: The lockfile is out of date. Run `deno install --frozen=false`, ...
//
// Matching it lets the runtime tell a stale lock (mechanically recoverable by
// regenerating the lock) apart from an ordinary dependency/type error.
const staleLockSignature = "lockfile is out of date"

// Relock regenerates (frozen=false) or verifies (frozen=true) the shared
// deno.lock covering every task.ts entrypoint under dir, using the exact Deno
// version dicode runs tasks with (provisioned on demand). A single entrypoint
// would prune the shared lock, so all are passed together. Deno's diff/download
// chatter is written to out. Returns the entrypoint count. This is the core
// behind both `dicode deno relock` and the runtime's stale-lock auto-recovery.
func Relock(ctx context.Context, dir string, frozen bool, out io.Writer) (int, error) {
	entrypoints, err := findEntrypoints(dir)
	if err != nil {
		return 0, err
	}
	if len(entrypoints) == 0 {
		return 0, fmt.Errorf("no task.ts entrypoints found under %s", dir)
	}

	denoPath, err := denopkg.EnsureDeno(denopkg.DefaultVersion)
	if err != nil {
		return 0, fmt.Errorf("provision deno %s: %w", denopkg.DefaultVersion, err)
	}

	lockPath := filepath.Join(dir, "deno.lock")
	args := []string{"cache"}
	if frozen {
		args = append(args, "--frozen")
	} else {
		args = append(args, "--frozen=false")
	}
	args = append(args, "--lock="+lockPath)
	if cfg := filepath.Join(dir, "deno.json"); fsutil.Exists(cfg) {
		args = append(args, "--config="+cfg)
	}
	args = append(args, entrypoints...)

	cmd := exec.CommandContext(ctx, denoPath, args...) //nolint:gosec
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return 0, err
	}
	return len(entrypoints), nil
}

// findEntrypoints returns every task.ts under dir, sorted for a stable command
// line so the regenerated lock is deterministic.
func findEntrypoints(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "task.ts" {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s for task.ts: %w", dir, err)
	}
	sort.Strings(out)
	return out, nil
}
