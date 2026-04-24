package taskset

import "regexp"

// urlUserinfoRe matches the `user:password@` portion of any URL
// embedded in an arbitrary string. Same shape as the one in pkg/relay;
// duplicated rather than shared because both packages are small and
// the coupling isn't worth a new internal utility package.
var urlUserinfoRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^\s/@]+@`)

// sanitizeErrorString strips user:password userinfo from URLs embedded
// in git error messages. Operators routinely put PATs in source URLs
// (`https://oauth2:ghp_...@github.com/...`); go-git's errors echo the
// full URL, which would otherwise reach every authenticated web-UI
// viewer via /api/sources.last_pull_error.
func sanitizeErrorString(s string) string {
	return urlUserinfoRe.ReplaceAllString(s, "$1")
}
