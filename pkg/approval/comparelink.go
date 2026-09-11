package approval

import (
	"strings"

	"github.com/dicode/dicode/internal/gitops"
)

// CommitRange is the "what moved" decoration for a pending task: the commit
// range between the previously-approved commit and the commit the pending
// content was observed at, plus a link to the git host's compare view.
//
// Every field can be legitimately empty, and that is the ordinary
// (non-error) state rather than a partial failure — ADR-0001 makes the strip
// decoration: when it cannot be computed the screen is less contextual,
// never blank.
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
// from...to on remote, or "" when no such link can be built: either endpoint
// missing, the two equal (a compare view of a commit against itself renders
// an empty diff), an unparseable remote, or a host whose compare-view URL
// shape is not known here. Guessing a shape for an unrecognized host produces
// a link that is wrong far more often than right, which is worse than no link.
func compareURL(remote, from, to string) string {
	if from == "" || to == "" || from == to {
		return ""
	}
	host, path, ok := gitops.ParseRemote(remote)
	if !ok {
		return ""
	}
	switch host {
	case "github.com":
		// GitHub repo paths are always exactly owner/repo — there is no
		// subgroup concept, so anything else is not a valid repo reference.
		owner, repo, ok := exactlyTwoSegments(path)
		if !ok {
			return ""
		}
		return "https://github.com/" + owner + "/" + repo + "/compare/" + from + "..." + to
	case "gitlab.com":
		// GitLab supports arbitrarily nested subgroups
		// (group/subgroup/.../project), so the whole path is the project
		// reference — never truncate to the trailing two segments. A bare
		// single segment is not a valid project path: there is always at
		// least a namespace and a project name.
		if !strings.Contains(path, "/") {
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
