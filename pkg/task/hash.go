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

// hashEntry is one file or symlink folded into a Hash digest. label is the
// string written into the digest to identify the entry — for in-dir entries
// it's the dir-relative slash path; for hash_include entries it's prefixed
// with "include:" (see Hash) so an include can never collide with an in-dir
// path of the same name. abs is the absolute filesystem path read to fill in
// file content (unused for symlinks, whose target string is the content).
type hashEntry struct {
	label  string
	abs    string
	isLink bool
	target string
	// missing marks an include path that doesn't exist. Folded into the
	// digest as a sentinel (rather than silently contributing nothing) so
	// deleting or mistyping a hash_include entry still changes the hash.
	missing bool
}

// walkTree walks root recursively (skipping heavyDirs, same rules as the
// top-level dir walk) and appends one hashEntry per regular file or symlink,
// labelling each with labelPrefix + the root-relative slash path. Shared by
// Hash's own dir walk (labelPrefix "") and by hash_include directory entries
// (labelPrefix "include:<path>/").
func walkTree(root, labelPrefix string) ([]hashEntry, error) {
	var entries []hashEntry
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && heavyDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		label := labelPrefix + filepath.ToSlash(rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", path, err)
			}
			entries = append(entries, hashEntry{label: label, isLink: true, target: target})
		case d.Type().IsRegular():
			entries = append(entries, hashEntry{label: label, abs: path})
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return entries, nil
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
//
// includes (task.yaml's hash_include field) name additional files or
// directories, each resolved relative to dir, whose content is folded into
// the same digest — for a task that imports a shared module living outside
// its own dir (e.g. a sibling buildin task's helper library), editing that
// module would otherwise never perturb this task's hash and so would never
// re-trip the reconciler reload or the #392 approval-gate re-pend (#585). A
// directory include is walked recursively like dir itself; a file include is
// hashed as a single entry. Each include is labelled "include:<path>[/...]"
// in the digest so it can never collide with an in-dir path of the same
// name, and a missing include folds in a sentinel rather than silently
// contributing nothing, so a mistyped or deleted include path still changes
// the hash instead of degrading to a no-op.
func Hash(dir string, includes ...string) (string, error) {
	entries, err := walkTree(dir, "")
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", dir, err)
	}

	sortedIncludes := append([]string(nil), includes...)
	sort.Strings(sortedIncludes)
	for _, inc := range sortedIncludes {
		if inc == "" {
			continue
		}
		label := "include:" + filepath.ToSlash(inc)
		incAbs := filepath.Join(dir, filepath.FromSlash(inc))
		info, err := os.Lstat(incAbs)
		if err != nil {
			if os.IsNotExist(err) {
				entries = append(entries, hashEntry{label: label, missing: true})
				continue
			}
			return "", fmt.Errorf("hash include %s: %w", inc, err)
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(incAbs)
			if err != nil {
				return "", fmt.Errorf("hash include %s: readlink: %w", inc, err)
			}
			entries = append(entries, hashEntry{label: label, isLink: true, target: target})
		case info.IsDir():
			sub, err := walkTree(incAbs, label+"/")
			if err != nil {
				return "", fmt.Errorf("hash include %s: %w", inc, err)
			}
			entries = append(entries, sub...)
		default:
			entries = append(entries, hashEntry{label: label, abs: incAbs})
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].label < entries[j].label })

	h := sha256.New()
	for _, e := range entries {
		// Label and kind act as separators so hash(A+B) != hash(AB) and a
		// regular file can never collide with a link whose target holds the
		// same bytes.
		kind := byte('f')
		switch {
		case e.isLink:
			kind = 'l'
		case e.missing:
			kind = 'm'
		}
		fmt.Fprintf(h, "%s\x00%c\x00", e.label, kind)
		switch {
		case e.isLink:
			h.Write([]byte(e.target))
		case e.missing:
			// No content to fold in beyond the label+kind separator above —
			// that alone is enough to make a missing include distinguishable
			// from both "not configured" and "present with any content".
		default:
			info, err := os.Stat(e.abs)
			if err != nil {
				return "", fmt.Errorf("hash %s: %w", e.abs, err)
			}
			if info.Size() > maxHashedFileBytes {
				fmt.Fprintf(h, "%d\x00%d", info.Size(), info.ModTime().UnixNano())
			} else {
				data, err := os.ReadFile(e.abs)
				if err != nil {
					return "", fmt.Errorf("hash %s: %w", e.abs, err)
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
