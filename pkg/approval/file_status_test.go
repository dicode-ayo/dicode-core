package approval

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// commitMore commits every change already made under root — an existing
// repository, previously seeded by commitAll (gate_commit_test.go) — as a
// second, real commit. Named distinctly from commitAll since it opens
// rather than initializes the repository.
func commitMore(t *testing.T, root, msg string) string {
	t.Helper()
	repo, err := gogit.PlainOpen(root)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("add: %v", err)
	}
	h, err := wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if h.IsZero() {
		t.Fatal("fixture produced no commit")
	}
	return h.String()
}

// approveThenReAdmitInRealRepo pends a task inside a real git repository at
// root, approves it (recording a real "from" commit in the lock), then
// mutates the task dir and commits again before re-Admitting — the exact
// re-pend shape the per-file "what moved" markers (#670) are computed
// against, with a real git tree backing every lookup instead of the
// SetCommitFunc fakes gate_commit_test.go otherwise uses for pairing tests.
func approveThenReAdmitInRealRepo(t *testing.T, mutate func(taskDir string)) (*Gate, State) {
	t.Helper()
	root := t.TempDir()
	spec := writeTaskDir(t, root, "repo/deploy", "export default () => 1;")
	commitAll(t, root)

	g, _, _ := newTestGate(t, enabledPolicy())
	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit: armed=%v err=%v", armed, err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	mutate(spec.TaskDir)
	commitMore(t, root, "re-pend")

	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("re-Admit: armed=%v err=%v", armed, err)
	}
	st, err := g.State("repo/deploy")
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	return g, st
}

func fileByPath(t *testing.T, st State, path string) InventoryFile {
	t.Helper()
	for _, f := range st.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("file %q missing from inventory: %+v", path, st.Files)
	return InventoryFile{}
}

// TestStateMarksEditedFileChanged pins the "changed" case: a file edited
// between the approved commit and the newly pending one renders with
// status "changed".
func TestStateMarksEditedFileChanged(t *testing.T) {
	_, st := approveThenReAdmitInRealRepo(t, func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "task.js"), []byte("export default () => 2;"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if got := fileByPath(t, st, "task.js").Status; got != FileStatusChanged {
		t.Errorf("task.js status = %q, want %q", got, FileStatusChanged)
	}
}

// TestStateMarksAddedFileNew pins the "new" case: a file that did not exist
// at the approved commit renders with status "new".
func TestStateMarksAddedFileNew(t *testing.T) {
	_, st := approveThenReAdmitInRealRepo(t, func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "extra.js"), []byte("export const x = 1;\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if got := fileByPath(t, st, "extra.js").Status; got != FileStatusNew {
		t.Errorf("extra.js status = %q, want %q", got, FileStatusNew)
	}
}

// TestStateLeavesUnchangedFileUnmarked pins the "unchanged" case: a file
// identical at both commits carries no status at all (omitempty), not an
// explicit "unchanged" value — it reads identically to "could not be
// determined" and neither is ever asserted when it isn't true (ADR-0001).
func TestStateLeavesUnchangedFileUnmarked(t *testing.T) {
	_, st := approveThenReAdmitInRealRepo(t, func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "extra.js"), []byte("export const x = 1;\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	// task.yaml is untouched by mutate in every case above.
	if got := fileByPath(t, st, "task.yaml").Status; got != "" {
		t.Errorf("task.yaml (unchanged) status = %q, want empty", got)
	}
}

// TestStateNoMarkersWithoutPriorApproval pins the "no from" case: a task
// pending for the first time inside a real repository has a commit to
// stamp as To, but no lock record to read From from — markers must not be
// computed at all, and that must not be reported as a failure.
func TestStateNoMarkersWithoutPriorApproval(t *testing.T) {
	root := t.TempDir()
	spec := writeTaskDir(t, root, "repo/fresh", "export default () => {}")
	commitAll(t, root)

	g, _, _ := newTestGate(t, enabledPolicy())
	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit: armed=%v err=%v", armed, err)
	}

	st, err := g.State("repo/fresh")
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if len(st.Files) == 0 {
		t.Fatal("first-ever pend must still inventory its files")
	}
	for _, f := range st.Files {
		if f.Status != "" {
			t.Errorf("%s: status = %q, want empty (no prior approval to diff against)", f.Path, f.Status)
		}
	}
	if st.FilesError != "" {
		t.Errorf("FilesError = %q, want empty", st.FilesError)
	}
}

// TestStateNoMarkersOutsideRepository pins the non-git-source case: a task
// dir with no repository at all resolves neither From nor To (headCommitOf
// degrades both to ""), so markers are optional decoration this surface
// simply omits — never a reported failure of the (still complete) file
// listing itself.
func TestStateNoMarkersOutsideRepository(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "export default () => 1;")

	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit: armed=%v err=%v", armed, err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if rec, ok := lock.Get("repo/deploy"); !ok || rec.Commit != "" {
		t.Fatalf("precondition: want an approval record with an empty commit, got %+v ok=%v", rec, ok)
	}

	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte("export default () => 2;"), 0o644); err != nil {
		t.Fatal(err)
	}
	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("re-Admit: armed=%v err=%v", armed, err)
	}

	st, err := g.State("repo/deploy")
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if len(st.Files) == 0 {
		t.Fatal("file listing must still be complete outside a repository")
	}
	for _, f := range st.Files {
		if f.Status != "" {
			t.Errorf("%s: status = %q, want empty outside a repository", f.Path, f.Status)
		}
	}
	if st.FilesError != "" {
		t.Errorf("FilesError = %q, want empty — markers are optional decoration, not a required capability", st.FilesError)
	}
}

// TestCurrentStateNeverComputesMarkers pins that CurrentState (an armed
// task's current-state render, #714) never computes markers even when a
// real prior-approval commit exists to diff against: the epic confines this
// decoration to the pending review, not general task inspection.
func TestCurrentStateNeverComputesMarkers(t *testing.T) {
	root := t.TempDir()
	spec := writeTaskDir(t, root, "repo/deploy", "export default () => 1;")
	commitAll(t, root)

	g, _, _ := newTestGate(t, enabledPolicy())
	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit: armed=%v err=%v", armed, err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte("export default () => 2;"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitMore(t, root, "edit after approve")

	// The task is armed (approved), not pending, so CurrentState is the
	// only render available — even though a real commit-range exists.
	st := g.CurrentState("repo/deploy", spec)
	if len(st.Files) == 0 {
		t.Fatal("CurrentState must still inventory files")
	}
	for _, f := range st.Files {
		if f.Status != "" {
			t.Errorf("%s: status = %q, want empty — CurrentState must never compute markers", f.Path, f.Status)
		}
	}
}
