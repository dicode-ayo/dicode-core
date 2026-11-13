package taskset

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"go.uber.org/zap"

	"github.com/dicode/dicode/internal/gitops"
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

	// See gitops.TestFixtureRemoteURL's doc comment for why this needs a
	// placeholder hostname rather than a bare file:// path.
	return gitops.TestFixtureRemoteURL(bareDir)
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

func TestSetDevMode_Branch_AllowsConcurrentSessions(t *testing.T) {
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

	// First session.
	if err := src.SetDevMode(ctx, true, DevModeOpts{Branch: "fix/a", Base: "main", RunID: "a"}); err != nil {
		t.Fatalf("first enable: %v", err)
	}
	// Second session with a different RunID must succeed (no busy error).
	if err := src.SetDevMode(ctx, true, DevModeOpts{Branch: "fix/b", Base: "main", RunID: "b"}); err != nil {
		t.Fatalf("second enable (different runID): %v", err)
	}
	// Primary should be the most-recently-enabled session.
	if got := filepath.Base(src.RepoPath()); got != "b" {
		t.Errorf("RepoPath base = %q, want \"b\" (latest primary)", got)
	}
}

func TestSetDevMode_Branch_RefusesRunIDCollision(t *testing.T) {
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

	if err := src.SetDevMode(ctx, true, DevModeOpts{Branch: "fix/a", Base: "main", RunID: "dup"}); err != nil {
		t.Fatalf("first enable: %v", err)
	}
	// Same RunID must return ErrDevModeBusy.
	err := src.SetDevMode(ctx, true, DevModeOpts{Branch: "fix/b", Base: "main", RunID: "dup"})
	if !errors.Is(err, ErrDevModeBusy) {
		t.Errorf("got %v, want ErrDevModeBusy for duplicate run ID", err)
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

func TestSetDevMode_Branch_ConcurrentDifferentRunIDsBothSucceed(t *testing.T) {
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

	// Both should succeed with different RunIDs.
	for i, err := range results {
		if err != nil {
			t.Errorf("goroutine %d: got %v, want nil", i, err)
		}
	}
}

func TestSetDevMode_Branch_TargetedDisableRemovesOnlyNamedSession(t *testing.T) {
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
		t.Fatalf("enable a: %v", err)
	}
	if err := src.SetDevMode(ctx, true, DevModeOpts{Branch: "fix/b", Base: "main", RunID: "b"}); err != nil {
		t.Fatalf("enable b: %v", err)
	}
	cloneA := filepath.Join(src.DataDir(), "dev-clones", src.Namespace(), "a")
	cloneB := filepath.Join(src.DataDir(), "dev-clones", src.Namespace(), "b")

	// Disable only session "b" (the current primary).
	if err := src.SetDevMode(ctx, false, DevModeOpts{RunID: "b"}); err != nil {
		t.Fatalf("disable b: %v", err)
	}
	if _, err := os.Stat(cloneB); !os.IsNotExist(err) {
		t.Errorf("clone b should be removed after targeted disable; stat err = %v", err)
	}
	if _, err := os.Stat(cloneA); err != nil {
		t.Errorf("clone a should still exist: %v", err)
	}
	// Still in dev mode because session "a" is active.
	if !src.DevMode() {
		t.Error("DevMode() = false after targeted disable of non-last session")
	}
}

// TestSetDevMode_Branch_RejectsSSRFHost is the regression test for #510
// item 2: enableClone (reached via SetDevMode from pkg/webui/sources.go and
// pkg/webui/task_delete.go) drove a real go-git PlainCloneContext against
// s.rootRef.URL with zero host validation — a third, unmitigated SSRF entry
// point alongside CloneAtRef and ListBranches (#489). This proves a
// malicious ssh/scp-shorthand rootRef.URL is rejected before any clone is
// attempted: no clone directory should ever be created on disk.
func TestSetDevMode_Branch_RejectsSSRFHost(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{name: "scp-shorthand loopback", url: "git@127.0.0.1:org/repo.git"},
		{name: "ssh loopback", url: "ssh://git@127.0.0.1/org/repo.git"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := newTestSourceWithRemote(t, "ns", tc.url, "main")
			ctx := context.Background()
			runID := "ssrf-" + strings.ReplaceAll(tc.name, " ", "-")

			_, err := src.enableClone(ctx, DevModeOpts{
				Branch: "fix/test", Base: "main", RunID: runID,
			})
			if err == nil {
				t.Fatalf("enableClone(%q) = nil error, want SSRF host rejection", tc.url)
			}
			if !errors.Is(err, gitops.ErrBlockedHost) {
				t.Errorf("enableClone(%q) error = %v, want errors.Is(gitops.ErrBlockedHost)", tc.url, err)
			}

			clonePath := filepath.Join(src.DataDir(), "dev-clones", src.Namespace(), runID)
			if _, statErr := os.Stat(clonePath); !os.IsNotExist(statErr) {
				t.Errorf("enableClone(%q) populated %s — a clone was attempted", tc.url, clonePath)
			}

			// Also drive it through the public SetDevMode entry point to
			// prove the guard is reachable end-to-end, not just when calling
			// enableClone directly.
			err = src.SetDevMode(ctx, true, DevModeOpts{
				Branch: "fix/test", Base: "main", RunID: runID + "-public",
			})
			if err == nil {
				t.Fatalf("SetDevMode with malicious url %q = nil error, want SSRF host rejection", tc.url)
			}
			if !errors.Is(err, gitops.ErrBlockedHost) {
				t.Errorf("SetDevMode error = %v, want errors.Is(gitops.ErrBlockedHost)", err)
			}
			publicClonePath := filepath.Join(src.DataDir(), "dev-clones", src.Namespace(), runID+"-public")
			if _, statErr := os.Stat(publicClonePath); !os.IsNotExist(statErr) {
				t.Errorf("SetDevMode populated %s — a clone was attempted", publicClonePath)
			}
		})
	}
}

func TestSetDevMode_Branch_PrimaryPromotedAfterDisable(t *testing.T) {
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
		t.Fatalf("enable a: %v", err)
	}
	if err := src.SetDevMode(ctx, true, DevModeOpts{Branch: "fix/b", Base: "main", RunID: "b"}); err != nil {
		t.Fatalf("enable b: %v", err)
	}
	// b is now primary.
	if got := filepath.Base(src.RepoPath()); got != "b" {
		t.Fatalf("expected b to be primary, RepoPath base = %q", got)
	}

	// Disable b; a should become primary.
	if err := src.SetDevMode(ctx, false, DevModeOpts{RunID: "b"}); err != nil {
		t.Fatalf("disable b: %v", err)
	}
	if got := filepath.Base(src.RepoPath()); got != "a" {
		t.Errorf("after disabling b, expected a to be primary, RepoPath base = %q", got)
	}
}

// TestSetDevMode_PinnedSourceForksFromTheTag covers clone-mode on a source
// that has no branch to fork from. The fix branch must start at the commit the
// source actually runs — the pinned one — not at whatever the default branch
// has moved on to.
func TestSetDevMode_PinnedSourceForksFromTheTag(t *testing.T) {
	bare := newSeededBareRepo(t)
	bare.commitTree(t, map[string]string{"taskset.yaml": "pinned"}, "release")
	bare.tagHead(t, "v1.0.0")
	bare.commitTree(t, map[string]string{"taskset.yaml": "moved on"}, "advance")

	src := NewSource(bare.url, "ns", &Ref{URL: bare.url, Tag: "v1.0.0"}, "", t.TempDir(), false, 30*time.Second, zap.NewNop())
	runID := "run-pinned"
	if err := src.SetDevMode(context.Background(), true, DevModeOpts{Branch: "fix/pinned", RunID: runID}); err != nil {
		t.Fatalf("enable clone-mode on a pinned source: %v", err)
	}

	clonePath := filepath.Join(src.DataDir(), "dev-clones", src.Namespace(), runID)
	repo, err := gogit.PlainOpen(clonePath)
	if err != nil {
		t.Fatalf("open clone repo: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("repo.Head: %v", err)
	}
	if got := head.Name().Short(); got != "fix/pinned" {
		t.Errorf("HEAD = %q, want fix/pinned", got)
	}
	body, err := os.ReadFile(filepath.Join(clonePath, "taskset.yaml"))
	if err != nil {
		t.Fatalf("read cloned taskset.yaml: %v", err)
	}
	if string(body) != "pinned" {
		t.Errorf("fix branch forked from %q, want the pinned commit's %q", string(body), "pinned")
	}
}
