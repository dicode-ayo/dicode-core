package task

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// maxHashedFileBytes bounds the contents read into the digest per file. The
// hash runs on every reconciler poll (~30s), so a committed large asset would
// re-read its full bytes each cycle. Files at or below this size are hashed by
// content; larger files fold in only a size+mtime descriptor — code files are
// small, so change-detection for task logic stays exact while bulk assets cost
// a stat instead of a full read.
const maxHashedFileBytes = 1 << 20 // 1 MiB

// heavyDirs are skipped wholesale during the walk: they hold no task code and
// can dominate walk cost. node_modules in particular can be a committed tree of
// thousands of files.
var heavyDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
}

// Hash computes a content hash over the regular files under dir, recursively,
// in sorted dir-relative path order. The runtime allows the task script to
// import any sibling file (the Deno sandbox allow-reads the whole task dir;
// Python imports sibling modules), so in-dir files are reachable code and form
// the fingerprint the approval gate (dicode.lock) keys re-approval off.
//
// Two bounded exclusions keep the per-poll cost flat without losing
// change-detection for task logic, which lives in small, top-level files:
//   - Files larger than maxHashedFileBytes fold in a size+mtime descriptor
//     instead of their contents (see the const); a content edit that preserves
//     both size and mtime is therefore not detected.
//   - The node_modules and .git subtrees are skipped wholesale (see heavyDirs).
//
// Consequently, code reachable only through those exclusions can change without
// re-triggering approval — acceptable under the trusted-author model, where the
// operator approves the source, not every transitive vendored file.
//
// Symlinks are never read through: the link's target string is folded in
// instead, so retargeting a link changes the hash without ever reading a
// file outside dir. Symlinked directories are not descended. Non-regular,
// non-symlink entries (sockets, devices) are skipped — reading them can
// block and they carry no task content.
//
// A missing dir hashes like an empty one (callers race task removal).
func Hash(dir string) (string, error) {
	type entry struct {
		rel    string
		isLink bool
		target string
	}
	var entries []entry
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != dir && heavyDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", path, err)
			}
			entries = append(entries, entry{rel: rel, isLink: true, target: target})
		case d.Type().IsRegular():
			entries = append(entries, entry{rel: rel})
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("hash %s: %w", dir, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })

	h := sha256.New()
	for _, e := range entries {
		// Path and kind act as separators so hash(A+B) != hash(AB) and a
		// regular file can never collide with a link whose target holds the
		// same bytes.
		kind := byte('f')
		if e.isLink {
			kind = 'l'
		}
		fmt.Fprintf(h, "%s\x00%c\x00", e.rel, kind)
		if e.isLink {
			h.Write([]byte(e.target))
		} else {
			abs := filepath.Join(dir, filepath.FromSlash(e.rel))
			info, err := os.Stat(abs)
			if err != nil {
				return "", fmt.Errorf("hash %s: %w", abs, err)
			}
			if info.Size() > maxHashedFileBytes {
				fmt.Fprintf(h, "%d\x00%d", info.Size(), info.ModTime().UnixNano())
			} else {
				data, err := os.ReadFile(abs)
				if err != nil {
					return "", fmt.Errorf("hash %s: %w", abs, err)
				}
				h.Write(data)
			}
		}
		h.Write([]byte{0})
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// ScanDir scans the tasks/ directory in a repo and returns a map of
// taskID → content hash for all valid task directories.
func ScanDir(tasksDir string) (map[string]string, error) {
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("scan tasks dir %s: %w", tasksDir, err)
	}

	result := make(map[string]string, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(tasksDir, e.Name())
		// skip directories that don't contain task.yaml
		if _, err := os.Stat(filepath.Join(dir, "task.yaml")); os.IsNotExist(err) {
			continue
		}
		hash, err := Hash(dir)
		if err != nil {
			return nil, err
		}
		result[e.Name()] = hash
	}
	return result, nil
}
