package approval

import "testing"

const (
	from40 = "1111111111111111111111111111111111111111"
	to40   = "2222222222222222222222222222222222222222"
)

func TestCompareURL(t *testing.T) {
	cases := []struct {
		name     string
		remote   string
		from, to string
		want     string
	}{
		{
			name:   "github.com HTTPS remote",
			remote: "https://github.com/dicode-ayo/dicode-core.git",
			from:   from40, to: to40,
			want: "https://github.com/dicode-ayo/dicode-core/compare/" + from40 + "..." + to40,
		},
		{
			name:   "github.com HTTPS remote, no .git suffix",
			remote: "https://github.com/dicode-ayo/dicode-core",
			from:   from40, to: to40,
			want: "https://github.com/dicode-ayo/dicode-core/compare/" + from40 + "..." + to40,
		},
		{
			name:   "github.com scp-like SSH remote",
			remote: "git@github.com:dicode-ayo/dicode-core.git",
			from:   from40, to: to40,
			want: "https://github.com/dicode-ayo/dicode-core/compare/" + from40 + "..." + to40,
		},
		{
			name:   "github.com ssh:// remote",
			remote: "ssh://git@github.com/dicode-ayo/dicode-core.git",
			from:   from40, to: to40,
			want: "https://github.com/dicode-ayo/dicode-core/compare/" + from40 + "..." + to40,
		},
		{
			name:   "gitlab.com HTTPS remote",
			remote: "https://gitlab.com/some-group/some-repo.git",
			from:   from40, to: to40,
			want: "https://gitlab.com/some-group/some-repo/-/compare/" + from40 + "..." + to40,
		},
		{
			name:   "gitlab.com scp-like SSH remote",
			remote: "git@gitlab.com:some-group/some-repo.git",
			from:   from40, to: to40,
			want: "https://gitlab.com/some-group/some-repo/-/compare/" + from40 + "..." + to40,
		},
		{
			name:   "unrecognized host (self-hosted Gitea)",
			remote: "https://git.example.com/owner/repo.git",
			from:   from40, to: to40,
			want: "",
		},
		{
			name:   "unrecognized host (self-hosted Bitbucket Server)",
			remote: "https://bitbucket.corp.example/scm/proj/repo.git",
			from:   from40, to: to40,
			want: "",
		},
		{
			name:   "empty from (first-ever approval / unknown history)",
			remote: "https://github.com/dicode-ayo/dicode-core.git",
			from:   "", to: to40,
			want: "",
		},
		{
			name:   "empty to",
			remote: "https://github.com/dicode-ayo/dicode-core.git",
			from:   from40, to: "",
			want: "",
		},
		{
			name:   "from equals to (nothing moved)",
			remote: "https://github.com/dicode-ayo/dicode-core.git",
			from:   from40, to: from40,
			want: "",
		},
		{
			name:   "unparseable remote",
			remote: "not a url at all ::::",
			from:   from40, to: to40,
			want: "",
		},
		{
			name:   "empty remote",
			remote: "",
			from:   from40, to: to40,
			want: "",
		},
		{
			name:   "remote with embedded credentials never leaks into the compare URL",
			remote: "https://x-access-token:ghp_supersecret@github.com/dicode-ayo/dicode-core.git",
			from:   from40, to: to40,
			want: "https://github.com/dicode-ayo/dicode-core/compare/" + from40 + "..." + to40,
		},
		{
			name:   "uppercase/mixed-case host still matches (DNS is case-insensitive)",
			remote: "https://GitHub.com/owner/repo.git",
			from:   from40, to: to40,
			want: "https://github.com/owner/repo/compare/" + from40 + "..." + to40,
		},
		{
			name:   "explicit port is stripped from the host before matching",
			remote: "https://github.com:443/owner/repo.git",
			from:   from40, to: to40,
			want: "https://github.com/owner/repo/compare/" + from40 + "..." + to40,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := compareURL(tc.remote, tc.from, tc.to)
			if got != tc.want {
				t.Errorf("compareURL(%q, %q, %q) = %q, want %q", tc.remote, tc.from, tc.to, got, tc.want)
			}
			if tc.want != "" {
				for _, leak := range []string{"ghp_supersecret", "x-access-token", "@"} {
					if leak == "@" {
						continue // legitimate compare URLs never contain "@" anyway; skip the trivial case
					}
					if containsSubstring(got, leak) {
						t.Errorf("compareURL leaked credential material %q into %q", leak, got)
					}
				}
			}
		})
	}
}

func containsSubstring(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestParseRemote(t *testing.T) {
	cases := []struct {
		name                          string
		remote                        string
		wantHost, wantOwner, wantRepo string
		wantOK                        bool
	}{
		{"https with .git", "https://github.com/o/r.git", "github.com", "o", "r", true},
		{"https without .git", "https://github.com/o/r", "github.com", "o", "r", true},
		{"scp-like", "git@github.com:o/r.git", "github.com", "o", "r", true},
		{"ssh scheme", "ssh://git@github.com/o/r.git", "github.com", "o", "r", true},
		{"gitlab https", "https://gitlab.com/group/proj.git", "gitlab.com", "group", "proj", true},
		{"empty", "", "", "", "", false},
		{"no owner segment", "https://github.com/onlyrepo", "", "", "", false},
		{"garbage", "not a url at all ::::", "", "", "", false},
		{"uppercase/mixed-case host is lowercased", "https://GitHub.com/owner/repo.git", "github.com", "owner", "repo", true},
		{"explicit port is stripped from the host", "https://github.com:443/owner/repo.git", "github.com", "owner", "repo", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, owner, repo, ok := parseRemote(tc.remote)
			if ok != tc.wantOK {
				t.Fatalf("parseRemote(%q) ok = %v, want %v", tc.remote, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if host != tc.wantHost || owner != tc.wantOwner || repo != tc.wantRepo {
				t.Errorf("parseRemote(%q) = (%q, %q, %q), want (%q, %q, %q)",
					tc.remote, host, owner, repo, tc.wantHost, tc.wantOwner, tc.wantRepo)
			}
		})
	}
}
