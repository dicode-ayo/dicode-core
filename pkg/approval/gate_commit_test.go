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

// ── PendingApproval ──────────────────────────────────────────────────────────

// TestPendingApproval_FirstApprovalHasNoFrom pins the ordinary state for a
// task pending for the first time: there is no prior lock record, so From is
// empty even though a commit was observed for the currently pending content.
func TestPendingApproval_FirstApprovalHasNoFrom(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")

	to := fakeCommit("b")
	g.SetCommitFunc(func(k task.Kinded) string { return to })
	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit: armed=%v err=%v", armed, err)
	}

	hash, cr, ok := g.PendingApproval("repo/deploy")
	if !ok {
		t.Fatal("PendingApproval: ok = false, want true for a pending task")
	}
	if hash == "" {
		t.Error("hash = \"\", want a non-empty observed hash")
	}
	if cr.From != "" {
		t.Errorf("From = %q, want empty (no prior approval)", cr.From)
	}
	if cr.To != to {
		t.Errorf("To = %q, want %q", cr.To, to)
	}
	if cr.CompareURL != "" {
		t.Errorf("CompareURL = %q, want empty (no remote configured)", cr.CompareURL)
	}
}

// TestPendingApproval_CachesRemoteAtAdmitTime pins the fix for a round-5
// code-review finding: the remote is resolved once per Admit (alongside
// commit, in the same pattern pendingEntry.remote's doc comment describes)
// and cached on the pending entry, rather than re-walked by PendingApproval
// on every call — including the repeated /approve/{token} link-prefetches
// mail clients and chat unfurlers are expected to make. A counting remote
// resolver pins this: one Admit followed by several PendingApproval calls
// must resolve the remote exactly once, not once per call.
func TestPendingApproval_CachesRemoteAtAdmitTime(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")
	var calls int
	g.SetRemoteFunc(func(k task.Kinded) string {
		calls++
		return "https://github.com/o/r.git"
	})

	to := fakeCommit("b")
	g.SetCommitFunc(func(k task.Kinded) string { return to })
	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit: armed=%v err=%v", armed, err)
	}
	if calls != 1 {
		t.Fatalf("remote resolver called %d times during Admit, want exactly 1", calls)
	}

	for i := 0; i < 3; i++ {
		hash, cr, ok := g.PendingApproval("repo/deploy")
		if !ok {
			t.Fatal("PendingApproval: ok = false, want true for a pending task")
		}
		if hash == "" {
			t.Error("hash = \"\", want a non-empty observed hash")
		}
		// From == "" on a first-ever approval, so CompareURL is still "" —
		// this test is about call count, not about exercising a populated
		// CompareURL (see TestPendingApproval_PopulatesCompareURL for that).
		if cr.CompareURL != "" {
			t.Errorf("CompareURL = %q, want empty (no prior approval to range from)", cr.CompareURL)
		}
	}
	if calls != 1 {
		t.Errorf("remote resolver called %d times total, want exactly 1 (cached at Admit, never re-walked by PendingApproval)", calls)
	}
}

// TestPendingApproval_ReflectsPriorApproval pins the repeat-pend case: once a
// task has been approved at some commit and later re-pends at a new one, From
// carries the prior approval's commit and To the newly pending one — the
// exact range the operator needs to reason about "what moved".
func TestPendingApproval_ReflectsPriorApproval(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")

	first := fakeCommit("a")
	g.SetCommitFunc(func(k task.Kinded) string { return first })
	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit: armed=%v err=%v", armed, err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if rec, ok := lock.Get("repo/deploy"); !ok || rec.Commit != first {
		t.Fatalf("precondition: lock commit = %+v, want %q", rec, first)
	}

	// Content changes again, re-pending the task at a new commit.
	second := fakeCommit("b")
	g.SetCommitFunc(func(k task.Kinded) string { return second })
	spec2 := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => 1")
	if armed, err := g.Admit(spec2); err != nil || armed {
		t.Fatalf("re-Admit: armed=%v err=%v", armed, err)
	}

	hash, cr, ok := g.PendingApproval("repo/deploy")
	if !ok {
		t.Fatal("PendingApproval: ok = false, want true")
	}
	if wantHash, _ := g.PendingHash("repo/deploy"); hash != wantHash {
		t.Errorf("hash = %q, want the currently pending hash %q", hash, wantHash)
	}
	if cr.From != first {
		t.Errorf("From = %q, want the prior approval's commit %q", cr.From, first)
	}
	if cr.To != second {
		t.Errorf("To = %q, want the newly pending commit %q", cr.To, second)
	}
}

// TestPendingApproval_UnchangedCommitCollapsesToSingleCommit covers a task
// re-pending with the SAME commit it was last approved at — e.g. a
// taskset/dicode.yaml override changed the resolved hash with no new git
// commit. From must collapse to "" rather than repeat To: a
// "Commit range: abc123…abc123" render would misleadingly imply a diff
// exists to review when git shows no change at all (see PendingApproval's
// doc comment and compareURL's own from == to short-circuit, which this
// mirrors at the CommitRange level too).
func TestPendingApproval_UnchangedCommitCollapsesToSingleCommit(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")

	only := fakeCommit("a")
	g.SetCommitFunc(func(k task.Kinded) string { return only })
	if _, err := g.Admit(spec); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if rec, ok := lock.Get("repo/deploy"); !ok || rec.Commit != only {
		t.Fatalf("precondition: lock commit = %+v, want %q", rec, only)
	}

	// Re-pend with a different resolved hash (a different spec body), but
	// the SAME commit — the git history hasn't moved.
	spec2 := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => 1")
	if armed, err := g.Admit(spec2); err != nil || armed {
		t.Fatalf("re-Admit: armed=%v err=%v", armed, err)
	}

	_, cr, ok := g.PendingApproval("repo/deploy")
	if !ok {
		t.Fatal("PendingApproval: ok = false, want true")
	}
	if cr.From != "" {
		t.Errorf("From = %q, want \"\" (nothing moved, so no range) not the repeated commit", cr.From)
	}
	if cr.To != only {
		t.Errorf("To = %q, want %q", cr.To, only)
	}
	if cr.CompareURL != "" {
		t.Errorf("CompareURL = %q, want \"\" when nothing moved", cr.CompareURL)
	}
}

// TestPendingApproval_PopulatesCompareURL wires a fake remote resolver and
// confirms CompareURL is built from it once From/To are both known.
func TestPendingApproval_PopulatesCompareURL(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")
	g.SetRemoteFunc(func(k task.Kinded) string { return "https://github.com/o/r.git" })

	first := fakeCommit("a")
	g.SetCommitFunc(func(k task.Kinded) string { return first })
	if _, err := g.Admit(spec); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if _, ok := lock.Get("repo/deploy"); !ok {
		t.Fatal("precondition: no lock record written")
	}

	second := fakeCommit("b")
	g.SetCommitFunc(func(k task.Kinded) string { return second })
	spec2 := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => 1")
	if _, err := g.Admit(spec2); err != nil {
		t.Fatalf("re-Admit: %v", err)
	}

	_, cr, ok := g.PendingApproval("repo/deploy")
	if !ok {
		t.Fatal("PendingApproval: ok = false, want true")
	}
	want := "https://github.com/o/r/compare/" + first + "..." + second
	if cr.CompareURL != want {
		t.Errorf("CompareURL = %q, want %q", cr.CompareURL, want)
	}
}

// TestPendingApproval_NotPending pins the not-pending case: ok is false and
// the returned hash and CommitRange are their zero values, mirroring
// PendingHash.
func TestPendingApproval_NotPending(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	if hash, cr, ok := g.PendingApproval("repo/ghost"); ok || hash != "" || cr != (CommitRange{}) {
		t.Fatalf("PendingApproval(not pending) = (%q, %+v, %v), want (\"\", zero value, false)", hash, cr, ok)
	}
}
