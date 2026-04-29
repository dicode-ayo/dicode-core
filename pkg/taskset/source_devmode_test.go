package taskset

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"go.uber.org/zap"
)

// newFixtureRemote creates a bare-ish git repo at a tempdir with a single
// commit on the given branch containing the provided files. Returns the repo's
// directory path (suitable as a `URL` for go-git PlainClone via file://).
func newFixtureRemote(t *testing.T, branch string, files map[string]string) string {
	t.Helper()

	bareDir := filepath.Join(t.TempDir(), "bare.git")
	if _, err := gogit.PlainInitWithOptions(bareDir, &gogit.PlainInitOptions{
		Bare:        true,
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName(branch)},
	}); err != nil {
		t.Fatalf("newFixtureRemote: init bare: %v", err)
	}

	wtPath := filepath.Join(t.TempDir(), "seed-wt")
	wt, err := gogit.PlainInitWithOptions(wtPath, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName(branch)},
	})
	if err != nil {
		t.Fatalf("newFixtureRemote: init wt: %v", err)
	}
	if _, err := wt.CreateRemote(&gogitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{bareDir},
	}); err != nil {
		t.Fatalf("newFixtureRemote: create remote: %v", err)
	}

	tree, err := wt.Worktree()
	if err != nil {
		t.Fatalf("newFixtureRemote: worktree: %v", err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(wtPath, name), []byte(content), 0o644); err != nil {
			t.Fatalf("newFixtureRemote: write %s: %v", name, err)
		}
		if _, err := tree.Add(name); err != nil {
			t.Fatalf("newFixtureRemote: add %s: %v", name, err)
		}
	}
	if _, err := tree.Commit("initial", &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()},
	}); err != nil {
		t.Fatalf("newFixtureRemote: commit: %v", err)
	}
	if err := wt.Push(&gogit.PushOptions{RemoteName: "origin"}); err != nil && err != gogit.NoErrAlreadyUpToDate {
		t.Fatalf("newFixtureRemote: push: %v", err)
	}

	return "file://" + bareDir
}

// newTestSourceWithRemote constructs a Source pointing at the given git remote URL.
func newTestSourceWithRemote(t *testing.T, namespace, remoteURL, branch string) *Source {
	t.Helper()
	return NewSource(
		remoteURL,
		namespace,
		&Ref{URL: remoteURL, Branch: branch},
		"",
		t.TempDir(),
		false,
		30*time.Second,
		zap.NewNop(),
	)
}

func TestSetDevMode_LocalPath_StillWorks(t *testing.T) {
	src := newTestSource(t, "ns", "/tmp/fixture-taskset.yaml")
	ctx := context.Background()

	if err := src.SetDevMode(ctx, true, DevModeOpts{LocalPath: "/tmp/fixture-taskset.yaml"}); err != nil {
		t.Fatalf("enable dev-mode with localPath: %v", err)
	}
	if !src.DevMode() {
		t.Fatal("DevMode() = false after enable, want true")
	}
	if got := src.DevRootPath(); got != "/tmp/fixture-taskset.yaml" {
		t.Errorf("DevRootPath = %q, want /tmp/fixture-taskset.yaml", got)
	}

	if err := src.SetDevMode(ctx, false, DevModeOpts{}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if src.DevMode() {
		t.Fatal("DevMode() = true after disable, want false")
	}
}

func TestSetDevMode_RejectsBothLocalPathAndBranch(t *testing.T) {
	src := newTestSource(t, "ns", "/tmp/fixture-taskset.yaml")
	err := src.SetDevMode(context.Background(), true, DevModeOpts{
		LocalPath: "/tmp/foo", Branch: "fix/x",
	})
	if err == nil {
		t.Fatal("expected error for both LocalPath and Branch set")
	}
}

func TestSetDevMode_Branch_ClonesRepo(t *testing.T) {
	remoteDir := newFixtureRemote(t, "main", map[string]string{
		"taskset.yaml": `apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: fixture
spec:
  entries: {}
`,
	})

	src := newTestSourceWithRemote(t, "ns", remoteDir, "main")
	ctx := context.Background()
	runID := "run-test-1"
	if err := src.SetDevMode(ctx, true, DevModeOpts{
		Branch: "fix/test-1",
		Base:   "main",
		RunID:  runID,
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !src.DevMode() {
		t.Fatal("DevMode() = false after enable")
	}

	wantPath := filepath.Join(src.DataDir(), "dev-clones", src.Namespace(), runID, "taskset.yaml")
	if got := src.DevRootPath(); got != wantPath {
		t.Errorf("DevRootPath = %q, want %q", got, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("clone taskset.yaml missing: %v", err)
	}

	// Branch should be checked out as fix/test-1 in the local clone
	cloneDir := filepath.Dir(wantPath)
	repo, err := gogit.PlainOpen(cloneDir)
	if err != nil {
		t.Fatalf("open clone repo: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("repo.Head: %v", err)
	}
	if got := head.Name().Short(); got != "fix/test-1" {
		t.Errorf("HEAD = %q, want fix/test-1", got)
	}
}

func TestSetDevMode_Branch_DisableRemovesClone(t *testing.T) {
	remoteDir := newFixtureRemote(t, "main", map[string]string{
		"taskset.yaml": `apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: fixture
spec:
  entries: {}
`,
	})
	src := newTestSourceWithRemote(t, "ns", remoteDir, "main")
	ctx := context.Background()
	runID := "run-disable-1"

	if err := src.SetDevMode(ctx, true, DevModeOpts{
		Branch: "fix/disable", Base: "main", RunID: runID,
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	clonePath := filepath.Join(src.DataDir(), "dev-clones", src.Namespace(), runID)
	if _, err := os.Stat(clonePath); err != nil {
		t.Fatalf("clone dir missing after enable: %v", err)
	}

	if err := src.SetDevMode(ctx, false, DevModeOpts{}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := os.Stat(clonePath); !os.IsNotExist(err) {
		t.Errorf("clone dir still exists after disable; err = %v", err)
	}
	if src.DevMode() {
		t.Error("DevMode() = true after disable")
	}
}

func TestSetDevMode_Branch_RefusesConcurrent(t *testing.T) {
	remoteDir := newFixtureRemote(t, "main", map[string]string{
		"taskset.yaml": `apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: fixture
spec:
  entries: {}
`,
	})
	src := newTestSourceWithRemote(t, "ns", remoteDir, "main")
	ctx := context.Background()

	if err := src.SetDevMode(ctx, true, DevModeOpts{Branch: "fix/a", Base: "main", RunID: "a"}); err != nil {
		t.Fatalf("first enable: %v", err)
	}
	err := src.SetDevMode(ctx, true, DevModeOpts{Branch: "fix/b", Base: "main", RunID: "b"})
	if !errors.Is(err, ErrDevModeBusy) {
		t.Errorf("got %v, want ErrDevModeBusy", err)
	}
}

func TestSetDevMode_Branch_RejectsTraversalRunID(t *testing.T) {
	remoteDir := newFixtureRemote(t, "main", map[string]string{
		"taskset.yaml": `apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: fixture
spec:
  entries: {}
`,
	})
	src := newTestSourceWithRemote(t, "ns", remoteDir, "main")
	err := src.SetDevMode(context.Background(), true, DevModeOpts{
		Branch: "fix/test", Base: "main", RunID: "../escape",
	})
	if !errors.Is(err, ErrInvalidRunID) {
		t.Errorf("got %v, want ErrInvalidRunID", err)
	}
}

func TestSetDevMode_Branch_ConcurrentSecondCallReturnsBusy(t *testing.T) {
	remoteDir := newFixtureRemote(t, "main", map[string]string{
		"taskset.yaml": `apiVersion: dicode/v1
kind: TaskSet
metadata: {name: fixture}
spec: {entries: {}}
`,
	})
	src := newTestSourceWithRemote(t, "ns", remoteDir, "main")
	ctx := context.Background()

	var wg sync.WaitGroup
	results := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		results[0] = src.SetDevMode(ctx, true, DevModeOpts{Branch: "fix/a", Base: "main", RunID: "ra"})
	}()
	go func() {
		defer wg.Done()
		results[1] = src.SetDevMode(ctx, true, DevModeOpts{Branch: "fix/b", Base: "main", RunID: "rb"})
	}()
	wg.Wait()

	// Exactly one nil, exactly one ErrDevModeBusy.
	nils := 0
	busies := 0
	for _, err := range results {
		switch {
		case err == nil:
			nils++
		case errors.Is(err, ErrDevModeBusy):
			busies++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if nils != 1 || busies != 1 {
		t.Errorf("got nils=%d busies=%d, want exactly 1 of each", nils, busies)
	}
}
