package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
)

func TestCommitPush_AddsAndCommits(t *testing.T) {
	// Create a fixture: bare remote + local init with remote pointing at it;
	// write a file; CommitPush it; verify the commit lands in the local repo HEAD.
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "remote.git")
	if _, err := gogit.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(tmp, "local")
	repo, err := gogit.PlainClone(local, false, &gogit.CloneOptions{URL: bare})
	if err != nil {
		// Empty bare repo — clone fails; init + add remote instead.
		repo, err = gogit.PlainInit(local, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.CreateRemote(&gogitconfig.RemoteConfig{
			Name: "origin",
			URLs: []string{bare},
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(filepath.Join(local, "hello.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	hash, err := CommitPush(context.Background(), local, CommitPushOptions{
		Message:   "test commit",
		Branch:    "main",
		Files:     []string{"hello.txt"},
		AllowMain: true,
		Author:    Signature{Name: "Test", Email: "test@example.com"},
	})
	if err != nil {
		t.Fatalf("CommitPush: %v", err)
	}
	if hash == "" {
		t.Error("returned empty hash")
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if head.Hash().String() != hash {
		t.Errorf("HEAD = %s, want %s", head.Hash().String(), hash)
	}
}

func TestCommitPush_RefusesOutOfPrefix(t *testing.T) {
	tmp := t.TempDir()
	repo, err := gogit.PlainInit(tmp, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = repo

	_, err = CommitPush(context.Background(), tmp, CommitPushOptions{
		Message:      "x",
		Branch:       "hotfix/foo", // doesn't match "fix/" prefix
		BranchPrefix: "fix/",
		AllowMain:    false,
		Author:       Signature{Name: "T", Email: "t@x"},
	})
	if err == nil {
		t.Error("expected error for out-of-prefix branch")
	}
}

func TestCommitPush_RefusesEmptyMessage(t *testing.T) {
	tmp := t.TempDir()
	repo, err := gogit.PlainInit(tmp, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = repo

	_, err = CommitPush(context.Background(), tmp, CommitPushOptions{
		Message:   "",
		Branch:    "main",
		AllowMain: true,
		Author:    Signature{Name: "T", Email: "t@x"},
	})
	if err == nil {
		t.Error("expected error for empty commit message")
	}
}

// Empty BranchPrefix used to vacuously bypass the prefix guard, leaving
// AllowMain=true as a permissive global allow. Now an empty prefix is itself
// only legal when AllowMain=true AND the branch is main/master.
func TestCommitPush_EmptyPrefixWithoutMainBranchRejected(t *testing.T) {
	tmp := t.TempDir()
	if _, err := gogit.PlainInit(tmp, false); err != nil {
		t.Fatal(err)
	}
	_, err := CommitPush(context.Background(), tmp, CommitPushOptions{
		Message:   "x",
		Branch:    "feature/foo",
		AllowMain: true, // does not help — branch isn't main/master
		Author:    Signature{Name: "T", Email: "t@x"},
	})
	if err == nil {
		t.Error("expected error: empty BranchPrefix + non-main/master branch must be rejected even with AllowMain=true")
	}
}

func TestCommitPush_EmptyPrefixNoAllowMainRejected(t *testing.T) {
	tmp := t.TempDir()
	if _, err := gogit.PlainInit(tmp, false); err != nil {
		t.Fatal(err)
	}
	_, err := CommitPush(context.Background(), tmp, CommitPushOptions{
		Message:   "x",
		Branch:    "main",
		AllowMain: false,
		Author:    Signature{Name: "T", Email: "t@x"},
	})
	if err == nil {
		t.Error("expected error: empty BranchPrefix + AllowMain=false must be rejected")
	}
}
