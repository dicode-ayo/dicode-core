package task

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
)

// File kinds reported by Inventory.
const (
	FileKindRegular = "file"
	FileKindSymlink = "symlink"
	FileKindMissing = "missing"
)

// FileMeta describes one file constituting a task, without carrying any of its
// content. It is the code-shaped fact a task spec cannot express: a new or
// edited file is visible from the hash alone, so a review surface can report
// it without rendering a single byte.
type FileMeta struct {
	// Path is the task-dir-relative slash path, or "include:<path>" for a
	// hash_include target living outside the directory. These are Hash's own
	// entry labels, so the two describe the same file set.
	Path string `json:"path"`
	// Kind is FileKindRegular, FileKindSymlink, or FileKindMissing.
	Kind string `json:"kind"`
	// Size is the file's size in bytes; 0 for symlinks and missing includes.
	Size int64 `json:"size"`
	// Hash is a hex SHA-256 over exactly the bytes Hash folds into the content
	// hash for this file, so it moves if and only if the file's contribution
	// to the gate's hash moves. For a file over maxHashedFileBytes that is the
	// size+mtime descriptor rather than the content, matching Hash's own
	// bounded-read rule. Empty for symlinks (Target is the reviewable fact)
	// and for missing includes.
	Hash string `json:"hash,omitempty"`
	// Target is a symlink's target string, never followed.
	Target string `json:"target,omitempty"`
}

// Inventory lists the files constituting the task rooted at dir, including any
// hash_include targets, in the same sorted order and under the same labels
// Hash uses. Content is read only to digest it; nothing is retained or
// returned, so an inventory can be rendered on a surface that must not display
// code.
//
// A missing dir inventories as an empty one, matching Hash — callers race task
// removal. An include escaping the sibling-task boundary is an error, also
// matching Hash.
func Inventory(dir string, includes ...string) ([]FileMeta, error) {
	entries, err := collectEntries(dir, includes...)
	if err != nil {
		return nil, err
	}

	out := make([]FileMeta, 0, len(entries))
	for _, e := range entries {
		switch {
		case e.isLink:
			out = append(out, FileMeta{Path: e.label, Kind: FileKindSymlink, Target: e.target})
		case e.missing:
			out = append(out, FileMeta{Path: e.label, Kind: FileKindMissing})
		default:
			info, err := e.stat()
			if err != nil {
				if os.IsNotExist(err) {
					continue // vanished mid-walk, same race walkTree tolerates
				}
				return nil, fmt.Errorf("inventory %s: %w", e.abs, err)
			}
			digest, err := entryDigest(e.abs, info)
			if err != nil {
				return nil, err
			}
			out = append(out, FileMeta{
				Path: e.label,
				Kind: FileKindRegular,
				Size: info.Size(),
				Hash: digest,
			})
		}
	}
	return out, nil
}

// entryDigest hashes exactly the bytes Hash folds in for a regular file.
func entryDigest(abs string, info os.FileInfo) (string, error) {
	h := sha256.New()
	if err := writeFileContribution(h, abs, info); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
