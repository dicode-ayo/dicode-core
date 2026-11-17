package gitops

import (
	"runtime"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"
)

func TestTestFixtureRemoteURL(t *testing.T) {
	cases := []struct {
		name, dir, wantPath string
	}{
		{"posix", "/tmp/repo.git", "/tmp/repo.git"},
		{"drive with forward slashes", "C:/Users/a/repo.git", "/C:/Users/a/repo.git"},
		{"space", "/tmp/my repo.git", "/tmp/my repo.git"},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases, struct{ name, dir, wantPath string }{
			"drive with backslashes", `C:\Users\a\repo.git`, "/C:/Users/a/repo.git",
		})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := TestFixtureRemoteURL(tc.dir)
			ep, err := transport.NewEndpoint(u)
			if err != nil {
				t.Fatalf("NewEndpoint(%q): %v", u, err)
			}
			if ep.Host != "test-fixture.invalid" || ep.Port != 0 || ep.Path != tc.wantPath {
				t.Errorf("NewEndpoint(%q) = host %q port %d path %q, want host %q port 0 path %q",
					u, ep.Host, ep.Port, ep.Path, "test-fixture.invalid", tc.wantPath)
			}
			if err := ValidateRemoteHost(u); err != nil {
				t.Errorf("ValidateRemoteHost(%q): %v", u, err)
			}
		})
	}
}
