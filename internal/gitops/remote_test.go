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

func TestRemoteURL_ReturnsOriginURL(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "task.yaml"), "name: t\n")
	initRepo(t, root)
	addOrigin(t, root, "https://github.com/dicode-ayo/dicode-core.git")

	got, err := RemoteURL(root)
	if err != nil {
		t.Fatalf("RemoteURL: %v", err)
	}
	if want := "https://github.com/dicode-ayo/dicode-core.git"; got != want {
		t.Fatalf("RemoteURL = %q, want %q", got, want)
	}
}

// TestRemoteURL_NestedDirectory pins that, exactly like HeadCommit, dir need
// not be the repository root — parents are searched for it.
func TestRemoteURL_NestedDirectory(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "tasks", "deploy")
	writeFile(t, filepath.Join(nested, "task.yaml"), "name: t\n")
	initRepo(t, root)
	addOrigin(t, root, "git@github.com:dicode-ayo/dicode-core.git")

	got, err := RemoteURL(nested)
	if err != nil {
		t.Fatalf("RemoteURL: %v", err)
	}
	if want := "git@github.com:dicode-ayo/dicode-core.git"; got != want {
		t.Fatalf("RemoteURL = %q, want %q", got, want)
	}
}

func TestRemoteURL_NoOriginRemote(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "task.yaml"), "name: t\n")
	initRepo(t, root)

	if got, err := RemoteURL(root); err == nil {
		t.Fatalf("RemoteURL = %q, want an error for a repository with no origin remote", got)
	}
}

func TestRemoteURL_OutsideRepository(t *testing.T) {
	if _, err := RemoteURL(t.TempDir()); err == nil {
		t.Fatal("expected an error for a directory outside any repository")
	}
}

// TestRemoteURL_StripsCredentials pins that a remote URL carrying embedded
// userinfo never reaches the caller intact — this URL only ever feeds a link
// handed to a human.
func TestRemoteURL_StripsCredentials(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "task.yaml"), "name: t\n")
	initRepo(t, root)
	addOrigin(t, root, "https://user:ghp_supersecrettoken@github.com/dicode-ayo/dicode-core.git")

	got, err := RemoteURL(root)
	if err != nil {
		t.Fatalf("RemoteURL: %v", err)
	}
	if want := "https://github.com/dicode-ayo/dicode-core.git"; got != want {
		t.Fatalf("RemoteURL = %q, want %q (credentials must be stripped)", got, want)
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
