package gitops

import "testing"

// TestStripURLCredentials is the canonical table for StripURLCredentials —
// the case set merged from the old net/url-based implementation's table
// (internal/gitops) and the old regex-based pkg/taskset.SanitizeURL's table,
// now that both delegate to this one function.
func TestStripURLCredentials(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no credentials", "https://github.com/o/r.git", "https://github.com/o/r.git"},
		{"user and password", "https://user:pass@github.com/o/r.git", "https://github.com/o/r.git"},
		{"token as password", "https://x-access-token:ghp_abc@github.com/o/r.git", "https://github.com/o/r.git"},
		{"scp-like, no scheme, left untouched (SSH user shorthand)", "git@github.com:o/r.git", "git@github.com:o/r.git"},

		// The exact discrepancy fixed by this refactor: net/url.Parse
		// (Go 1.20+) rejects a non-numeric "port" position, so the old
		// gitops.StripURLCredentials silently returned this credentialed
		// URL unchanged. The regex-based implementation has no such
		// structural requirement and strips it, matching what
		// taskset.SanitizeURL already did for this shape.
		{"ssh scheme with owner-shorthand non-numeric port", "ssh://git@github.com:org/repo.git", "ssh://github.com:org/repo.git"},
		{"ssh scheme with owner-shorthand, nested path", "ssh://deploy:s3cr3t@github.com:org/repo.git", "ssh://github.com:org/repo.git"},

		// Cases carried over from pkg/taskset.SanitizeURL's table: URLs
		// embedded in arbitrary surrounding text, as seen in git error
		// messages.
		{
			"embedded URL, @ is part of a ref not userinfo (no ://@ run)",
			"pull https://github.com/org/repo@branch:main: authentication required",
			"pull https://github.com/org/repo@branch:main: authentication required",
		},
		{
			"embedded URL, @ is part of a tag ref",
			"pull https://github.com/org/repo@tag:v1.0.0: authentication required",
			"pull https://github.com/org/repo@tag:v1.0.0: authentication required",
		},
		{
			"embedded URL with real userinfo inside a longer message",
			"git clone failed: https://oauth2:ghp_abc123@github.com/org/repo.git",
			"git clone failed: https://github.com/org/repo.git",
		},
		{
			"percent-encoded password",
			"fetch https://user:p%40ss@example.com/r.git: ok",
			"fetch https://example.com/r.git: ok",
		},
		{"no URL at all", "no URL here", "no URL here"},
		{"empty string", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripURLCredentials(tc.in); got != tc.want {
				t.Errorf("StripURLCredentials(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
