// Package fsutil collects small filesystem helpers shared across dicode:
// atomic file replacement, existence probes, and walk-up file search.
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to path atomically: it writes to a temp file in
// the same directory, fsyncs it, applies mode, and renames it over path. A
// crash mid-write leaves any existing file untouched. mode is applied with
// os.Chmod, so it is exact (not filtered by umask); pass the original file's
// mode to preserve it across replacements.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("sync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod tmp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// Exists reports whether path exists (any file type). Stat errors other than
// "not exist" (e.g. permission denied) also report false; callers that must
// distinguish those cases should use os.Stat directly.
func Exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// FindUp looks for rel (a file name or relative path) in startDir and then in
// up to maxParents ancestor directories, i.e. maxParents+1 directories are
// probed in total. It returns the first match and true, or ("", false) when
// nothing is found before running out of parents or hitting the filesystem
// root.
func FindUp(startDir, rel string, maxParents int) (string, bool) {
	dir := filepath.Clean(startDir)
	for i := 0; i <= maxParents; i++ {
		candidate := filepath.Join(dir, rel)
		if Exists(candidate) {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}
