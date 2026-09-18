package gitops

import "regexp"

// urlUserinfoRe matches the `user:password@` portion of a URL embedded
// anywhere in an arbitrary string, capturing the URL's "scheme://" prefix
// so the replacement can drop only the userinfo and leave everything else
// byte-for-byte intact.
//
// The userinfo run `[^\s/?#]*` deliberately allows `@` itself, and relies on
// Go RE2's greedy matching to extend that run as far as possible — up to the
// last `@` that appears before the URL authority component ends — before
// requiring the trailing literal `@` delimiter. Per RFC 3986, the authority
// component ends at the first `/`, `?`, `#`, whitespace, or the end of the
// string, whichever comes first; the character class excludes exactly those
// four terminators so the run can never cross into the path, query string,
// fragment, or surrounding text. That mirrors net/url's split point
// (userinfo ends at the *last* `@` before the authority ends) and is what
// makes this safe for a userinfo segment that itself contains a literal
// `@`, such as an email address used as a Basic-Auth username or a password
// containing `@` (e.g. "https://alice:P@ssw0rd@host/repo.git" must have the
// whole "alice:P@ssw0rd@" stripped, not just "alice:P@"). A naive
// `[^\s/@]+@` (stopping at the *first* `@`) under-strips exactly that case,
// leaking the credential remainder — that was a regression an earlier
// refactor introduced and this comment exists to keep it from coming back.
//
// Excluding `?` and `#` (not just `/` and whitespace) closes a second,
// opposite bug: without them, the run also greedily swallows a query string
// or fragment and matches through to a LATER unrelated `@` that happens to
// appear there (e.g. a token value like "?token=abc@evil.com"), stripping
// past the real host and corrupting the output. Bounding the run to the
// authority component means a `@` in the query or fragment is never even
// considered as the match's closing delimiter.
//
// It is deliberately not structure-aware the way net/url.Parse is. That
// matters: url.Parse (Go 1.20+) rejects a non-numeric value in the "port"
// position, so an owner-shorthand SSH remote like
// "ssh://git@github.com:org/repo.git" ("org" sits where a port would go)
// fails to parse at all, and a strip built on it has to give up and return
// the credentialed URL unchanged — silently, for exactly the shape most
// likely to carry a real token. The regex has no such structural
// requirement, so it strips consistently across every remote shape this
// package and pkg/taskset need to handle, including URLs embedded in
// surrounding text (e.g. a git error message).
var urlUserinfoRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^\s/?#]*@`)

// StripURLCredentials returns rawURL — or, for callers scanning free-form
// text such as a git error message, any string with a URL embedded in it —
// with `user:password@` / `user:token@` userinfo removed from every
// "scheme://" URL found. It leaves the scheme and everything from the host
// onward intact, and returns the input completely unchanged when there is
// no userinfo to strip. An SCP-like "git@host:owner/repo.git" remote has no
// "scheme://" prefix, so it is never treated as a URL to begin with — that
// syntax is the SSH user shorthand, not a credential, and must stay as-is.
//
// This is the one canonical credential-stripping implementation for the
// module; pkg/taskset.SanitizeURL delegates to it rather than keeping a
// second copy.
func StripURLCredentials(rawURL string) string {
	return urlUserinfoRe.ReplaceAllString(rawURL, "$1")
}
