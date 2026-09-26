package approval

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

// ── Commit-range atomicity ───────────────────────────────────────────────────
//
// The range a renderer reports must describe exactly one pending generation:
// the baseline is captured on the pending entry when the generation is held,
// so an approval or eviction landing afterwards changes what the NEXT pend
// renders, never what a render of the generation already held reports.

// TestPendingApproval_BaselineSurvivesLaterApproval pins the baseline against
// an approval recorded for a different generation while this one is pending:
// From must still be the commit on record when this generation was held, not
// whatever the lock holds by the time the entry is rendered.
func TestPendingApproval_BaselineSurvivesLaterApproval(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	first, second := fakeCommit("a"), fakeCommit("b")
	approveThenRepend(t, g, lock, first, second, "https://github.com/o/r.git")

	_, before, ok := g.PendingApproval("repo/deploy")
	if !ok {
		t.Fatal("PendingApproval: ok = false, want true")
	}
	if before.From != first {
		t.Fatalf("precondition: From = %q, want the prior approval's commit %q", before.From, first)
	}

	if err := lock.Record("repo/deploy", "another-generation", ApprovedByManual, fakeCommit("c")); err != nil {
		t.Fatalf("Record: %v", err)
	}

	_, after, ok := g.PendingApproval("repo/deploy")
	if !ok {
		t.Fatal("PendingApproval after the record: ok = false, want true")
	}
	if after != before {
		t.Errorf("CommitRange = %+v, want it unchanged at %+v", after, before)
	}
}

// TestPendingApproval_BaselineSurvivesLockEviction covers the other
// direction: a record removed out from under a pending generation must not
// blank out a range that was resolved when the generation was held.
func TestPendingApproval_BaselineSurvivesLockEviction(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	first, second := fakeCommit("a"), fakeCommit("b")
	approveThenRepend(t, g, lock, first, second, "")

	if err := lock.Remove("repo/deploy"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	_, cr, ok := g.PendingApproval("repo/deploy")
	if !ok {
		t.Fatal("PendingApproval: ok = false, want true")
	}
	if cr.From != first || cr.To != second {
		t.Errorf("CommitRange = %+v, want From=%q To=%q", cr, first, second)
	}
}

// TestStateMarkersUseTheGenerationsBaseline pins the same rule on the
// per-file "what moved" markers, the other consumer of the range: both
// renderers must diff against the commit this generation pended from, not a
// commit recorded afterwards.
func TestStateMarkersUseTheGenerationsBaseline(t *testing.T) {
	first, second := fakeCommit("a"), fakeCommit("b")
	cases := []struct {
		name   string
		render func(t *testing.T, g *Gate, spec *task.Spec)
	}{
		{"State", func(t *testing.T, g *Gate, _ *task.Spec) {
			if _, err := g.State("repo/deploy"); err != nil {
				t.Fatalf("State: %v", err)
			}
		}},
		{"StateFor", func(t *testing.T, g *Gate, spec *task.Spec) {
			g.StateFor("repo/deploy", spec)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, _, lock := newTestGate(t, enabledPolicy())
			spec := approveThenRepend(t, g, lock, first, second, "")

			var gotFrom, gotTo string
			g.SetTreeDiffFunc(func(dir, from, to string, absPaths []string) (map[string]string, map[string]string, error) {
				gotFrom, gotTo = from, to
				return nil, nil, nil
			})
			if err := lock.Record("repo/deploy", "another-generation", ApprovedByManual, fakeCommit("c")); err != nil {
				t.Fatalf("Record: %v", err)
			}

			tc.render(t, g, spec)
			if gotFrom != first || gotTo != second {
				t.Errorf("markers computed over %q...%q, want %q...%q", gotFrom, gotTo, first, second)
			}
		})
	}
}

// TestCommitRangeNeverCrossesGenerations races Approve and Forget against all
// three renderers, under the race detector as CI runs them.
//
// Every Admit stamps a unique hash and a unique, monotonically numbered
// commit, so a hash names exactly one pending generation and a commit number
// orders them. Two renders of the same hash reporting different From values —
// or per-file markers whose baseline is not strictly older than the commit
// being rendered — both mean a renderer paired one generation's baseline with
// another generation's content.
//
// Admits run on a single goroutine, mirroring the reconciler loop that is the
// only caller of Admit in the daemon: concurrent admits would interleave the
// commit numbering itself and make the ordering assertion meaningless.
func TestCommitRangeNeverCrossesGenerations(t *testing.T) {
	const id = "repo/race"
	const iterations = 200

	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), id, "export default () => {}")

	var hashSeq, commitSeq atomic.Int64
	g.SetHashFunc(func(task.Kinded) (string, error) {
		return fmt.Sprintf("h%d", hashSeq.Add(1)), nil
	})
	g.SetCommitFunc(func(task.Kinded) (string, string) {
		return fmt.Sprintf("c%d", commitSeq.Add(1)), ""
	})
	g.SetTreeDiffFunc(func(dir, from, to string, absPaths []string) (map[string]string, map[string]string, error) {
		if commitNumber(t, from) >= commitNumber(t, to) {
			t.Errorf("markers computed over %q...%q: the baseline must be an older generation than the pending commit", from, to)
		}
		return nil, nil, nil
	})

	var mu sync.Mutex
	seen := map[string]string{}
	observe := func(hash, from string) {
		mu.Lock()
		defer mu.Unlock()
		if prev, ok := seen[hash]; ok && prev != from {
			t.Errorf("pending hash %s rendered From %q and then %q: the range crossed a generation boundary", hash, prev, from)
		}
		seen[hash] = from
	}

	stop := make(chan struct{})
	var readers, writers sync.WaitGroup
	for i := 0; i < 3; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if v, ok := g.PendingSnapshot(id); ok {
					observe(v.Hash, v.CommitRange.From)
				}
				// State and StateFor expose the range only through the
				// per-file markers, checked by the tree-diff hook above.
				_, _ = g.State(id)
				g.StateFor(id, spec)
			}
		}()
	}
	hammer := func(fn func()) {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := 0; i < iterations; i++ {
				fn()
			}
		}()
	}
	hammer(func() { _, _ = g.Admit(spec) })
	hammer(func() { _ = g.Approve(id) })
	hammer(func() { _ = g.Approve(id) })
	hammer(func() { g.Forget(id) })

	writers.Wait()
	close(stop)
	readers.Wait()
}

// commitNumber returns the sequence number of a commit stamped by
// TestCommitRangeNeverCrossesGenerations' resolver.
func commitNumber(t *testing.T, commit string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimPrefix(commit, "c"))
	if err != nil {
		t.Errorf("unexpected commit %q from the test resolver: %v", commit, err)
		return 0
	}
	return n
}
