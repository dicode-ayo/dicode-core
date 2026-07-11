package registry

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dicode/dicode/pkg/db"
)

// ── #569 root_run_id computation ────────────────────────────────────────────

func TestStartRun_RootRunID_ParentlessIsSelfRoot(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)
	ctx := context.Background()

	runID, err := r.StartRun(ctx, "task-a", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	run, err := r.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.RootRunID != runID {
		t.Errorf("RootRunID = %q, want self %q", run.RootRunID, runID)
	}
}

func TestStartRun_RootRunID_ChildInheritsParentRoot(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)
	ctx := context.Background()

	parentID, _ := r.StartRun(ctx, "parent-task", "")
	childID, _ := r.StartRun(ctx, "child-task", parentID)

	child, err := r.GetRun(ctx, childID)
	if err != nil {
		t.Fatalf("GetRun child: %v", err)
	}
	if child.RootRunID != parentID {
		t.Errorf("child RootRunID = %q, want parent %q", child.RootRunID, parentID)
	}
}

func TestStartRun_RootRunID_GrandchildSharesRootWithGrandparent(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)
	ctx := context.Background()

	rootID, _ := r.StartRun(ctx, "root-task", "")
	midID, _ := r.StartRun(ctx, "mid-task", rootID)
	leafID, _ := r.StartRun(ctx, "leaf-task", midID)

	mid, err := r.GetRun(ctx, midID)
	if err != nil {
		t.Fatalf("GetRun mid: %v", err)
	}
	leaf, err := r.GetRun(ctx, leafID)
	if err != nil {
		t.Fatalf("GetRun leaf: %v", err)
	}
	if mid.RootRunID != rootID {
		t.Errorf("mid RootRunID = %q, want %q", mid.RootRunID, rootID)
	}
	if leaf.RootRunID != rootID {
		t.Errorf("leaf RootRunID = %q, want %q (3 levels must share one root)", leaf.RootRunID, rootID)
	}
}

func TestStartRun_RootRunID_MissingParentFallsBackToParentID(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)
	ctx := context.Background()

	// parentRunID references a row that was never created (e.g. a caller
	// passed a stale/foreign ID). StartRunWithID must not error — it falls
	// back to the parent ID itself as the root rather than failing the run.
	childID, err := r.StartRun(ctx, "child-task", "does-not-exist")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	child, err := r.GetRun(ctx, childID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if child.RootRunID != "does-not-exist" {
		t.Errorf("RootRunID = %q, want fallback %q", child.RootRunID, "does-not-exist")
	}
}

// ── ListRunGroup / GetRunGroupLogs ──────────────────────────────────────────

func TestListRunGroup_ReturnsWholeTree(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)
	ctx := context.Background()

	rootID, _ := r.StartRun(ctx, "root-task", "")
	childID, _ := r.StartRun(ctx, "child-task", rootID)
	grandchildID, _ := r.StartRun(ctx, "grandchild-task", childID)
	// Unrelated tree — must not appear.
	otherRoot, _ := r.StartRun(ctx, "other-task", "")

	group, err := r.ListRunGroup(ctx, rootID, 50)
	if err != nil {
		t.Fatalf("ListRunGroup: %v", err)
	}
	if len(group) != 3 {
		t.Fatalf("len(group) = %d, want 3: %+v", len(group), group)
	}
	got := map[string]bool{}
	for _, run := range group {
		got[run.ID] = true
		if run.RootRunID != rootID {
			t.Errorf("member %s RootRunID = %q, want %q", run.ID, run.RootRunID, rootID)
		}
	}
	if !got[rootID] || !got[childID] || !got[grandchildID] {
		t.Errorf("group missing expected members: %+v", got)
	}
	if got[otherRoot] {
		t.Errorf("group leaked unrelated run %s", otherRoot)
	}
}

func TestGetRunGroupLogs_AggregatesAllMembersInOrder(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)
	ctx := context.Background()

	rootID, _ := r.StartRun(ctx, "root-task", "")
	childID, _ := r.StartRun(ctx, "child-task", rootID)

	if err := r.AppendLog(ctx, rootID, "info", "root line 1"); err != nil {
		t.Fatalf("AppendLog: %v", err)
	}
	if err := r.AppendLog(ctx, childID, "info", "child line 1"); err != nil {
		t.Fatalf("AppendLog: %v", err)
	}
	if err := r.AppendLog(ctx, rootID, "info", "root line 2"); err != nil {
		t.Fatalf("AppendLog: %v", err)
	}

	// Unrelated run's logs must not leak into the group.
	otherID, _ := r.StartRun(ctx, "other-task", "")
	if err := r.AppendLog(ctx, otherID, "info", "should not appear"); err != nil {
		t.Fatalf("AppendLog: %v", err)
	}

	logs, err := r.GetRunGroupLogs(ctx, rootID)
	if err != nil {
		t.Fatalf("GetRunGroupLogs: %v", err)
	}
	if len(logs) != 3 {
		t.Fatalf("len(logs) = %d, want 3: %+v", len(logs), logs)
	}
	for _, l := range logs {
		if l.RunID != rootID && l.RunID != childID {
			t.Errorf("unexpected run_id %q in group logs", l.RunID)
		}
	}
	// Insertion order preserved (ts then id tie-break).
	if logs[0].Message != "root line 1" || logs[1].Message != "child line 1" || logs[2].Message != "root line 2" {
		t.Errorf("logs not in insertion order: %+v", logs)
	}
}

// ── backfill (pkg/db migration) ─────────────────────────────────────────────

// TestBackfillRootRunID_OnReopen writes rows directly (simulating a
// pre-#569 database with root_run_id left NULL) and then reopens the DB,
// which re-runs migrate() and its backfill pass. Requires a real file (not
// ":memory:") since a fresh in-memory DB has no persisted rows to reopen.
func TestBackfillRootRunID_OnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backfill.db")

	d, err := db.Open(db.Config{Type: "sqlite", Path: path})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx := context.Background()

	// Insert a 3-level chain the way pre-#569 code would have (no
	// root_run_id column value — StartRunWithID would have set it, so
	// instead write directly to simulate rows that predate the migration).
	if err := d.Exec(ctx,
		`INSERT INTO runs (id, task_id, status, started_at, parent_run_id) VALUES (?, ?, ?, ?, ?)`,
		"root-1", "t", "success", 1000, "",
	); err != nil {
		t.Fatalf("insert root: %v", err)
	}
	if err := d.Exec(ctx,
		`INSERT INTO runs (id, task_id, status, started_at, parent_run_id) VALUES (?, ?, ?, ?, ?)`,
		"mid-1", "t", "success", 2000, "root-1",
	); err != nil {
		t.Fatalf("insert mid: %v", err)
	}
	if err := d.Exec(ctx,
		`INSERT INTO runs (id, task_id, status, started_at, parent_run_id) VALUES (?, ?, ?, ?, ?)`,
		"leaf-1", "t", "success", 3000, "mid-1",
	); err != nil {
		t.Fatalf("insert leaf: %v", err)
	}
	// An orphan: parent_run_id points at a row that doesn't exist.
	if err := d.Exec(ctx,
		`INSERT INTO runs (id, task_id, status, started_at, parent_run_id) VALUES (?, ?, ?, ?, ?)`,
		"orphan-1", "t", "success", 4000, "ghost-parent",
	); err != nil {
		t.Fatalf("insert orphan: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen — migrate() runs again, including backfillRootRunID.
	d2, err := db.Open(db.Config{Type: "sqlite", Path: path})
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer d2.Close()
	r := New(d2)

	for id, want := range map[string]string{
		"root-1":   "root-1",
		"mid-1":    "root-1",
		"leaf-1":   "root-1",
		"orphan-1": "orphan-1", // straggler: self-rooted since its parent row is missing
	} {
		run, err := r.GetRun(ctx, id)
		if err != nil {
			t.Fatalf("GetRun %s: %v", id, err)
		}
		if run.RootRunID != want {
			t.Errorf("%s: RootRunID = %q, want %q", id, run.RootRunID, want)
		}
	}
}
