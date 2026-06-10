// Package gitops provides shared git clone/pull helpers used by both the
// git source and the taskset loader.
package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// CloneOrPull clones url@branch into dir if not present, otherwise pulls.
// If a pull fails with a reclonable error (corrupted object DB, stuck
// shallow clone, etc.) the directory is wiped and re-cloned from scratch.
//
// auth may be nil for public repos.
//
// Clones are full (no Depth limit) so that go-git's PullContext can
// always compute a merge base when the remote advances. See #175.
func CloneOrPull(ctx context.Context, dir, url, branch string, auth *http.BasicAuth) error {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if err := pullExisting(ctx, dir, branch, auth); err == nil {
			return nil
		} else if !IsReclonableError(err) {
			return err
		}
		// Reclonable failure: wipe and fall through to the clone path.
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return fmt.Errorf("recover clone (remove): %w", rmErr)
		}
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	opts := &gogit.CloneOptions{
		URL:           url,
		ReferenceName: plumbing.NewBranchReferenceName(branch),
		SingleBranch:  true,
	}
	if auth != nil {
		opts.Auth = auth
	}
	if _, err := gogit.PlainCloneContext(ctx, dir, false, opts); err != nil {
		return fmt.Errorf("clone: %w", err)
	}
	return nil
}

// pullExisting opens the repo at dir and pulls origin/branch. Returns
// nil on success (including NoErrAlreadyUpToDate) or a wrapped pull
// error that callers inspect with IsReclonableError.
func pullExisting(ctx context.Context, dir, branch string, auth *http.BasicAuth) error {
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		return fmt.Errorf("open repo: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	opts := &gogit.PullOptions{
		RemoteName:    "origin",
		ReferenceName: plumbing.NewBranchReferenceName(branch),
		Force:         true,
	}
	if auth != nil {
		opts.Auth = auth
	}
	if err := wt.PullContext(ctx, opts); err != nil && err != gogit.NoErrAlreadyUpToDate {
		return fmt.Errorf("pull: %w", err)
	}
	return nil
}

// IsReclonableError reports whether the local clone is in a state that
// blowing-it-away-and-re-cloning fixes. These are the error shapes
// observed in production: stuck-shallow reconcile failures, missing
// objects/packfiles, dangling refs. Network errors and auth errors
// are NOT reclonable — re-cloning would just fail the same way and
// thrash the remote.
func IsReclonableError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, sig := range []string{
		"object not found",
		"reference not found",
		"packfile",
		"invalid pkt-len",
	} {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// HTTPAuth returns HTTP basic-auth credentials derived from the named
// environment variable, or nil if tokenEnv is empty or the variable is
// unset.
func HTTPAuth(tokenEnv string) *http.BasicAuth {
	if tokenEnv == "" {
		return nil
	}
	token := os.Getenv(tokenEnv)
	if token == "" {
		return nil
	}
	return &http.BasicAuth{Username: "git", Password: token}
}
