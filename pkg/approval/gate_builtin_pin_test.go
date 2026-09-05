package approval

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestBuiltinUnpinnedBypassesHashGate pins today's behavior for the default
// (unpinned) buildin source: any content edit still auto-arms without ever
// pending. This is the regression guard that #832's fix must not disturb —
// a moving branch is expected to move.
func TestBuiltinUnpinnedBypassesHashGate(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	root := t.TempDir()
	spec := writeTaskDir(t, root, "buildin/mcp", "export default () => {}")

	if armed, err := g.Admit(spec); err != nil || !armed {
		t.Fatalf("first admit: armed=%v err=%v", armed, err)
	}
	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte("export default () => 2"), 0o644); err != nil {
		t.Fatal(err)
	}
	armed, err := g.Admit(spec)
	if err != nil || !armed {
		t.Fatalf("unpinned buildin must still auto-arm after an edit: armed=%v err=%v", armed, err)
	}
	if g.IsPending("buildin/mcp") {
		t.Fatal("unpinned buildin must never pend")
	}
	if rec, ok := lock.Get("buildin/mcp"); !ok || rec.ApprovedBy != ApprovedByBuiltin {
		t.Fatalf("record = %+v ok=%v, want ApprovedByBuiltin", rec, ok)
	}
}

// TestBuiltinPinnedContentChangeHoldsPending is the end-to-end regression for
// #832: once buildin is pinned to a tag, it goes through the same
// content-hash gate as any other source — a re-cut tag that changes content
// pends the task instead of silently re-arming it.
func TestBuiltinPinnedContentChangeHoldsPending(t *testing.T) {
	g, arm, lock := newTestGate(t, enabledPolicy())
	g.SetBuiltinPinned(true)
	root := t.TempDir()
	spec := writeTaskDir(t, root, "buildin/mcp", "export default () => {}")

	armed, err := g.Admit(spec)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if armed {
		t.Fatal("a pinned buildin task with no prior approval must pend, exactly like any other source")
	}
	if !g.IsPending("buildin/mcp") {
		t.Fatal("task must be pending")
	}

	if err := g.Approve("buildin/mcp"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	approved, _ := lock.Get("buildin/mcp")
	if approved.ApprovedBy != ApprovedByManual {
		t.Fatalf("ApprovedBy = %q, want manual", approved.ApprovedBy)
	}

	// The tag is re-cut: content changes.
	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte("export default () => 2"), 0o644); err != nil {
		t.Fatal(err)
	}
	armed, err = g.Admit(spec)
	if err != nil {
		t.Fatalf("Admit after re-cut: %v", err)
	}
	if armed {
		t.Fatal("a re-cut pinned tag must hold the task pending, not auto-arm")
	}
	if !g.IsPending("buildin/mcp") {
		t.Fatal("task must be pending after the content change")
	}
	if rec, ok := lock.Get("buildin/mcp"); !ok || rec.Hash != approved.Hash {
		t.Fatalf("record = %+v ok=%v, want the old approved hash preserved until re-approval", rec, ok)
	}
	if got := arm.armedIDs(); len(got) != 1 {
		t.Fatalf("armedIDs = %v, want 1 (the manual approval only — the re-cut must not arm)", got)
	}
}

// TestBuiltinPinnedUnchangedContentKeepsArming covers the steady state: a
// pinned buildin task whose content hasn't changed keeps auto-arming, same
// as an unpinned one.
func TestBuiltinPinnedUnchangedContentKeepsArming(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "buildin/mcp", "export default () => {}")

	// Admitted once while unpinned — mirrors how an existing lock record's
	// hash is already tracking buildin's live content by the time an
	// operator pins it.
	if armed, err := g.Admit(spec); err != nil || !armed {
		t.Fatalf("seed admit: armed=%v err=%v", armed, err)
	}

	g.SetBuiltinPinned(true)
	armed, err := g.Admit(spec)
	if err != nil || !armed {
		t.Fatalf("unchanged content must keep auto-arming once pinned: armed=%v err=%v", armed, err)
	}
	if g.IsPending("buildin/mcp") {
		t.Fatal("unchanged content must not pend")
	}
}

// TestBuiltinPinnedUpgradeDoesNotPendExistingInventory covers the other half
// of #832's acceptance: an existing install's buildin lock records already
// track their live content hash exactly (Record runs on every admit whose
// hash changes, builtin bypass or not) — pinning buildin and restarting onto
// this check must not spuriously pend an unchanged inventory.
func TestBuiltinPinnedUpgradeDoesNotPendExistingInventory(t *testing.T) {
	root := t.TempDir()
	spec := writeTaskDir(t, root, "buildin/mcp", "export default () => {}")

	// Simulates every admit cycle before the operator ever pins buildin or
	// upgrades onto this fix: an unpinned gate, seen many times.
	seed, _, lock := newTestGate(t, enabledPolicy())
	for i := 0; i < 3; i++ {
		if armed, err := seed.Admit(spec); err != nil || !armed {
			t.Fatalf("seed admit %d: armed=%v err=%v", i, armed, err)
		}
	}
	preExisting, ok := lock.Get("buildin/mcp")
	if !ok {
		t.Fatal("precondition: expected a pre-existing record")
	}

	// A fresh Gate over the same lock, as a daemon restart onto a pinned
	// config would construct.
	g2 := NewGate(enabledPolicy(), lock, (&fakeArm{}).arm, nil)
	g2.SetBuiltinPinned(true)
	armed, err := g2.Admit(spec)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if !armed {
		t.Fatal("upgrading onto a pinned buildin must not pend an inventory whose content never changed")
	}
	if after, _ := lock.Get("buildin/mcp"); after.Hash != preExisting.Hash {
		t.Fatalf("hash changed across the restart: %q -> %q", preExisting.Hash, after.Hash)
	}
}

// TestBuiltinPinnedTrustedSourceOverridesHashGate covers precedence: an
// operator who has explicitly declared approval.sources.buildin.trust:
// always still gets that trust honored even once buildin is pinned and its
// content changes — an explicit trust declaration outranks the pinned
// content-hash gate, exactly as it does for any other source.
func TestBuiltinPinnedTrustedSourceOverridesHashGate(t *testing.T) {
	policy := enabledPolicy()
	policy.TrustedSources[BuiltinSource] = true
	g, _, lock := newTestGate(t, policy)
	g.SetBuiltinPinned(true)
	spec := writeTaskDir(t, t.TempDir(), "buildin/mcp", "export default () => {}")

	armed, err := g.Admit(spec)
	if err != nil || !armed {
		t.Fatalf("an explicitly trusted source must auto-arm even when pinned: armed=%v err=%v", armed, err)
	}
	if rec, ok := lock.Get("buildin/mcp"); !ok || rec.ApprovedBy != ApprovedByTrustedSource {
		t.Fatalf("record = %+v ok=%v, want ApprovedByTrustedSource", rec, ok)
	}
}

// TestFireGuardPinnedBuiltinVetoesEditedContent is the FireGuard counterpart
// of TestFireGuardVetoesEditedApprovedTask: once buildin is pinned, trusted()
// no longer exempts it, so FireGuard's live re-hash closes the same
// reconcile-window race for a pinned buildin task that it already closes for
// any other gated source — an edit landing between reconcile cycles must not
// run under a stale approval.
func TestFireGuardPinnedBuiltinVetoesEditedContent(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	g.SetBuiltinPinned(true)
	spec := writeTaskDir(t, t.TempDir(), "buildin/mcp", "export default () => 1")

	if armed, err := g.Admit(spec); err != nil {
		t.Fatalf("Admit: %v", err)
	} else if armed {
		t.Fatal("precondition: first admit of a pinned buildin task must pend")
	}
	if err := g.Approve("buildin/mcp"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := g.FireGuard("buildin/mcp"); err != nil {
		t.Fatalf("FireGuard on freshly approved task: %v", err)
	}

	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte("export default () => evil()"), 0o644); err != nil {
		t.Fatal(err)
	}
	if g.IsPending("buildin/mcp") {
		t.Fatal("precondition: task must not yet be re-pended by the reconciler")
	}

	if err := g.FireGuard("buildin/mcp"); !errors.Is(err, ErrPending) {
		t.Fatalf("FireGuard after edit = %v, want ErrPending (changed content must not run)", err)
	}
	approved, _ := lock.Get("buildin/mcp")
	if rec, _ := lock.Get("buildin/mcp"); rec.Hash != approved.Hash {
		t.Fatal("lock hash must not be mutated by a live FireGuard check")
	}
}

// TestFireGuardUnpinnedBuiltinAllowsEditedContent is the regression guard
// alongside the above: an unpinned buildin task (the default) must keep
// firing through an edit exactly as before, since trusted() still exempts
// it from FireGuard's re-hash when builtinPinned is false.
func TestFireGuardUnpinnedBuiltinAllowsEditedContent(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "buildin/mcp", "export default () => 1")

	if armed, err := g.Admit(spec); err != nil || !armed {
		t.Fatalf("Admit: armed=%v err=%v", armed, err)
	}
	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte("export default () => 2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.FireGuard("buildin/mcp"); err != nil {
		t.Fatalf("unpinned buildin must still fire through an edit: %v", err)
	}
}
