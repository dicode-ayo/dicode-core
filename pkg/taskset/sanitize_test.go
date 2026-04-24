package taskset

import "testing"

// The regex is duplicated from pkg/relay — keep these tests parallel to
// pkg/relay/sanitize_test.go so a future tightening in one place gets
// caught by a failing assertion in the other (as long as the fix is
// applied to both copies).
func TestSanitizeErrorString_StripsUserinfo(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{
			in:   `pull https://github.com/org/repo@main: authentication required`,
			want: `pull https://github.com/org/repo@main: authentication required`,
		},
		{
			in:   `git clone failed: https://oauth2:ghp_abc123@github.com/org/repo.git`,
			want: `git clone failed: https://github.com/org/repo.git`,
		},
		{
			in:   `fetch https://user:p%40ss@example.com/r.git: ok`,
			want: `fetch https://example.com/r.git: ok`,
		},
		{
			in:   `no URL here`,
			want: `no URL here`,
		},
		{
			in:   `ssh://git@github.com:org/repo.git`,
			want: `ssh://github.com:org/repo.git`,
		},
	}
	for _, tc := range cases {
		if got := sanitizeErrorString(tc.in); got != tc.want {
			t.Errorf("sanitizeErrorString(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}
