package taskset

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

// cloneOrPull clones url@branch into dir if not present, otherwise pulls.
// tokenEnv is the name of an env var holding an HTTP auth token; pass "" for public repos.
//
// If a pull fails with a reconcile-style error (e.g. the clone is a
// leftover shallow from an older dicode version, or the object DB is
// corrupted), the directory is wiped and re-cloned from scratch. This
// keeps users upgrading from pre-#176 dicode builds from getting stuck
// in a pull-fails-forever loop on their existing ~/.dicode/tasksets/
// clones without having to manually delete the directory.
func cloneOrPull(ctx context.Context, dir, url, branch, tokenEnv string) error {
	auth := httpAuth(tokenEnv)

	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if err := pullExisting(ctx, dir, branch, auth); err == nil {
			return nil
		} else if !isReclonableError(err) {
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
	// Full clone (no Depth). A shallow clone costs bandwidth once but
	// fails PullContext with "object not found" the first time the remote
	// advances past the shallow tip — go-git can't compute a merge base
	// when the parent commits aren't in the local object DB. See #175.
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
// error that callers inspect with isReclonableError.
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

// isReclonableError reports whether the local clone is in a state that
// blowing-it-away-and-re-cloning fixes. These are the error shapes
// observed in production: stuck-shallow reconcile failures, missing
// objects/packfiles, dangling refs. Network errors and auth errors
// are NOT reclonable — re-cloning would just fail the same way and
// thrash the remote.
func isReclonableError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	// Positive signals: references or objects that the remote has but
	// the local object DB doesn't, which go-git surfaces as these
	// phrases. Matching on substrings is deliberately permissive —
	// we'd rather over-recover (cheap) than under-recover (breaks
	// tasksets silently for the operator).
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

func httpAuth(tokenEnv string) *http.BasicAuth {
	if tokenEnv == "" {
		return nil
	}
	token := os.Getenv(tokenEnv)
	if token == "" {
		return nil
	}
	return &http.BasicAuth{Username: "git", Password: token}
}
