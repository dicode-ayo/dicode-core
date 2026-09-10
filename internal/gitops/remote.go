package gitops

import (
	"fmt"
	"net/url"

	gogit "github.com/go-git/go-git/v5"
)

// RemoteURL returns the "origin" remote URL of the git repository that
// tracks dir. dir need not be the repository root — parents are searched for
// it, exactly like HeadCommit — but unlike HeadCommit there is no
// tree-tracking check to perform: a remote describes the repository as a
// whole, not any one path inside it, so there is no notion of dir being
// "outside" what the remote covers once a repository is found at all.
//
// Any userinfo (user:token@ or user:password@) embedded in the URL is
// stripped before it is returned — this URL is only ever used to build a
// link handed to a human, never to authenticate a fetch.
//
// Errors when dir lies outside any repository, when the repository has no
// "origin" remote configured, and when that remote has no URL. All three are
// ordinary states — a local source, a repository cloned without an
// "origin", a remote entry with a malformed config — so callers that treat
// the remote as optional should discard the error rather than report it.
func RemoteURL(dir string) (string, error) {
	repo, err := gogit.PlainOpenWithOptions(dir, &gogit.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return "", fmt.Errorf("open repository at %s: %w", dir, err)
	}
	remote, err := repo.Remote("origin")
	if err != nil {
		return "", fmt.Errorf("resolve origin remote at %s: %w", dir, err)
	}
	urls := remote.Config().URLs
	if len(urls) == 0 {
		return "", fmt.Errorf("origin remote at %s has no URL", dir)
	}
	return StripURLCredentials(urls[0]), nil
}

// StripURLCredentials returns rawURL with any userinfo (user:password@ or
// user:token@) removed. Returns rawURL unchanged if it cannot be parsed as a
// URL (e.g. an SCP-like "git@host:owner/repo.git" remote, which carries no
// URL scheme at all and so embeds no parseable userinfo in the first place)
// or has no userinfo.
//
// Exported and shared: pkg/source/git.New uses this same logic to build the
// deterministic local-dir name it derives from a credential-stripped source
// URL, and previously carried a byte-for-byte private duplicate of it — one
// implementation here means a future fix to the stripping logic (a new
// credential-encoding edge case, say) can't silently apply to only one
// caller.
func StripURLCredentials(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.User == nil {
		return rawURL
	}
	u.User = nil
	return u.String()
}
