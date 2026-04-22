package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"go.uber.org/zap"

	"github.com/dicode/dicode/pkg/source"
)

// Tests for GitSource polling latency + Sync behaviour. Covers issue #125
// items 2 and 3: "push to a git source, assert registration within
// poll_interval + 1s" and "Sync triggers immediate rescan".
//
// No real network: everything runs against a local bare repo reachable via
// a `file://` URL through the production go-git code path.

// seededRepo wraps a bare repo on disk plus a detached worktree used to
// author commits. All pushes go through the worktree so the bare repo's
// refs advance as they would for a real remote.
type seededRepo struct {
	bareDir string
	url     string
	wt      *gogit.Repository
	wtPath  string
	branch  string
}

// newSeededRepo creates a bare repo with an initial commit registering one
// task at tasks/<taskID>/ (but the default scan target for GitSource is the
// repo root, so we place task dirs at the root level).
func newSeededRepo(t *testing.T, initialTaskID string) *seededRepo {
	t.Helper()

	bareDir := filepath.Join(t.TempDir(), "bare.git")
	if _, err := gogit.PlainInitWithOptions(bareDir, &gogit.PlainInitOptions{
		Bare:        true,
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")},
	}); err != nil {
		t.Fatalf("plain init bare: %v", err)
	}

	// Worktree used to author commits and push to the bare repo.
	wtPath := filepath.Join(t.TempDir(), "seed-worktree")
	wt, err := gogit.PlainInitWithOptions(wtPath, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")},
	})
	if err != nil {
		t.Fatalf("plain init worktree: %v", err)
	}
	if _, err := wt.CreateRemote(&gogitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{bareDir},
	}); err != nil {
		t.Fatalf("create remote: %v", err)
	}

	r := &seededRepo{
		bareDir: bareDir,
		url:     "file://" + bareDir,
		wt:      wt,
		wtPath:  wtPath,
		branch:  "main",
	}

	r.addCommit(t, initialTaskID, "init "+initialTaskID)
	r.push(t)
	return r
}

// addCommit writes a task.yaml + task.ts at <taskID>/ and commits.
func (r *seededRepo) addCommit(t *testing.T, taskID, msg string) {
	t.Helper()

	taskDir := filepath.Join(r.wtPath, taskID)
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yaml := "name: " + taskID + "\ntrigger:\n  manual: true\nruntime: deno\n"
	if err := os.WriteFile(filepath.Join(taskDir, "task.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatalf("write task.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "task.ts"), []byte("export default () => '"+taskID+"'\n"), 0644); err != nil {
		t.Fatalf("write task.ts: %v", err)
	}

	tree, err := r.wt.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if _, err := tree.Add(taskID); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := tree.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{Name: "e2e", Email: "e2e@test", When: time.Now()},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// push sends the current branch to origin.
func (r *seededRepo) push(t *testing.T) {
	t.Helper()
	err := r.wt.Push(&gogit.PushOptions{RemoteName: "origin"})
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		t.Fatalf("push: %v", err)
	}
}

// drain reads events non-blockingly until the channel has none available
// within timeout. Used after Start() to clear the initial-scan burst so
// the per-test assertion can see only the events we actually induce.
func drain(ch <-chan source.Event, timeout time.Duration) []source.Event {
	var out []source.Event
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
}

// waitForTaskEvent blocks until an event for taskID arrives or timeout
// elapses. Returns the event and the elapsed time since the call started.
func waitForTaskEvent(ch <-chan source.Event, taskID string, timeout time.Duration) (source.Event, time.Duration, bool) {
	start := time.Now()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return source.Event{}, time.Since(start), false
			}
			if ev.TaskID == taskID {
				return ev, time.Since(start), true
			}
		case <-deadline:
			return source.Event{}, time.Since(start), false
		}
	}
}

// TestGitSource_PollLatency covers #125 item 2: a commit pushed to a
// file:// remote should be picked up within poll_interval + 1 s. We use a
// 300 ms poll interval and a 1500 ms budget as the regression gate.
func TestGitSource_PollLatency(t *testing.T) {
	repo := newSeededRepo(t, "alpha")

	dataDir := t.TempDir()
	gs, err := New(dataDir, repo.url, "main", 300*time.Millisecond, "", "", zap.NewNop())
	if err != nil {
		t.Fatalf("New GitSource: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ch, err := gs.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Drain the initial-scan burst so the latency measurement only covers
	// the second commit.
	initial := drain(ch, 2*time.Second)
	seenAlpha := false
	for _, ev := range initial {
		if ev.TaskID == "alpha" {
			seenAlpha = true
		}
	}
	if !seenAlpha {
		t.Fatalf("initial scan did not emit alpha: %v", initial)
	}

	// Second commit: add a new task. Start the timer immediately before push.
	repo.addCommit(t, "beta", "add beta")
	start := time.Now()
	repo.push(t)

	_, _, ok := waitForTaskEvent(ch, "beta", 1500*time.Millisecond)
	latency := time.Since(start)
	if !ok {
		t.Fatalf("beta not observed within 1500 ms of push (elapsed %v)", latency)
	}
	t.Logf("[latency] git poll → registered: %v", latency)
	if latency > 1500*time.Millisecond {
		t.Errorf("poll latency %v exceeds budget 1500ms", latency)
	}
}

// TestGitSource_SyncTriggersImmediatePull covers #125 item 3 best we can
// without a dedicated webhook endpoint: calling Sync() on a GitSource
// should pull and update the snapshot faster than the normal poll tick.
//
// Note: Sync() does NOT emit events to the Start() channel (see git.go —
// it only calls diff() which updates g.snapshot). So the assertion here is
// that Sync() returns nil AND subsequent ScanDir of the local clone shows
// the new task — which is what a future webhook handler would observe via
// the registry API.
func TestGitSource_SyncTriggersImmediatePull(t *testing.T) {
	repo := newSeededRepo(t, "alpha")

	dataDir := t.TempDir()
	// Very long poll interval so only Sync() can explain an observed pull.
	gs, err := New(dataDir, repo.url, "main", 1*time.Hour, "", "", zap.NewNop())
	if err != nil {
		t.Fatalf("New GitSource: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ch, err := gs.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	drain(ch, 2*time.Second)

	// Commit a new task to the bare repo.
	repo.addCommit(t, "beta", "add beta")
	repo.push(t)

	// Sync should pull + refresh the snapshot well under 1 s (no network).
	start := time.Now()
	if err := gs.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	latency := time.Since(start)
	t.Logf("[latency] git sync: %v", latency)
	if latency > 1*time.Second {
		t.Errorf("Sync latency %v exceeds 1s budget", latency)
	}

	// The local clone should now contain beta/task.yaml.
	betaPath := filepath.Join(dataDir, "repos")
	// localDir is derived from sha256(url)[:8]; we don't reach into that,
	// just walk repos/ looking for beta.
	found := false
	if err := filepath.Walk(betaPath, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			t.Logf("walk %s: %v", p, err)
			return nil
		}
		if info.IsDir() && info.Name() == "beta" {
			if _, err := os.Stat(filepath.Join(p, "task.yaml")); err == nil {
				found = true
			}
		}
		return nil
	}); err != nil {
		t.Logf("filepath.Walk returned: %v", err)
	}
	if !found {
		t.Errorf("beta/task.yaml not found in local clone under %s", betaPath)
	}
}

// TestGitSource_IdempotentPoll covers the hash-stability contract from the
// reconciler's perspective: running two poll-equivalent sync cycles with
// no new commits must produce zero events the second time.
func TestGitSource_IdempotentPoll(t *testing.T) {
	repo := newSeededRepo(t, "alpha")

	dataDir := t.TempDir()
	gs, err := New(dataDir, repo.url, "main", 100*time.Millisecond, "", "", zap.NewNop())
	if err != nil {
		t.Fatalf("New GitSource: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ch, err := gs.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// First drain: initial scan should emit alpha exactly once.
	first := drain(ch, 500*time.Millisecond)
	alphaAdds := 0
	for _, ev := range first {
		if ev.TaskID == "alpha" && ev.Kind == source.EventAdded {
			alphaAdds++
		}
	}
	if alphaAdds != 1 {
		t.Fatalf("expected 1 alpha add, got %d (events: %v)", alphaAdds, first)
	}

	// Let a few poll ticks pass without any repo changes.
	second := drain(ch, 500*time.Millisecond)
	for _, ev := range second {
		if ev.TaskID == "alpha" {
			t.Errorf("alpha re-emitted on idle poll tick (kind=%s) — hash non-determinism in Hash()/ScanDir()", ev.Kind)
		}
	}
}
