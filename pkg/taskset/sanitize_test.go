package taskset

import "testing"

// The full case table lives in internal/gitops (TestStripURLCredentials),
// which owns the actual stripping logic. These are smoke tests confirming
// SanitizeURL / sanitizeErrorString correctly delegate to it under this
// package's own public names.
func TestSanitizeErrorString_StripsUserinfo(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{
			in:   `git clone failed: https://oauth2:ghp_abc123@github.com/org/repo.git`,
			want: `git clone failed: https://github.com/org/repo.git`,
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

func TestSanitizeURL_StripsUserinfo(t *testing.T) {
	in := "https://user:pass@github.com/o/r.git"
	want := "https://github.com/o/r.git"
	if got := SanitizeURL(in); got != want {
		t.Errorf("SanitizeURL(%q) = %q; want %q", in, got, want)
	}
}
