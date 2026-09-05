package approval

import (
	"errors"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

// TestBuiltinUnpinnedIgnoresCommitDrift pins today's behavior for the
// default (unpinned) buildin source: a moving branch is expected to move,
// so a changed commit — even paired with a changed hash — must still
// auto-arm without ever pending. This is the regression guard that #832's
// fix must not disturb.
func TestBuiltinUnpinnedIgnoresCommitDrift(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "buildin/mcp", "export default () => {}")

	g.SetCommitFunc(func(task.Kinded) string { return fakeCommit("a") })
	if armed, err := g.Admit(spec); err != nil || !armed {
		t.Fatalf("first admit: armed=%v err=%v", armed, err)
	}

	// Commit moves (a fresh git pull on the tracked branch) with content
	// unchanged from this test's point of view.
	g.SetCommitFunc(func(task.Kinded) string { return fakeCommit("b") })
	armed, err := g.Admit(spec)
	if err != nil || !armed {
		t.Fatalf("unpinned buildin must still auto-arm on commit drift: armed=%v err=%v", armed, err)
	}
	if g.IsPending("buildin/mcp") {
		t.Fatal("unpinned buildin must never pend on commit drift")
	}
	if rec, ok := lock.Get("buildin/mcp"); !ok || rec.ApprovedBy != ApprovedByBuiltin {
		t.Fatalf("record = %+v ok=%v, want ApprovedByBuiltin", rec, ok)
	}
}

// TestBuiltinPinnedFirstSeenSeedsBaseline covers the "existing installs
// don't strand their whole buildin inventory pending on upgrade" half of
// #832's acceptance: the very first time a pinned-buildin task is admitted
// (fresh install, or upgrading onto this check with no commit on record
// yet), it must auto-arm and seed its commit as the baseline rather than
// pend.
func TestBuiltinPinnedFirstSeenSeedsBaseline(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	g.SetBuiltinPinned(true)
	spec := writeTaskDir(t, t.TempDir(), "buildin/mcp", "export default () => {}")

	baseline := fakeCommit("a")
	g.SetCommitFunc(func(task.Kinded) string { return baseline })

	armed, err := g.Admit(spec)
	if err != nil || !armed {
		t.Fatalf("first admit of a pinned buildin task must auto-arm: armed=%v err=%v", armed, err)
	}
	rec, ok := lock.Get("buildin/mcp")
	if !ok || rec.Commit != baseline {
		t.Fatalf("record = %+v ok=%v, want baseline commit %q seeded", rec, ok, baseline)
	}
}

// TestBuiltinPinnedCommitUnchangedAutoApproves covers the steady state: a
// pinned buildin task whose resolved commit still matches the one on record
// keeps auto-arming, exactly like an unpinned one.
func TestBuiltinPinnedCommitUnchangedAutoApproves(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	g.SetBuiltinPinned(true)
	spec := writeTaskDir(t, t.TempDir(), "buildin/mcp", "export default () => {}")

	same := fakeCommit("a")
	g.SetCommitFunc(func(task.Kinded) string { return same })

	if armed, err := g.Admit(spec); err != nil || !armed {
		t.Fatalf("seed admit: armed=%v err=%v", armed, err)
	}
	armed, err := g.Admit(spec)
	if err != nil || !armed {
		t.Fatalf("unchanged commit must keep auto-arming: armed=%v err=%v", armed, err)
	}
	if rec, ok := lock.Get("buildin/mcp"); !ok || rec.Commit != same {
		t.Fatalf("record = %+v ok=%v, want unchanged commit %q", rec, ok, same)
	}
}

// TestBuiltinPinnedCommitDriftHoldsPending is the end-to-end regression for
// #832: once a pinned buildin task has a baseline commit on record, a
// re-cut tag (a resolved commit that no longer matches the baseline) must
// hold the task pending — exactly like any other source's content-hash
// gate — instead of silently re-arming on new content.
func TestBuiltinPinnedCommitDriftHoldsPending(t *testing.T) {
	g, arm, lock := newTestGate(t, enabledPolicy())
	g.SetBuiltinPinned(true)
	spec := writeTaskDir(t, t.TempDir(), "buildin/mcp", "export default () => {}")

	baseline := fakeCommit("a")
	g.SetCommitFunc(func(task.Kinded) string { return baseline })
	if armed, err := g.Admit(spec); err != nil || !armed {
		t.Fatalf("seed admit: armed=%v err=%v", armed, err)
	}

	// A re-cut tag moves both the commit and the content it points at — a
	// same-content re-cut (commit changes, hash doesn't) has nothing for an
	// operator to review and is intentionally left to the existing
	// lock.Approved(id, hash) short-circuit; this is the realistic case the
	// gate exists to hold.
	recutSpec := writeTaskDir(t, t.TempDir(), "buildin/mcp", "export default () => { return 2; }")
	recut := fakeCommit("b")
	g.SetCommitFunc(func(task.Kinded) string { return recut })
	armed, err := g.Admit(recutSpec)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if armed {
		t.Fatal("a re-cut pinned tag must hold the task pending, not auto-arm")
	}
	if !g.IsPending("buildin/mcp") {
		t.Fatal("task must be pending after commit drift")
	}
	// The lock record must still reflect the old baseline until approved —
	// the pending set, not the lock, tracks the newly-observed generation.
	if rec, ok := lock.Get("buildin/mcp"); !ok || rec.Commit != baseline {
		t.Fatalf("record = %+v ok=%v, want the old baseline %q preserved until approval", rec, ok, baseline)
	}

	if err := g.Approve("buildin/mcp"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if rec, ok := lock.Get("buildin/mcp"); !ok || rec.Commit != recut {
		t.Fatalf("after approval record = %+v ok=%v, want the re-cut commit %q", rec, ok, recut)
	}
	if got := arm.armedIDs(); len(got) != 2 {
		// One arm from the seed admit, one from the explicit approval.
		t.Fatalf("armedIDs = %v, want 2 (seed + approve)", got)
	}
}

// TestBuiltinPinnedNoCommitInfoKeepsBypass covers a pinned buildin whose
// task directory resolves no commit at all (e.g. a local override outside
// any repository) — there is nothing to compare against, so it must keep
// the plain auto-approve bypass rather than get permanently stuck unable to
// ever establish a baseline.
func TestBuiltinPinnedNoCommitInfoKeepsBypass(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	g.SetBuiltinPinned(true)
	spec := writeTaskDir(t, t.TempDir(), "buildin/mcp", "export default () => {}")
	g.SetCommitFunc(func(task.Kinded) string { return "" })

	armed, err := g.Admit(spec)
	if err != nil || !armed {
		t.Fatalf("no resolvable commit must still auto-arm: armed=%v err=%v", armed, err)
	}
	if rec, ok := lock.Get("buildin/mcp"); !ok || rec.Commit != "" {
		t.Fatalf("record = %+v ok=%v, want no commit recorded", rec, ok)
	}
}

// TestBuiltinPinnedBackfillsMissingCommitOnUnchangedHash covers an existing
// lock record that predates commit tracking (or predates the operator
// pinning buildin to a tag): its hash is already approved and stays
// unchanged, so the ordinary Record path would never touch it, yet
// builtinPinnedDrift needs a baseline to ever engage. The gate must
// backfill the commit in place on the very next admit.
func TestBuiltinPinnedBackfillsMissingCommitOnUnchangedHash(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "buildin/mcp", "export default () => {}")

	// Seed a record the old way: unpinned, so Record's auto-approve path
	// never resolves a commit (mirrors an install whose lock predates #832,
	// or one where buildin was only pinned to a tag afterwards).
	if armed, err := g.Admit(spec); err != nil || !armed {
		t.Fatalf("seed admit: armed=%v err=%v", armed, err)
	}
	if rec, ok := lock.Get("buildin/mcp"); !ok || rec.Commit != "" {
		t.Fatalf("precondition: record = %+v ok=%v, want no commit yet", rec, ok)
	}

	// Now the operator pins buildin to a tag, and the resolver can compute a
	// commit for this task's directory going forward.
	g.SetBuiltinPinned(true)
	current := fakeCommit("a")
	g.SetCommitFunc(func(task.Kinded) string { return current })

	armed, err := g.Admit(spec)
	if err != nil || !armed {
		t.Fatalf("backfill admit: armed=%v err=%v", armed, err)
	}
	if rec, ok := lock.Get("buildin/mcp"); !ok || rec.Commit != current {
		t.Fatalf("record = %+v ok=%v, want backfilled commit %q", rec, ok, current)
	}

	// The backfilled baseline must now actually gate a subsequent re-cut
	// (changed content and commit together — see the realistic-case note in
	// TestBuiltinPinnedCommitDriftHoldsPending).
	recutSpec := writeTaskDir(t, t.TempDir(), "buildin/mcp", "export default () => { return 2; }")
	recut := fakeCommit("b")
	g.SetCommitFunc(func(task.Kinded) string { return recut })
	armed, err = g.Admit(recutSpec)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if armed {
		t.Fatal("backfilled baseline must gate a subsequent commit drift")
	}
}

// TestFireGuardVetoesPinnedBuiltinCommitDrift is the FireGuard counterpart
// of TestBuiltinPinnedCommitDriftHoldsPending, for the reconcile-window race
// FireGuard's own doc comment describes: a git source updates its working
// tree, then walks its tasks admitting each in turn — so a buildin task's
// resolved commit can already have moved on disk before the reconciler's
// loop reaches this particular task's own Admit call. Until that happens,
// IsPending stays false, and trusted() would otherwise wave a builtin task
// through unconditionally regardless of the drift. FireGuard must veto it
// live off the commit function, exactly as it already re-hashes a
// non-trusted source's live content in TestFireGuardVetoesEditedApprovedTask.
func TestFireGuardVetoesPinnedBuiltinCommitDrift(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	g.SetBuiltinPinned(true)
	spec := writeTaskDir(t, t.TempDir(), "buildin/mcp", "export default () => {}")

	baseline := fakeCommit("a")
	g.SetCommitFunc(func(task.Kinded) string { return baseline })
	if armed, err := g.Admit(spec); err != nil || !armed {
		t.Fatalf("seed admit: armed=%v err=%v", armed, err)
	}
	if err := g.FireGuard("buildin/mcp"); err != nil {
		t.Fatalf("FireGuard on freshly seeded task: %v", err)
	}

	// The commit moves (the tag was re-cut) with no Admit in between —
	// exactly the reconcile-window gap: IsPending is still false here.
	g.SetCommitFunc(func(task.Kinded) string { return fakeCommit("b") })
	if g.IsPending("buildin/mcp") {
		t.Fatal("precondition: task must not yet be re-pended by the reconciler")
	}

	if err := g.FireGuard("buildin/mcp"); !errors.Is(err, ErrPending) {
		t.Fatalf("FireGuard after commit drift = %v, want ErrPending (drifted content must not run)", err)
	}
	if rec, _ := lock.Get("buildin/mcp"); rec.Commit != baseline {
		t.Fatalf("lock commit mutated by a live FireGuard check: %q -> %q", baseline, rec.Commit)
	}
}

// TestFireGuardAllowsUnpinnedBuiltinCommitDrift is the regression guard
// alongside the above: an unpinned buildin task (the default) must keep
// firing through a commit change exactly as before, since FireGuard's new
// live drift check is a no-op when builtinPinned is false.
func TestFireGuardAllowsUnpinnedBuiltinCommitDrift(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "buildin/mcp", "export default () => {}")

	g.SetCommitFunc(func(task.Kinded) string { return fakeCommit("a") })
	if armed, err := g.Admit(spec); err != nil || !armed {
		t.Fatalf("seed admit: armed=%v err=%v", armed, err)
	}
	g.SetCommitFunc(func(task.Kinded) string { return fakeCommit("b") })
	if err := g.FireGuard("buildin/mcp"); err != nil {
		t.Fatalf("unpinned buildin must still fire through a commit change: %v", err)
	}
}

// TestBuiltinPinnedTrustedSourceOverridesDrift covers precedence: an
// operator who has explicitly declared approval.sources.buildin.trust:
// always still gets that trust honored even while a pinned buildin's
// commit has drifted — an explicit trust declaration outranks the
// automatic drift hold.
func TestBuiltinPinnedTrustedSourceOverridesDrift(t *testing.T) {
	policy := enabledPolicy()
	policy.TrustedSources[BuiltinSource] = true
	g, _, lock := newTestGate(t, policy)
	g.SetBuiltinPinned(true)
	spec := writeTaskDir(t, t.TempDir(), "buildin/mcp", "export default () => {}")

	g.SetCommitFunc(func(task.Kinded) string { return fakeCommit("a") })
	if armed, err := g.Admit(spec); err != nil || !armed {
		t.Fatalf("seed admit: armed=%v err=%v", armed, err)
	}

	g.SetCommitFunc(func(task.Kinded) string { return fakeCommit("b") })
	armed, err := g.Admit(spec)
	if err != nil || !armed {
		t.Fatalf("an explicitly trusted source must still auto-arm despite drift: armed=%v err=%v", armed, err)
	}
	if g.IsPending("buildin/mcp") {
		t.Fatal("an explicitly trusted source must never pend, drift or not")
	}
	// The hash never changed (only the commit did), so Record's no-op guard
	// means the seed admit's record — and its ApprovedByBuiltin stamp —
	// stands untouched; the trusted-source path is what let this admit
	// re-arm despite the drift, not a fresh record.
	if rec, ok := lock.Get("buildin/mcp"); !ok || rec.ApprovedBy != ApprovedByBuiltin {
		t.Fatalf("record = %+v ok=%v, want the original ApprovedByBuiltin record untouched", rec, ok)
	}
}
