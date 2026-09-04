// Package gitops provides the shared git clone/refresh helpers used by both
// the git source and the taskset loader.
package gitops

import (
	"os"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

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
