package gitops

import (
	"fmt"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// TreeBlobHashesForPaths resolves, for each absolute filesystem path in
// absPaths, the git blob hash it had in commit sha's tree, in the
// repository that tracks dir (dir need not be the repository root — see
// HeadInfo). The result is keyed by the same absolute path passed in.
//
// A path simply absent from the result means one of: it did not exist in
// that commit (the ordinary "this file is new" signal a caller wants), it
// falls outside the repository dir belongs to, or it could not be made
// relative to the repository root. That is never reported as an error —
// every one of those degrades to "no marker" on the caller's review
// surface (see docs/adr/0001-approval-review-renders-end-state.md), never a
// false claim. err is returned only when there is no repository, or sha's
// commit/tree cannot be resolved at all — i.e. there is no baseline of any
// shape to compare against.
//
// No blob content is read: a git tree entry carries its blob's hash without
// reading the blob itself.
func TreeBlobHashesForPaths(dir, sha string, absPaths []string) (map[string]string, error) {
	repo, err := gogit.PlainOpenWithOptions(dir, &gogit.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return nil, fmt.Errorf("open repository at %s: %w", dir, err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("worktree at %s: %w", dir, err)
	}
	root := wt.Filesystem.Root()

	hash := plumbing.NewHash(sha)
	if hash.IsZero() {
		return nil, fmt.Errorf("resolve commit %s: not a valid commit hash", sha)
	}
	commit, err := repo.CommitObject(hash)
	if err != nil {
		return nil, fmt.Errorf("resolve commit %s: %w", sha, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("read tree of %s: %w", sha, err)
	}

	out := make(map[string]string, len(absPaths))
	for _, abs := range absPaths {
		if abs == "" {
			// hashEntry.abs is unset for some entry shapes (see
			// task.hashEntry's doc comment) — nothing to look up.
			continue
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			// Cannot be related to the repository root at all — degrade
			// rather than fail the whole batch over one path.
			continue
		}
		rel = filepath.ToSlash(rel)
		if rel == ".." || strings.HasPrefix(rel, "../") || rel == "." {
			// Outside root, or the root itself — neither is a file this
			// tree can carry an entry for.
			continue
		}
		entry, err := tree.FindEntry(rel)
		if err != nil {
			// object.ErrEntryNotFound / object.ErrDirectoryNotFound (go-git's
			// two "no such path in this tree" errors) is the ordinary "this
			// file is new" signal a caller wants, not a fault — the same
			// classification head.go's headTracks applies to its own tree
			// lookup. Any other per-path failure degrades identically: this
			// function's err return is reserved for "no baseline at all" (no
			// repository, or sha unresolvable), never a single path.
			continue
		}
		out[abs] = entry.Hash.String()
	}
	return out, nil
}
