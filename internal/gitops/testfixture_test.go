package gitops

import (
	"runtime"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"
)

func TestTestFixtureRemoteURL(t *testing.T) {
	type tcase struct{ name, dir, wantURL, wantPath string }
	cases := []tcase{
		{"drive with forward slashes", "C:/Users/a/repo.git", "file://test-fixture.invalid/C:/Users/a/repo.git", "/C:/Users/a/repo.git"},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases,
			tcase{"drive with backslashes", `C:\Users\a\repo.git`, "file://test-fixture.invalid/C:/Users/a/repo.git", "/C:/Users/a/repo.git"},
			tcase{"space", `C:\tmp\my repo.git`, "file://test-fixture.invalid/C:/tmp/my%20repo.git", "/C:/tmp/my repo.git"},
		)
	} else {
		cases = append(cases,
			tcase{"posix", "/tmp/repo.git", "file://test-fixture.invalid/tmp/repo.git", "/tmp/repo.git"},
			tcase{"space", "/tmp/my repo.git", "file://test-fixture.invalid/tmp/my%20repo.git", "/tmp/my repo.git"},
		)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := TestFixtureRemoteURL(tc.dir)
			if u != tc.wantURL {
				t.Errorf("TestFixtureRemoteURL(%q) = %q, want %q", tc.dir, u, tc.wantURL)
			}
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

func TestTestFixtureRemoteURL_RelativePathPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal(`TestFixtureRemoteURL("repo.git") did not panic`)
		}
	}()
	TestFixtureRemoteURL("repo.git")
}

func TestTestFixtureRemoteURL_RootedWithoutDrivePanicsOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("a rooted path without a drive letter is absolute everywhere but Windows")
	}
	defer func() {
		if recover() == nil {
			t.Fatal(`TestFixtureRemoteURL("\\repo.git") did not panic`)
		}
	}()
	TestFixtureRemoteURL(`\repo.git`)
}
