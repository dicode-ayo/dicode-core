package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dicode/dicode/internal/fsutil"
	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// ErrTagNotFound reports a tag the remote does not publish.
//
// IsReclonableError must not match it: a mistyped tag is a configuration
// error that re-cloning cannot fix, and go-git's own "reference not found"
// wording would otherwise send the caller through wipe-and-re-clone against
// the remote on every resolve.
var ErrTagNotFound = errors.New("tag not found on remote")

// CloneAtTag clones url into dir at tag, or brings an existing clone onto it.
//
// A tag names one commit for good, so a clone already sitting on it is left
// alone with no network round-trip at all: refreshing a pinned source is a
// check, not a fetch. Only a tag the clone has never seen is fetched, by name.
//
// A tag the remote has since re-pointed is NOT followed. The commit a pin
// resolves to is the one the tag named when dicode first cloned it, so
// re-pointing a released tag cannot change what an operator's daemon runs.
//
// auth may be nil for public repos.
func CloneAtTag(ctx context.Context, dir, url, tag string, auth *http.BasicAuth) error {
	if err := ValidateRemoteHost(url); err != nil {
		return err
	}

	if fsutil.Exists(filepath.Join(dir, ".git")) {
		err := checkoutTag(ctx, dir, tag, auth)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrTagNotFound) || !IsReclonableError(err) {
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
	// Naming a tag as the ReferenceName narrows the clone's refspec to that one
	// tag on its own — SingleBranch is not consulted for a tag ref. History
	// stays unshallowed, matching CloneOrPull (see #175).
	opts := &gogit.CloneOptions{
		URL:           url,
		ReferenceName: plumbing.NewTagReferenceName(tag),
	}
	if auth != nil {
		opts.Auth = auth
	}
	if _, err := gogit.PlainCloneContext(ctx, dir, false, opts); err != nil {
		if isMissingRef(err) {
			return fmt.Errorf("%w: %q", ErrTagNotFound, tag)
		}
		return fmt.Errorf("clone at tag %q: %w", tag, err)
	}
	return nil
}

// checkoutTag points the worktree at dir onto tag, fetching the tag only when
// the clone does not already carry it. Returns ErrTagNotFound when the remote
// does not publish it, and a wrapped error the caller inspects with
// IsReclonableError for everything else.
func checkoutTag(ctx context.Context, dir, tag string, auth *http.BasicAuth) error {
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		return fmt.Errorf("open repo: %w", err)
	}
	hash, err := resolveTag(repo, tag)
	if err != nil {
		if err := fetchTag(ctx, repo, tag, auth); err != nil {
			return err
		}
		if hash, err = resolveTag(repo, tag); err != nil {
			return fmt.Errorf("%w: %q", ErrTagNotFound, tag)
		}
	}
	head, err := repo.Head()
	if err == nil && head.Hash() == *hash {
		return nil
	}
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	if err := wt.Checkout(&gogit.CheckoutOptions{Hash: *hash, Force: true}); err != nil {
		return fmt.Errorf("checkout tag %q: %w", tag, err)
	}
	return nil
}

// resolveTag returns the commit refs/tags/<tag> names, peeling an annotated
// tag's own object. The full ref name is passed rather than the short one so a
// tag whose name looks like an abbreviated object ID cannot resolve to that
// object instead.
func resolveTag(repo *gogit.Repository, tag string) (*plumbing.Hash, error) {
	return repo.ResolveRevision(plumbing.Revision(plumbing.NewTagReferenceName(tag)))
}

// fetchTag fetches one tag by name into the local clone.
func fetchTag(ctx context.Context, repo *gogit.Repository, tag string, auth *http.BasicAuth) error {
	ref := plumbing.NewTagReferenceName(tag)
	opts := &gogit.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []gogitconfig.RefSpec{gogitconfig.RefSpec("+" + ref + ":" + ref)},
		Force:      true,
	}
	if auth != nil {
		opts.Auth = auth
	}
	if err := repo.FetchContext(ctx, opts); err != nil && err != gogit.NoErrAlreadyUpToDate {
		if isMissingRef(err) {
			return fmt.Errorf("%w: %q", ErrTagNotFound, tag)
		}
		return fmt.Errorf("fetch tag %q: %w", tag, err)
	}
	return nil
}

// isMissingRef reports whether err is go-git's way of saying the remote does
// not publish the ref that was asked for.
func isMissingRef(err error) bool {
	return errors.Is(err, gogit.NoMatchingRefSpecError{}) || errors.Is(err, plumbing.ErrReferenceNotFound)
}
