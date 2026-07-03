package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// parseRelockArgs parses the argument shape shared by `dicode deno relock`
// and `dicode python relock`:
//
//	relock [--check] [dir]     (dir defaults to "tasks")
//
// tool names the subcommand in usage/error text ("" for the runtime-spanning
// `dicode relock` frontend). showedHelp is true when --help/-h was handled
// (the caller should return nil without doing work). Extracting this keeps
// the relock frontends from drifting apart.
func parseRelockArgs(tool string, args []string) (check bool, dir string, showedHelp bool, err error) {
	name := "dicode relock"
	if tool != "" {
		name = "dicode " + tool + " relock"
	}
	for _, a := range args {
		switch {
		case a == "--check":
			check = true
		case a == "--help", a == "-h":
			fmt.Fprintf(os.Stderr, "Usage: %s [--check] [dir]   (dir defaults to \"tasks\")\n", name)
			return false, "", true, nil
		case len(a) > 0 && a[0] == '-':
			return false, "", false, fmt.Errorf("unknown flag %q — usage: %s [--check] [dir]", a, name)
		default:
			if dir != "" {
				return false, "", false, fmt.Errorf("unexpected argument %q — only one dir is allowed", a)
			}
			dir = a
		}
	}
	if dir == "" {
		dir = "tasks"
	}
	return check, dir, false, nil
}

// cmdRelock implements `dicode relock [--check] [dir]` — the runtime-spanning
// frontend over the deno and python lock passes. It runs each runtime's
// relock core for the runtimes actually present under dir (task.ts →
// deno.lock, task.py → task.py.lock sidecars), skipping absent ones, and
// errors only when the tree contains neither. `dicode deno relock` and
// `dicode python relock` remain as single-runtime entry points delegating to
// the same cores.
func cmdRelock(args []string) error {
	check, dir, showedHelp, err := parseRelockArgs("", args)
	if err != nil || showedHelp {
		return err
	}

	denoScripts, err := findTaskFiles(dir, "task.ts")
	if err != nil {
		return err
	}
	pyScripts, err := findTaskFiles(dir, "task.py")
	if err != nil {
		return err
	}
	if len(denoScripts) == 0 && len(pyScripts) == 0 {
		return fmt.Errorf("no task.ts or task.py scripts found under %s", dir)
	}

	if len(denoScripts) > 0 {
		if err := runDenoRelock(check, dir); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(os.Stderr, "dicode: no task.ts under %s — skipping deno lock\n", dir)
	}
	if len(pyScripts) > 0 {
		if err := runPythonRelock(check, dir); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(os.Stderr, "dicode: no task.py under %s — skipping python lock\n", dir)
	}
	return nil
}

// findTaskFiles returns every file named name under dir, sorted for a stable
// command line (deterministic lock/error ordering). Shared by the deno
// (task.ts) and python (task.py) relock walkers.
func findTaskFiles(dir, name string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == name {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s for %s: %w", dir, name, err)
	}
	sort.Strings(out)
	return out, nil
}
