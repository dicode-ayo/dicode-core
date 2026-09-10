package gitops

import (
	"net/url"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// originURL returns the credential-stripped "origin" remote URL of repo, or
// "" when it has no "origin" or that remote carries no URL. Both are
// ordinary states — a clone made without an "origin", a malformed config
// entry — and the remote is optional decoration to every caller, so there is
// no error to report.
func originURL(repo *gogit.Repository) string {
	remote, err := repo.Remote("origin")
	if err != nil {
		return ""
	}
	urls := remote.Config().URLs
	if len(urls) == 0 {
		return ""
	}
	return StripURLCredentials(urls[0])
}

// StripURLCredentials returns rawURL with any userinfo (user:password@ or
// user:token@) removed. Returns rawURL unchanged when it has no userinfo, or
// cannot be parsed as a URL at all — an SCP-like "git@host:owner/repo.git"
// remote carries no scheme, so there is no parseable userinfo to strip.
func StripURLCredentials(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.User == nil {
		return rawURL
	}
	u.User = nil
	return u.String()
}

// ParseRemote extracts the canonical host and repository path from a git
// remote URL. Parsing goes through go-git's transport.NewEndpoint, which
// handles http(s), ssh, git, and the SCP shorthand (git@host:path) under one
// parser and keeps any port out of Host, and the host is canonicalized with
// the same normalizer the SSRF guard compares under, so a host is recognized
// here under exactly the rules that decide whether it is safe to fetch from.
//
// ok is false for a remote NewEndpoint rejects and for one carrying no host.
func ParseRemote(rawURL string) (host, path string, ok bool) {
	ep, err := transport.NewEndpoint(rawURL)
	if err != nil || ep.Host == "" {
		return "", "", false
	}
	return normalizeHost(ep.Host), strings.TrimSuffix(strings.Trim(ep.Path, "/"), ".git"), true
}
