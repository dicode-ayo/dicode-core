package relay

import "regexp"

// urlUserinfoRe matches the `user:password@` portion of any URL
// embedded in an arbitrary string. `[^\s/@]+` is deliberately strict:
// userinfo cannot contain whitespace, slash, or another `@`. Matching
// `scheme://` requires RFC-3986-ish scheme chars.
var urlUserinfoRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^\s/@]+@`)

// sanitizeErrorString strips user:password userinfo from any URLs
// embedded in an error message. Relay transports wrap
// "ws://user:token@host/…" verbatim into their errors, which would
// otherwise get echoed to every authenticated UI viewer via
// /api/relay/status.last_error.
func sanitizeErrorString(s string) string {
	return urlUserinfoRe.ReplaceAllString(s, "$1")
}
