package gitops

import (
	"context"
	"fmt"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
)

// DefaultBranch asks the remote at url which branch its HEAD points to,
// without credentials. A remote that advertises HEAD only as a hash gets the
// branch at that hash, or ErrRefNotFound when none matches.
func DefaultBranch(ctx context.Context, url string) (string, error) {
	if err := ValidateRemoteHost(url); err != nil {
		return "", err
	}
	rem := gogit.NewRemote(nil, &gogitconfig.RemoteConfig{Name: "origin", URLs: []string{url}})
	refs, err := rem.ListContext(ctx, &gogit.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list remote: %w", err)
	}
	var head *plumbing.Reference
	for _, r := range refs {
		if r.Name() == plumbing.HEAD {
			head = r
			break
		}
	}
	if head == nil {
		return "", fmt.Errorf("%w: HEAD", ErrRefNotFound)
	}
	if head.Type() == plumbing.SymbolicReference && head.Target().IsBranch() {
		return head.Target().Short(), nil
	}
	for _, r := range refs {
		if r.Name().IsBranch() && r.Type() == plumbing.HashReference && r.Hash() == head.Hash() {
			return strings.TrimPrefix(r.Name().String(), "refs/heads/"), nil
		}
	}
	return "", fmt.Errorf("%w: HEAD", ErrRefNotFound)
}
