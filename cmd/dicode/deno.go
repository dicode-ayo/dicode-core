package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	denopkg "github.com/dicode/dicode/pkg/deno"
)

// cmdDeno implements `dicode deno <subcommand>` — local, daemon-free helpers
// around the pinned Deno toolchain.
func cmdDeno(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: dicode deno relock [--check] [dir]")
	}
	switch args[0] {
	case "relock":
		return cmdDenoRelock(args[1:])
	default:
		return fmt.Errorf("unknown deno subcommand %q — supported: relock", args[0])
	}
}

// cmdDenoRelock regenerates or verifies a task source's deno.lock using the
// exact Deno version dicode itself runs tasks with (provisioned on demand), so
// the lock is deterministic across developer machines and CI regardless of any
// system Deno. Runs without the daemon and works on any task tree, so task
// developers can guard their own lockfiles the same way the buildin tasks do.
//
//	dicode deno relock [--check] [dir]
//
// dir defaults to "tasks". Without --check the lock is regenerated across all
// task.ts entrypoints under dir (a single entrypoint would prune the shared
// lock). With --check the lock is verified with --frozen and left untouched;
// exit is non-zero if it is stale.
func cmdDenoRelock(args []string) error {
	check := false
	dir := ""
	for _, a := range args {
		switch {
		case a == "--check":
			check = true
		case a == "--help", a == "-h":
			fmt.Fprintln(os.Stderr, `Usage: dicode deno relock [--check] [dir]   (dir defaults to "tasks")`)
			return nil
		case len(a) > 0 && a[0] == '-':
			return fmt.Errorf("unknown flag %q — usage: dicode deno relock [--check] [dir]", a)
		default:
			if dir != "" {
				return fmt.Errorf("unexpected argument %q — only one dir is allowed", a)
			}
			dir = a
		}
	}
	if dir == "" {
		dir = "tasks"
	}

	lockPath := filepath.Join(dir, "deno.lock")
	if check {
		if _, err := os.Stat(lockPath); err != nil {
			return fmt.Errorf("no deno.lock at %s (run `dicode deno relock %s` to create it)", lockPath, dir)
		}
	}

	entrypoints, err := findTaskEntrypoints(dir)
	if err != nil {
		return err
	}
	if len(entrypoints) == 0 {
		return fmt.Errorf("no task.ts entrypoints found under %s", dir)
	}

	denoPath, err := denopkg.EnsureDeno(denopkg.DefaultVersion)
	if err != nil {
		return fmt.Errorf("provision deno %s: %w", denopkg.DefaultVersion, err)
	}

	cacheArgs := []string{"cache"}
	if check {
		cacheArgs = append(cacheArgs, "--frozen")
	} else {
		cacheArgs = append(cacheArgs, "--frozen=false")
	}
	cacheArgs = append(cacheArgs, "--lock="+lockPath)
	if cfg := filepath.Join(dir, "deno.json"); fileExists(cfg) {
		cacheArgs = append(cacheArgs, "--config="+cfg)
	}
	cacheArgs = append(cacheArgs, entrypoints...)

	cmd := exec.Command(denoPath, cacheArgs...)
	// Deno's Check/Download/lockfile-diff chatter goes to the operator terminal;
	// stdout is kept clean for scripting.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if runErr := cmd.Run(); runErr != nil {
		if check {
			// deno prints the cause above — a lockfile diff if the lock is
			// stale, otherwise a dependency/config/type error. Don't assert
			// staleness; surface both so a non-lock failure isn't mistaken for
			// drift.
			return fmt.Errorf("deno cache --frozen failed (see output above); if %s is stale, run `dicode deno relock %s`: %w", lockPath, dir, runErr)
		}
		return fmt.Errorf("deno cache: %w", runErr)
	}

	if check {
		fmt.Fprintf(os.Stderr, "dicode: %s is up to date (%d entrypoints)\n", lockPath, len(entrypoints))
	} else {
		fmt.Fprintf(os.Stderr, "dicode: regenerated %s (%d entrypoints)\n", lockPath, len(entrypoints))
	}
	return nil
}

// findTaskEntrypoints returns every task.ts under dir, sorted for a stable
// command line (deterministic lock ordering).
func findTaskEntrypoints(dir string) ([]string, error) {
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

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
