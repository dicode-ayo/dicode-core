package approval

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/task"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// commitAll initialises a repository at root and commits everything under it,
// returning the resulting commit ID.
func commitAll(t *testing.T, root string) string {
	t.Helper()
	repo, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("add: %v", err)
	}
	h, err := wt.Commit("seed", &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if h.IsZero() {
		t.Fatal("fixture produced no commit; every commit assertion below would pass vacuously")
	}
	return h.String()
}

// TestApproveRecordsCommitOfRepository is the end-to-end shape: a task inside a
// real clone pends, is approved, and the lock carries the commit its content
// was observed at.
func TestApproveRecordsCommitOfRepository(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	root := t.TempDir()
	spec := writeTaskDir(t, root, "repo/deploy", "export default () => {}")
	want := commitAll(t, root)

	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit: armed=%v err=%v", armed, err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	rec, ok := lock.Get("repo/deploy")
	if !ok {
		t.Fatal("no record written")
	}
	if rec.Commit != want {
		t.Fatalf("Commit = %q, want %q", rec.Commit, want)
	}
}

// TestApproveRecordsNoCommitOutsideRepository pins the degradation: a local
// source outside any repository approves normally and simply records no commit.
func TestApproveRecordsNoCommitOutsideRepository(t *testing.T) {
	g, arm, lock := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")

	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit: armed=%v err=%v", armed, err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	rec, ok := lock.Get("repo/deploy")
	if !ok {
		t.Fatal("no record written")
	}
	if rec.Commit != "" {
		t.Fatalf("Commit = %q, want empty outside a repository", rec.Commit)
	}
	if got := arm.armedIDs(); len(got) != 1 {
		t.Fatalf("task must arm on approval regardless of the commit: %v", got)
	}
}

// TestCommitCapturedAtPendNotAtApprove pins the ordering the design turns on:
// the commit belongs to the generation that pended. Reading HEAD at approve
// time would record whatever the repository moved to in between, pairing the
// approved hash with a commit whose tree never produced it.
func TestCommitCapturedAtPendNotAtApprove(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")

	atPend := fakeCommit("a")
	g.SetCommitFunc(func(k task.Kinded) string { return atPend })
	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit: armed=%v err=%v", armed, err)
	}

	// The repository moves on while the task sits pending, with no Admit in
	// between to observe it.
	g.SetCommitFunc(func(k task.Kinded) string { return fakeCommit("b") })
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	rec, _ := lock.Get("repo/deploy")
	if rec.Commit != atPend {
		t.Fatalf("Commit = %q, want the commit captured at pend time %q", rec.Commit, atPend)
	}
}

// TestPendTracksLatestCommitAtUnchangedHash covers the other half: while the
// task is still pending, each Admit re-observes the commit, so approving after
// a week of unrelated commits records where the content actually sits now
// rather than where it first appeared.
func TestPendTracksLatestCommitAtUnchangedHash(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")

	g.SetCommitFunc(func(k task.Kinded) string { return fakeCommit("a") })
	if _, err := g.Admit(spec); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	latest := fakeCommit("b")
	g.SetCommitFunc(func(k task.Kinded) string { return latest })
	if _, err := g.Admit(spec); err != nil {
		t.Fatalf("re-Admit: %v", err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	rec, _ := lock.Get("repo/deploy")
	if rec.Commit != latest {
		t.Fatalf("Commit = %q, want the most recently observed commit %q", rec.Commit, latest)
	}
}

// TestTrustedTaskRecordsCommit covers the auto-approve path: a trusted source
// never pends, but its lock entry is still an approval record and carries the
// commit its content came from.
func TestTrustedTaskRecordsCommit(t *testing.T) {
	policy := enabledPolicy()
	policy.TrustedSources["repo"] = true
	g, _, lock := newTestGate(t, policy)
	root := t.TempDir()
	spec := writeTaskDir(t, root, "repo/deploy", "export default () => {}")
	want := commitAll(t, root)

	if armed, err := g.Admit(spec); err != nil || !armed {
		t.Fatalf("trusted task must arm: armed=%v err=%v", armed, err)
	}
	rec, ok := lock.Get("repo/deploy")
	if !ok || rec.Commit != want {
		t.Fatalf("Commit = %q, want %q (ok=%v)", rec.Commit, want, ok)
	}
}

// TestUnchangedTrustedTaskSkipsCommitLookup pins the guard that keeps the
// steady-state reconcile free: at an unchanged hash the record is a no-op, so
// the repository must not be opened on every ~30s poll.
func TestUnchangedTrustedTaskSkipsCommitLookup(t *testing.T) {
	policy := enabledPolicy()
	policy.TrustedSources["repo"] = true
	g, _, _ := newTestGate(t, policy)
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")

	var calls atomic.Int32
	g.SetCommitFunc(func(k task.Kinded) string {
		calls.Add(1)
		return fakeCommit("a")
	})

	for i := 0; i < 3; i++ {
		if _, err := g.Admit(spec); err != nil {
			t.Fatalf("Admit %d: %v", i, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("commit resolved %d times across three polls of an unchanged task, want 1", got)
	}
}

// TestForgetDropsRecordedCommit pins the cost of an eviction: Forget removes
// the whole lock entry, so the recorded commit goes with it and the task
// returns as brand new rather than as one whose history is known.
func TestForgetDropsRecordedCommit(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	root := t.TempDir()
	spec := writeTaskDir(t, root, "repo/deploy", "export default () => {}")
	commitAll(t, root)

	if _, err := g.Admit(spec); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if rec, ok := lock.Get("repo/deploy"); !ok || rec.Commit == "" {
		t.Fatalf("precondition: expected a recorded commit, got %+v ok=%v", rec, ok)
	}

	g.Forget("repo/deploy")
	if _, ok := lock.Get("repo/deploy"); ok {
		t.Fatal("Forget must drop the record, commit included")
	}
}

// TestHeadCommitOfDirlessTask pins that an inline taskset entry, which has no
// directory to locate a repository from, resolves to no commit.
func TestHeadCommitOfDirlessTask(t *testing.T) {
	if got := headCommitOf(&task.Spec{ID: "repo/inline"}); got != "" {
		t.Fatalf("headCommitOf(dir-less) = %q, want empty", got)
	}
}

// TestApproveRecordsNoCommitForUntrackedTaskDir covers a local source whose
// tasks merely sit inside an unrelated repository — a folder under a
// version-controlled home directory. That repository's HEAD describes none of
// the task's content, so stamping the record with it would hand the review
// surface a commit range computed from a commit the task never appeared in.
func TestApproveRecordsNoCommitForUntrackedTaskDir(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "unrelated.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitAll(t, root)

	// Written after the commit, so the repository does not track it.
	spec := writeTaskDir(t, root, "repo/deploy", "export default () => {}")

	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit: armed=%v err=%v", armed, err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	rec, ok := lock.Get("repo/deploy")
	if !ok {
		t.Fatal("no record written")
	}
	if rec.Commit != "" {
		t.Fatalf("Commit = %q, want empty for a task the repository does not track", rec.Commit)
	}
}
