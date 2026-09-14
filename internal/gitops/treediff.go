package gitops

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TreeBlobHashesForPathsAtTwoCommits resolves, for each absolute filesystem
// path in absPaths, the git blob hash it had in fromSHA's tree and in
// toSHA's tree, in the repository that tracks dir (dir need not be the
// repository root — see HeadInfo). Both result maps are keyed by the same
// absolute path passed in. The repository is opened once and shared between
// both commit resolutions: resolving fromSHA and toSHA against two separate
// opens would walk to .git and parse its config twice for one call — the
// same principle HeadInfo's doc comment states for its own two return
// values.
//
// A path simply absent from a result map means one of: it did not exist in
// that commit (the ordinary "this file is new" signal a caller wants), it
// falls outside the repository dir belongs to, or it could not be made
// relative to the repository root. That is never reported as an error —
// every one of those degrades to "no marker" on the caller's review surface
// (see docs/adr/0001-approval-review-renders-end-state.md), never a false
// claim.
//
// err is returned when there is no repository, when fromSHA's or toSHA's
// commit/tree cannot be resolved at all (no baseline of any shape to
// compare against), or when a per-path lookup in either tree fails with
// anything other than go-git's two "not present in this tree" errors
// (object.ErrEntryNotFound, object.ErrDirectoryNotFound) — e.g. a corrupted
// pack or a transient object-read failure. Such a failure is a repository
// fault rather than a single bad path — pack corruption does not usually
// affect only one file — so it fails the whole call instead of silently
// reading as "absent": a per-path fault that degraded to absent would be
// indistinguishable from "unchanged" on the caller's review surface, which
// would be exactly the false claim ADR-0001 rules out.
//
// No blob content is read: a git tree entry carries its blob's hash without
// reading the blob itself.
func TreeBlobHashesForPathsAtTwoCommits(dir, fromSHA, toSHA string, absPaths []string) (fromHashes, toHashes map[string]string, err error) {
	repo, err := gogit.PlainOpenWithOptions(dir, &gogit.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return nil, nil, fmt.Errorf("open repository at %s: %w", dir, err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, nil, fmt.Errorf("worktree at %s: %w", dir, err)
	}
	root := wt.Filesystem.Root()

	fromHashes, err = treeBlobHashesForPaths(repo, root, fromSHA, absPaths)
	if err != nil {
		return nil, nil, err
	}
	toHashes, err = treeBlobHashesForPaths(repo, root, toSHA, absPaths)
	if err != nil {
		return nil, nil, err
	}
	return fromHashes, toHashes, nil
}

// treeBlobHashesForPaths resolves sha's tree from an already-open repo
// (root is repo's worktree root, resolved once by the caller) and looks up
// each absPaths entry within it. Shared by TreeBlobHashesForPathsAtTwoCommits
// between its two commits so the per-tree-per-paths lookup body exists once.
func treeBlobHashesForPaths(repo *gogit.Repository, root, sha string, absPaths []string) (map[string]string, error) {
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
			// lookup. Any other per-path failure is the repository failing
			// to answer, which must not read as "absent" (see this
			// function's doc comment): surface it and let the whole call
			// fail rather than silently drop one path's status.
			if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
				continue
			}
			return nil, fmt.Errorf("look up %s in tree of %s: %w", rel, sha, err)
		}
		out[abs] = entry.Hash.String()
	}
	return out, nil
}
