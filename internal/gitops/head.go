package gitops

import (
	"fmt"

	gogit "github.com/go-git/go-git/v5"
)

// HeadCommit returns the hex commit ID at HEAD of the git repository
// containing dir. dir need not be the repository root — parents are searched
// for it — so a task directory nested anywhere inside a clone resolves.
//
// Errors when dir lies outside any repository or the repository has no commit
// yet; both are ordinary states for a local source, so callers that treat the
// commit as optional should discard the error rather than report it.
func HeadCommit(dir string) (string, error) {
	repo, err := gogit.PlainOpenWithOptions(dir, &gogit.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return "", fmt.Errorf("open repository at %s: %w", dir, err)
	}
	ref, err := repo.Head()
	if err != nil {
		return "", fmt.Errorf("resolve HEAD at %s: %w", dir, err)
	}
	return ref.Hash().String(), nil
}
