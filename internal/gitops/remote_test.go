package gitops

import (
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
)

// addOrigin creates an "origin" remote pointing at url on the repository at
// root, which must already exist (see initRepo in head_test.go).
func addOrigin(t *testing.T, root, url string) {
	t.Helper()
	repo, err := gogit.PlainOpen(root)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	if _, err := repo.CreateRemote(&gogitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{url},
	}); err != nil {
		t.Fatalf("CreateRemote: %v", err)
	}
}

func TestHeadInfo_ReturnsOriginURL(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "task.yaml"), "name: t\n")
	initRepo(t, root)
	addOrigin(t, root, "https://github.com/dicode-ayo/dicode-core.git")

	_, got, err := HeadInfo(root)
	if err != nil {
		t.Fatalf("HeadInfo: %v", err)
	}
	if want := "https://github.com/dicode-ayo/dicode-core.git"; got != want {
		t.Fatalf("remote = %q, want %q", got, want)
	}
}

// TestHeadInfo_NestedDirectoryResolvesOrigin pins that the remote resolves
// from a directory nested inside the repository, not only from its root.
func TestHeadInfo_NestedDirectoryResolvesOrigin(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "tasks", "deploy")
	writeFile(t, filepath.Join(nested, "task.yaml"), "name: t\n")
	initRepo(t, root)
	addOrigin(t, root, "git@github.com:dicode-ayo/dicode-core.git")

	_, got, err := HeadInfo(nested)
	if err != nil {
		t.Fatalf("HeadInfo: %v", err)
	}
	if want := "git@github.com:dicode-ayo/dicode-core.git"; got != want {
		t.Fatalf("remote = %q, want %q", got, want)
	}
}

// TestHeadInfo_NoOriginRemote pins that a missing "origin" is not an error:
// the commit still resolves and the remote is simply absent.
func TestHeadInfo_NoOriginRemote(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "task.yaml"), "name: t\n")
	initRepo(t, root)

	commit, remote, err := HeadInfo(root)
	if err != nil {
		t.Fatalf("HeadInfo: %v", err)
	}
	if commit == "" {
		t.Error("commit is empty, want the HEAD commit")
	}
	if remote != "" {
		t.Errorf("remote = %q, want %q", remote, "")
	}
}

// TestHeadInfo_StripsCredentials pins that a remote URL carrying embedded
// userinfo never reaches the caller intact — it only ever feeds a link
// handed to a human.
func TestHeadInfo_StripsCredentials(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "task.yaml"), "name: t\n")
	initRepo(t, root)
	addOrigin(t, root, "https://user:ghp_supersecrettoken@github.com/dicode-ayo/dicode-core.git")

	_, got, err := HeadInfo(root)
	if err != nil {
		t.Fatalf("HeadInfo: %v", err)
	}
	if want := "https://github.com/dicode-ayo/dicode-core.git"; got != want {
		t.Fatalf("remote = %q, want %q (credentials must be stripped)", got, want)
	}
}

func TestStripCredentials(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no credentials", "https://github.com/o/r.git", "https://github.com/o/r.git"},
		{"user and password", "https://user:pass@github.com/o/r.git", "https://github.com/o/r.git"},
		{"token as password", "https://x-access-token:ghp_abc@github.com/o/r.git", "https://github.com/o/r.git"},
		{"scp-like, unparseable as a URL scheme", "git@github.com:o/r.git", "git@github.com:o/r.git"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripURLCredentials(tc.in); got != tc.want {
				t.Errorf("StripURLCredentials(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseRemote(t *testing.T) {
	cases := []struct {
		name           string
		in             string
		wantHost, want string
		wantOK         bool
	}{
		{"https with .git", "https://github.com/o/r.git", "github.com", "o/r", true},
		{"scp-like shorthand", "git@gitlab.com:group/sub/p.git", "gitlab.com", "group/sub/p", true},
		{"ssh scheme with port", "ssh://git@github.com:2222/o/r.git", "github.com", "o/r", true},
		{"mixed case and FQDN-root dot", "https://GitHub.Com./o/r.git", "github.com", "o/r", true},
		{"empty", "", "", "", false},
		{"unparseable", "not a url at all", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, path, ok := ParseRemote(tc.in)
			if ok != tc.wantOK || host != tc.wantHost || path != tc.want {
				t.Errorf("ParseRemote(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.in, host, path, ok, tc.wantHost, tc.want, tc.wantOK)
			}
		})
	}
}
