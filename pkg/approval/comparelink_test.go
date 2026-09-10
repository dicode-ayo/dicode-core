package approval

import (
	"strings"
	"testing"
)

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
			name:   "gitlab.com nested subgroup remote preserves the full namespace path",
			remote: "https://gitlab.com/group/subgroup/project.git",
			from:   from40, to: to40,
			want: "https://gitlab.com/group/subgroup/project/-/compare/" + from40 + "..." + to40,
		},
		{
			name:   "gitlab.com deeply nested subgroup remote",
			remote: "git@gitlab.com:group/subgroup/subsubgroup/project.git",
			from:   from40, to: to40,
			want: "https://gitlab.com/group/subgroup/subsubgroup/project/-/compare/" + from40 + "..." + to40,
		},
		{
			name:   "gitlab.com bare single-segment path is not a valid project reference",
			remote: "https://gitlab.com/onlyname",
			from:   from40, to: to40,
			want: "",
		},
		{
			name:   "github.com remote with more than two path segments produces no link",
			remote: "https://github.com/owner/repo/extra.git",
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
		{
			name:   "trailing FQDN-root dot still matches (github.com. names the same host as github.com)",
			remote: "https://github.com./owner/repo.git",
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
					if strings.Contains(got, leak) {
						t.Errorf("compareURL leaked credential material %q into %q", leak, got)
					}
				}
			}
		})
	}
}

func TestParseRemote(t *testing.T) {
	cases := []struct {
		name     string
		remote   string
		wantHost string
		wantPath string
		wantOK   bool
	}{
		{"https with .git", "https://github.com/o/r.git", "github.com", "o/r", true},
		{"https without .git", "https://github.com/o/r", "github.com", "o/r", true},
		{"scp-like", "git@github.com:o/r.git", "github.com", "o/r", true},
		{"ssh scheme", "ssh://git@github.com/o/r.git", "github.com", "o/r", true},
		{"gitlab https", "https://gitlab.com/group/proj.git", "gitlab.com", "group/proj", true},
		{"gitlab subgroup https", "https://gitlab.com/group/subgroup/project.git", "gitlab.com", "group/subgroup/project", true},
		{"empty", "", "", "", false},
		// parseRemote no longer decides owner/repo segment counts — that
		// is compareURL's per-host job now (see exactlyTwoSegments) — so a
		// single-segment path parses fine here; it just won't be a valid
		// GitHub repo reference downstream.
		{"single-segment path still parses", "https://github.com/onlyrepo", "github.com", "onlyrepo", true},
		{"garbage (no scheme, no host — treated as a local path)", "not a url at all ::::", "", "", false},
		{"uppercase/mixed-case host is lowercased", "https://GitHub.com/owner/repo.git", "github.com", "owner/repo", true},
		{"explicit port is stripped from the host", "https://github.com:443/owner/repo.git", "github.com", "owner/repo", true},
		{"trailing FQDN-root dot is dropped", "https://github.com./owner/repo.git", "github.com", "owner/repo", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, path, ok := parseRemote(tc.remote)
			if ok != tc.wantOK {
				t.Fatalf("parseRemote(%q) ok = %v, want %v", tc.remote, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if host != tc.wantHost || path != tc.wantPath {
				t.Errorf("parseRemote(%q) = (%q, %q), want (%q, %q)",
					tc.remote, host, path, tc.wantHost, tc.wantPath)
			}
		})
	}
}
