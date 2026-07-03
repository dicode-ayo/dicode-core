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
// tool names the subcommand in usage/error text. showedHelp is true when
// --help/-h was handled (the caller should return nil without doing work).
// Extracting this keeps the two relock frontends from drifting apart.
func parseRelockArgs(tool string, args []string) (check bool, dir string, showedHelp bool, err error) {
	for _, a := range args {
		switch {
		case a == "--check":
			check = true
		case a == "--help", a == "-h":
			fmt.Fprintf(os.Stderr, "Usage: dicode %s relock [--check] [dir]   (dir defaults to \"tasks\")\n", tool)
			return false, "", true, nil
		case len(a) > 0 && a[0] == '-':
			return false, "", false, fmt.Errorf("unknown flag %q — usage: dicode %s relock [--check] [dir]", a, tool)
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
