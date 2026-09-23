package approval

import (
	"errors"
	"testing"
)

// ── Commit-count decoration ──────────────────────────────────────────────────
//
// PendingApproval/PendingSnapshot's CommitRange.Commits/CommitsBounded are
// resolved via g.commitCountFn, capped and cached the same way the per-file
// "what moved" markers are (fileStatusCache) — see commitCountCache's doc
// comment.

// TestPendingApproval_CommitsResolvedViaCommitCountFn pins that the range's
// Commits/CommitsBounded come from whatever g.commitCountFn reports, called
// with the pending generation's own From/To and the shared cap.
func TestPendingApproval_CommitsResolvedViaCommitCountFn(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	first, second := fakeCommit("a"), fakeCommit("b")
	approveThenRepend(t, g, lock, first, second, "")

	var gotFrom, gotTo string
	var gotLimit int
	g.SetCommitCountFunc(func(dir, from, to string, limit int) (int, bool, error) {
		gotFrom, gotTo, gotLimit = from, to, limit
		return 7, false, nil
	})

	_, cr, ok := g.PendingApproval("repo/deploy")
	if !ok {
		t.Fatal("PendingApproval: ok = false, want true")
	}
	if gotFrom != first || gotTo != second {
		t.Errorf("commitCountFn called with %q...%q, want %q...%q", gotFrom, gotTo, first, second)
	}
	if gotLimit != commitCountLimit {
		t.Errorf("commitCountFn called with limit %d, want %d", gotLimit, commitCountLimit)
	}
	if cr.Commits != 7 || cr.CommitsBounded {
		t.Errorf("CommitRange = %+v, want Commits=7 CommitsBounded=false", cr)
	}
}

// TestPendingApproval_CommitsBoundedPropagates pins that a bounded ("at
// least this many") result reaches the rendered range unchanged.
func TestPendingApproval_CommitsBoundedPropagates(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	first, second := fakeCommit("a"), fakeCommit("b")
	approveThenRepend(t, g, lock, first, second, "")

	g.SetCommitCountFunc(func(dir, from, to string, limit int) (int, bool, error) {
		return commitCountLimit, true, nil
	})

	_, cr, ok := g.PendingApproval("repo/deploy")
	if !ok {
		t.Fatal("PendingApproval: ok = false, want true")
	}
	if cr.Commits != commitCountLimit || !cr.CommitsBounded {
		t.Errorf("CommitRange = %+v, want Commits=%d CommitsBounded=true", cr, commitCountLimit)
	}
}

// TestPendingApproval_CommitsDegradeOnError pins ADR-0001 for this
// decoration: a walk failure (unreachable objects, a rewritten history)
// must never render a number, only its absence.
func TestPendingApproval_CommitsDegradeOnError(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	first, second := fakeCommit("a"), fakeCommit("b")
	approveThenRepend(t, g, lock, first, second, "")

	g.SetCommitCountFunc(func(dir, from, to string, limit int) (int, bool, error) {
		return 0, false, errors.New("simulated: rewritten history")
	})

	_, cr, ok := g.PendingApproval("repo/deploy")
	if !ok {
		t.Fatal("PendingApproval: ok = false, want true")
	}
	if cr.Commits != -1 || cr.CommitsBounded {
		t.Errorf("CommitRange = %+v, want Commits=-1 CommitsBounded=false on a walk failure", cr)
	}
}

// TestPendingApproval_CommitsUnknownWithNoBaseline pins that a first-ever
// pend (no prior approval, so From is "") never calls the resolver at all —
// there is no range to walk — and reports Commits=-1.
func TestPendingApproval_CommitsUnknownWithNoBaseline(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => {}")

	called := false
	g.SetCommitCountFunc(func(dir, from, to string, limit int) (int, bool, error) {
		called = true
		return 1, false, nil
	})
	// writeTaskDir's directory has no .git, so the default commitFn
	// (headCommitOf) resolves no commit at all — exactly the "no baseline"
	// case this test exercises, with no override needed.

	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit: armed=%v err=%v", armed, err)
	}
	_, cr, ok := g.PendingApproval("repo/deploy")
	if !ok {
		t.Fatal("PendingApproval: ok = false, want true")
	}
	if called {
		t.Error("commitCountFn was called with no baseline commit — nothing to walk")
	}
	if cr.Commits != -1 || cr.CommitsBounded {
		t.Errorf("CommitRange = %+v, want Commits=-1 CommitsBounded=false", cr)
	}
}

// TestPendingApproval_CommitsWalkedOncePerGeneration pins the same
// prefetch-safety guarantee TestPendingApproval_ResolvesGitOncePerAdmit pins
// for the commit/remote resolver: /approve/{token} is prefetched by mail
// clients and chat unfurlers, so repeated renders of one pending generation
// must cost at most one commit-count walk.
func TestPendingApproval_CommitsWalkedOncePerGeneration(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	first, second := fakeCommit("a"), fakeCommit("b")
	approveThenRepend(t, g, lock, first, second, "")

	var calls int
	g.SetCommitCountFunc(func(dir, from, to string, limit int) (int, bool, error) {
		calls++
		return 3, false, nil
	})

	for i := 0; i < 3; i++ {
		if _, _, ok := g.PendingApproval("repo/deploy"); !ok {
			t.Fatal("PendingApproval: ok = false, want true")
		}
	}
	if calls != 1 {
		t.Errorf("commitCountFn called %d times, want exactly 1 — PendingApproval must never re-walk", calls)
	}
}

// TestPendingSnapshot_NeverComputesCommitCount pins the cost split between
// PendingSnapshot and PendingApproval: PendingSnapshot backs daemon.go's
// "dicode list" / "dicode task pending" status query and MintApproveLink,
// neither of which reads CommitRange.Commits, so it must never trigger the
// git walk — only PendingApproval, the /approve/{token} confirm page's own
// accessor, does.
func TestPendingSnapshot_NeverComputesCommitCount(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	first, second := fakeCommit("a"), fakeCommit("b")
	approveThenRepend(t, g, lock, first, second, "")

	called := false
	g.SetCommitCountFunc(func(dir, from, to string, limit int) (int, bool, error) {
		called = true
		return 3, false, nil
	})

	v, ok := g.PendingSnapshot("repo/deploy")
	if !ok {
		t.Fatal("PendingSnapshot: ok = false, want true")
	}
	if called {
		t.Error("PendingSnapshot triggered a commit-count walk — only PendingApproval should")
	}
	if v.CommitRange.Commits != -1 || v.CommitRange.CommitsBounded {
		t.Errorf("CommitRange = %+v, want Commits=-1 CommitsBounded=false", v.CommitRange)
	}
	// From/To/CompareURL are cheap, in-memory decoration and must still be
	// there — only Commits is deferred to PendingApproval.
	if v.CommitRange.From != first || v.CommitRange.To != second {
		t.Errorf("CommitRange = %+v, want From=%q To=%q", v.CommitRange, first, second)
	}
}
