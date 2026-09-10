package approval

import (
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
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
	host, path, ok := parseRemote(remote)
	if !ok {
		return ""
	}
	switch host {
	case "github.com":
		// GitHub repo paths are always exactly owner/repo — there is no
		// subgroup concept, so anything other than exactly two segments is
		// not a valid GitHub repo reference.
		owner, repo, ok := exactlyTwoSegments(path)
		if !ok {
			return ""
		}
		return "https://github.com/" + owner + "/" + repo + "/compare/" + from + "..." + to
	case "gitlab.com":
		// GitLab supports arbitrarily nested subgroups
		// (group/subgroup/.../project), so the whole cleaned path is the
		// project reference — never truncate to the trailing two segments.
		// A bare single segment isn't a valid project path though (there is
		// always at least a namespace and a project name).
		if path == "" || !strings.Contains(path, "/") {
			return ""
		}
		return "https://gitlab.com/" + path + "/-/compare/" + from + "..." + to
	default:
		return ""
	}
}

// exactlyTwoSegments reports whether path is exactly two non-empty
// "/"-separated segments, returning them as (a, b) when so.
func exactlyTwoSegments(path string) (a, b string, ok bool) {
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// parseRemote extracts the host and repository path out of a git remote
// URL via go-git's transport.NewEndpoint, which understands http(s), ssh,
// git, and the SCP shorthand form (git@host:path) uniformly — the same
// parser internal/gitops/hostguard.go's ValidateRemoteHost uses for the
// same purpose (extracting a remote's host). Using the library parser here
// instead of hand-rolled SCP-vs-URL detection also means the port never
// needs manual stripping: transport.Endpoint carries it as a separate Port
// field rather than folding it into Host.
//
// ok is false for anything a remote host this function does not recognize
// — an empty string, a remote transport.NewEndpoint rejects outright, or
// one that parses but carries no host component.
func parseRemote(remote string) (host, path string, ok bool) {
	if remote == "" {
		return "", "", false
	}
	ep, err := transport.NewEndpoint(remote)
	if err != nil || ep.Host == "" {
		return "", "", false
	}
	// Host is lowercased here — DNS hosts are case-insensitive
	// ("GitHub.com" and "github.com" are the same host) — so a
	// differently-cased remote still matches compareURL's literal-lowercase
	// switch.
	host = strings.ToLower(ep.Host)
	path = strings.TrimSuffix(strings.Trim(ep.Path, "/"), ".git")
	return host, path, true
}
