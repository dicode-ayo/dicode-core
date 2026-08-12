package gitops

import (
	"errors"
	"fmt"
	"path/filepath"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// HeadCommit returns the hex commit ID at HEAD of the git repository that
// tracks dir. dir need not be the repository root — parents are searched for
// it — so a task directory nested anywhere inside a clone resolves.
//
// dir must be present in HEAD's tree. A directory that merely sits underneath
// some unrelated repository — tasks in a home directory that happens to be
// version-controlled — resolves no commit, because that repository's HEAD
// describes none of dir's content.
//
// Errors when dir lies outside any repository, when the repository has no
// commit yet, and when HEAD does not track dir. All three are ordinary states
// for a local source, so callers that treat the commit as optional should
// discard the error rather than report it.
//
// No blob is read: resolving a tree entry needs the tree objects along dir's
// path and nothing else.
func HeadCommit(dir string) (string, error) {
	repo, err := gogit.PlainOpenWithOptions(dir, &gogit.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return "", fmt.Errorf("open repository at %s: %w", dir, err)
	}
	ref, err := repo.Head()
	if err != nil {
		return "", fmt.Errorf("resolve HEAD at %s: %w", dir, err)
	}
	tracked, err := headTracks(repo, ref.Hash(), dir)
	if err != nil {
		return "", err
	}
	if !tracked {
		return "", fmt.Errorf("HEAD does not track %s", dir)
	}
	return ref.Hash().String(), nil
}

// headTracks reports whether dir appears in the tree of commit head. The
// repository's own root always does, without a lookup.
func headTracks(repo *gogit.Repository, head plumbing.Hash, dir string) (bool, error) {
	wt, err := repo.Worktree()
	if err != nil {
		return false, fmt.Errorf("worktree at %s: %w", dir, err)
	}
	rel, err := filepath.Rel(wt.Filesystem.Root(), dir)
	if err != nil {
		return false, fmt.Errorf("locate %s within its repository: %w", dir, err)
	}
	if rel == "." {
		return true, nil
	}
	commit, err := repo.CommitObject(head)
	if err != nil {
		return false, fmt.Errorf("read commit %s: %w", head, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return false, fmt.Errorf("read tree of %s: %w", head, err)
	}
	if _, err := tree.FindEntry(filepath.ToSlash(rel)); err != nil {
		// Absence is the answer; anything else is the repository failing to
		// answer, which must not read as "untracked".
		if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("look up %s in tree of %s: %w", rel, head, err)
	}
	return true, nil
}
