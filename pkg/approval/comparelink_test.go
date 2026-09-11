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
		})
	}
}
