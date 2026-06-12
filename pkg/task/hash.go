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

// Hash computes a content hash over every regular file under dir,
// recursively, in sorted dir-relative path order. The runtime allows the
// task script to import any sibling file (the Deno sandbox allow-reads the
// whole task dir; Python imports sibling modules), so every in-dir file is
// reachable code and must be part of the fingerprint — the approval gate
// (dicode.lock) keys re-approval off this hash.
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
			data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(e.rel)))
			if err != nil {
				return "", fmt.Errorf("hash %s: %w", filepath.Join(dir, e.rel), err)
			}
			h.Write(data)
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
