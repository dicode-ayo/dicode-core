// Package git holds the git operations the daemon exposes to the web UI and
// IPC: listing remote branches and committing and pushing task changes.
package git

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/dicode/dicode/internal/gitops"
	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	gogittransport "github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// ListBranches contacts the remote and returns branch names sorted alphabetically.
// tokenEnv is the name of an env var holding an HTTP auth token; pass "" for public repos.
//
// Remotes on loopback/private/link-local/internal hosts are rejected before
// any connection is attempted — this function is reachable from the REST API
// with a caller-supplied URL, so it must not be usable to probe the daemon's
// internal network (#475). Uses gitops.ValidateRemoteHost, the same shared
// guard CloneAtRef calls (#489), so there is exactly one place that can
// drift out of sync.
func ListBranches(ctx context.Context, repoURL, tokenEnv string) ([]string, error) {
	if err := gitops.ValidateRemoteHost(repoURL); err != nil {
		return nil, err
	}

	var auth gogittransport.AuthMethod
	if tokenEnv != "" {
		if token := os.Getenv(tokenEnv); token != "" {
			auth = &http.BasicAuth{Username: "git", Password: token}
		}
	}

	rem := gogit.NewRemote(nil, &gogitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{repoURL},
	})

	refs, err := rem.ListContext(ctx, &gogit.ListOptions{Auth: auth})
	if err != nil {
		return nil, fmt.Errorf("list remote: %w", err)
	}

	var branches []string
	for _, ref := range refs {
		name := ref.Name().String()
		if strings.HasPrefix(name, "refs/heads/") {
			branches = append(branches, strings.TrimPrefix(name, "refs/heads/"))
		}
	}
	sort.Strings(branches)
	return branches, nil
}
