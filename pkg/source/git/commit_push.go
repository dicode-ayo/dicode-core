package git

import (
	"context"
	"fmt"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// CommitPushOptions controls what CommitPush adds, commits, and pushes.
type CommitPushOptions struct {
	// Message is the commit message. Required (non-empty).
	Message string

	// Branch is the local branch to push. Required.
	Branch string

	// BranchPrefix, when non-empty, is the literal prefix Branch must start
	// with. Used by the auto-fix flow to enforce that pushes only target
	// fix-branch namespaces. Empty bypasses prefix checking.
	BranchPrefix string

	// AllowMain authorises pushing even when Branch doesn't satisfy
	// BranchPrefix. Required to be true for auto-fix autonomous mode;
	// defaults to false.
	AllowMain bool

	// Files is the list of paths (relative to repoPath) to git-add. Empty
	// means "all tracked changes" — equivalent to `git add -u`.
	Files []string

	// Author is the commit author. Name + Email required.
	Author Signature

	// AuthToken, when non-empty, is used as a bearer token for HTTPS push
	// auth (e.g. a GitHub fine-grained PAT). Empty disables auth.
	AuthToken string
}

// Signature names a commit author. Mirrors object.Signature without
// importing the go-git type into our public surface.
type Signature struct {
	Name  string
	Email string
}

// CommitPush adds, commits, and pushes the requested branch in repoPath.
// Returns the new commit hash hex string.
//
// Validation:
//   - Message must be non-empty.
//   - Branch must be non-empty.
//   - Author.Name + Author.Email must be non-empty.
//   - Branch must satisfy BranchPrefix (when non-empty) unless AllowMain is true.
//
// Never sets Force on the push; a non-fast-forward push fails with the
// underlying go-git error.
func CommitPush(ctx context.Context, repoPath string, opts CommitPushOptions) (string, error) {
	if opts.Message == "" {
		return "", fmt.Errorf("commit message required")
	}
	if opts.Branch == "" {
		return "", fmt.Errorf("branch required")
	}
	if opts.Author.Name == "" || opts.Author.Email == "" {
		return "", fmt.Errorf("author name + email required")
	}
	if opts.BranchPrefix != "" && !strings.HasPrefix(opts.Branch, opts.BranchPrefix) && !opts.AllowMain {
		return "", fmt.Errorf("branch %q does not start with prefix %q (AllowMain=false)", opts.Branch, opts.BranchPrefix)
	}

	repo, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return "", fmt.Errorf("open repo: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("worktree: %w", err)
	}

	if len(opts.Files) == 0 {
		if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
			return "", fmt.Errorf("add all: %w", err)
		}
	} else {
		for _, p := range opts.Files {
			if _, err := wt.Add(p); err != nil {
				return "", fmt.Errorf("add %q: %w", p, err)
			}
		}
	}

	commit, err := wt.Commit(opts.Message, &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  opts.Author.Name,
			Email: opts.Author.Email,
			When:  time.Now(),
		},
	})
	if err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}

	pushOpts := &gogit.PushOptions{}
	if opts.AuthToken != "" {
		pushOpts.Auth = &http.BasicAuth{
			Username: "x-access-token",
			Password: opts.AuthToken,
		}
	}
	if err := repo.PushContext(ctx, pushOpts); err != nil && err != gogit.NoErrAlreadyUpToDate {
		return "", fmt.Errorf("push: %w", err)
	}
	return commit.String(), nil
}
