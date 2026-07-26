package approval

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"unicode/utf8"
)

// maxSnapshotFileBytes bounds the content read into a single file's diff
// snapshot. This is much smaller than task/hash.go's maxHashedFileBytes
// because a snapshot is held in memory for the life of the gate (not just
// digested and discarded) and is rendered as human-readable diff text, not
// folded into a hash.
const maxSnapshotFileBytes = 256 * 1024 // 256 KiB

// maxSnapshotFiles bounds the number of files captured per task snapshot.
// Snapshotting runs on every Admit — including every ~30s reconcile poll —
// so a pathologically large task dir must not make that otherwise-cheap
// operation slow or memory-hungry. Files beyond the cap still appear in the
// snapshot (so Diff can report that they exist / changed) but with a
// placeholder value instead of their content.
const maxSnapshotFiles = 200

// snapshotPlaceholder is the value stored for a file whose content was not
// captured — because it is binary (fails UTF-8 validation), larger than
// maxSnapshotFileBytes, or beyond maxSnapshotFiles. Diff never synthesizes a
// line-level unified diff for these; FileDiff.UnifiedDiff carries this
// string verbatim instead.
const snapshotPlaceholder = "binary or file too large to diff"

// snapshotHeavyDirs mirrors task/hash.go's heavyDirs: directories that hold
// no task content and can dominate walk cost. Kept as its own copy (rather
// than reaching into task's unexported map) so this package's walk isn't
// coupled to task's internals — same list, same rationale.
var snapshotHeavyDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
}

// snapshotDir walks dir the same way task/hash.go's Hash walker does (same
// heavyDirs skip list, same fs.DirEntry.Info()-based regular-file
// classification) and returns a map of dir-relative slash path to file
// content, for Gate.Diff to compare snapshots taken at different times.
//
// This is a distinct walk from Hash's: the approval lock (pkg/approval/lock.go)
// only ever stores a content hash, never file bytes, so there is no
// "before" snapshot to diff against once a task's hash has changed and the
// working tree already holds the new content. snapshotDir exists to give
// Gate an in-memory "before" to compare the "after" against.
//
// Every file is capped at maxSnapshotFileBytes and the whole dir at
// maxSnapshotFiles; a file over either bound, or one that is not valid
// UTF-8 text (binary), is stored as snapshotPlaceholder instead of its raw
// bytes — so the map's key set still reflects every observed file even
// though not every value is diffable text.
//
// A missing dir returns an empty map (no error) — callers may race task
// removal, matching Hash's behavior for the same case. Symlinks, sockets,
// devices, and other non-regular entries are skipped entirely (no text
// content to diff), matching Hash's walkTree classification.
func snapshotDir(dir string) (map[string]string, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != dir && snapshotHeavyDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil // vanished mid-walk (race) — skip, don't fail the scan
			}
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	sort.Strings(paths)

	out := make(map[string]string, len(paths))
	for i, rel := range paths {
		if i >= maxSnapshotFiles {
			out[rel] = snapshotPlaceholder
			continue
		}
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		info, statErr := os.Stat(abs)
		if statErr != nil || info.Size() > maxSnapshotFileBytes {
			out[rel] = snapshotPlaceholder
			continue
		}
		data, readErr := os.ReadFile(abs)
		if readErr != nil || !utf8.Valid(data) {
			out[rel] = snapshotPlaceholder
			continue
		}
		out[rel] = string(data)
	}
	return out, nil
}
