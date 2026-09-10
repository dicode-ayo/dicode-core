package gitops

import (
	"errors"
	"fmt"
	"path/filepath"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// HeadInfo returns the hex commit ID at HEAD of the git repository that
// tracks dir, together with that repository's credential-stripped "origin"
// remote URL. dir need not be the repository root — parents are searched for
// it — so a task directory nested anywhere inside a clone resolves.
//
// dir must be present in HEAD's tree. A directory that merely sits underneath
// some unrelated repository — tasks in a home directory that happens to be
// version-controlled — resolves no commit, because that repository's HEAD
// describes none of dir's content.
//
// remote is "" when the repository has no "origin"; that alone is never an
// error, since callers treat the remote as optional decoration. err covers
// the states that leave no commit either: dir outside any repository, a
// repository with no commit yet, and a HEAD that does not track dir. All are
// ordinary for a local source, so callers that treat the commit as optional
// should discard the error rather than report it.
//
// Both values come from a single repository open: resolving them separately
// would walk to .git and parse its config twice for one task.
//
// No blob is read: resolving a tree entry needs the tree objects along dir's
// path and nothing else.
func HeadInfo(dir string) (commit, remote string, err error) {
	repo, err := gogit.PlainOpenWithOptions(dir, &gogit.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return "", "", fmt.Errorf("open repository at %s: %w", dir, err)
	}
	ref, err := repo.Head()
	if err != nil {
		return "", "", fmt.Errorf("resolve HEAD at %s: %w", dir, err)
	}
	tracked, err := headTracks(repo, ref.Hash(), dir)
	if err != nil {
		return "", "", err
	}
	if !tracked {
		return "", "", fmt.Errorf("HEAD does not track %s", dir)
	}
	return ref.Hash().String(), originURL(repo), nil
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
