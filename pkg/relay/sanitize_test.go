package relay

import "testing"

func TestSanitizeErrorString_StripsUserinfo(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{
			in:   `dial relay: ws://user:s3cret@relay.example/u/abc: EOF`,
			want: `dial relay: ws://relay.example/u/abc: EOF`,
		},
		{
			in:   `git clone failed: https://oauth2:ghp_abc123@github.com/org/repo.git`,
			want: `git clone failed: https://github.com/org/repo.git`,
		},
		{
			in:   `no URL here, just a plain error`,
			want: `no URL here, just a plain error`,
		},
		{
			// No userinfo → unchanged.
			in:   `wss://relay.example/u/abc/hello`,
			want: `wss://relay.example/u/abc/hello`,
		},
		{
			// Two URLs in one string, both carrying secrets.
			in:   `primary https://pat@a.example/x fell back to ssh://key@b.example/y`,
			want: `primary https://a.example/x fell back to ssh://b.example/y`,
		},
	}
	for _, tc := range cases {
		if got := sanitizeErrorString(tc.in); got != tc.want {
			t.Errorf("sanitizeErrorString(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}
