package taskset

import "github.com/dicode/dicode/internal/gitops"

// sanitizeErrorString strips user:password userinfo from URLs embedded
// in git error messages. Operators routinely put PATs in source URLs
// (`https://oauth2:ghp_...@github.com/...`); go-git's errors echo the
// full URL, which would otherwise reach every authenticated web-UI
// viewer via /api/sources.last_pull_error.
func sanitizeErrorString(s string) string {
	return SanitizeURL(s)
}

// SanitizeURL strips user:password userinfo from any URL in s, leaving the
// scheme and everything from the host onward intact. Callers hand out the
// result in place of a configured source URL: the same PATs the error path
// above guards against are readable from ref.url itself, and a source URL
// reaches further than an error message does — any task granted
// permissions.dicode.sources_list receives one.
//
// The stripping logic itself lives in internal/gitops.StripURLCredentials,
// which this package already imports elsewhere and which is the single
// canonical implementation shared with pkg/source/git and gitops.HeadInfo.
// SanitizeURL and sanitizeErrorString stay as this package's own exported
// names — pull_status.go and pkg/webui/sources.go are written against
// them — and simply delegate.
func SanitizeURL(s string) string {
	return gitops.StripURLCredentials(s)
}
