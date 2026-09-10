package approval

import (
	"net/url"
	"strings"
)

// CommitRange is the "what moved" decoration for a pending task: the commit
// range between the previously-approved commit and the commit the pending
// content was observed at, plus a link to the git host's compare view.
//
// Every field can be legitimately empty, and that is the ordinary
// (non-error) state rather than a partial failure — see ADR-0001
// ("End state needs no baseline... The 'what moved' strip is decoration:
// when it cannot be computed, it is absent, and the screen is less
// contextual, never blank"). From is empty on a task's first-ever approval,
// or when the source has no git history; To is empty under the same
// conditions for the currently pending content; CompareURL is empty
// whenever a range cannot be resolved to a link at all — no From/To, an
// unchanged commit, or a remote host this package does not recognize.
type CommitRange struct {
	// From is the commit the task was approved at last time, or "" if there
	// is no prior approval (or it recorded no commit).
	From string
	// To is the commit the currently pending content was observed at, or ""
	// if none could be resolved.
	To string
	// CompareURL is a link to the git host's compare view for From...To, or
	// "" when it cannot be built (see compareURL).
	CompareURL string
}

// compareURL returns a link to the git host's compare view for the range
// from...to on remote, or "" when no such link can be built.
//
// "" covers every case a caller should render as "no link" rather than a
// broken one: no prior commit (from == ""), nothing moved (from == to), an
// empty or unparseable remote, and a remote host this function does not
// recognize. Guessing a URL shape for an unrecognized host would produce a
// link that is wrong far more often than it is right, which is worse than no
// link at all.
func compareURL(remote, from, to string) string {
	if from == "" || to == "" || from == to {
		return ""
	}
	host, owner, repo, ok := parseRemote(remote)
	if !ok {
		return ""
	}
	switch host {
	case "github.com":
		return "https://github.com/" + owner + "/" + repo + "/compare/" + from + "..." + to
	case "gitlab.com":
		return "https://gitlab.com/" + owner + "/" + repo + "/-/compare/" + from + "..." + to
	default:
		return ""
	}
}

// parseRemote extracts the host, owner, and repo name out of a git remote
// URL. It recognizes:
//
//   - HTTPS remotes: https://host/owner/repo(.git)?
//   - scp-like SSH remotes: git@host:owner/repo(.git)?
//   - ssh:// remotes: ssh://git@host/owner/repo(.git)?
//
// ok is false for anything else — an empty string, a remote with too few
// path segments to name an owner and a repo, or a string url.Parse rejects
// outright.
func parseRemote(remote string) (host, owner, repo string, ok bool) {
	if remote == "" {
		return "", "", "", false
	}

	// scp-like form has no URL scheme at all: user@host:path. Detect it
	// before handing the string to url.Parse, which does not understand it.
	if i := strings.Index(remote, "@"); i >= 0 && !strings.Contains(remote[:i], "://") {
		rest := remote[i+1:]
		if j := strings.Index(rest, ":"); j >= 0 && !strings.HasPrefix(rest[j:], "://") {
			host = rest[:j]
			return splitOwnerRepo(host, rest[j+1:])
		}
	}

	u, err := url.Parse(remote)
	if err != nil || u.Host == "" {
		return "", "", "", false
	}
	return splitOwnerRepo(u.Host, u.Path)
}

// splitOwnerRepo pulls owner/repo off the trailing two path segments of
// path (leading/trailing slashes and a trailing ".git" ignored).
func splitOwnerRepo(host, path string) (h, owner, repo string, ok bool) {
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return "", "", "", false
	}
	owner, repo = parts[len(parts)-2], parts[len(parts)-1]
	if host == "" || owner == "" || repo == "" {
		return "", "", "", false
	}
	return host, owner, repo, true
}
