package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/google/uuid"
)

func TestListExpiredInputs_Basic(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	now := time.Now().Unix()
	expiredID := uuid.New().String()
	freshID := uuid.New().String()
	pinnedID := uuid.New().String()

	for _, id := range []string{expiredID, freshID, pinnedID} {
		if _, err := r.StartRunWithID(ctx, id, "task-a", "", "manual", "task"); err != nil {
			t.Fatal(err)
		}
	}

	if err := r.SetRunInput(ctx, expiredID, "k1", 100, now-3600, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRunInput(ctx, freshID, "k2", 100, now+3600, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRunInput(ctx, pinnedID, "k3", 100, now-3600, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.PinRunInput(ctx, pinnedID); err != nil {
		t.Fatal(err)
	}

	expired, err := r.ListExpiredInputs(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 {
		t.Fatalf("got %d expired, want 1: %#v", len(expired), expired)
	}
	if expired[0].RunID != expiredID {
		t.Errorf("got RunID %q, want %q", expired[0].RunID, expiredID)
	}
}

func TestPinUnpinRunInput(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	id := uuid.New().String()
	if _, err := r.StartRunWithID(ctx, id, "t", "", "manual", "task"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRunInput(ctx, id, "k", 1, time.Now().Unix(), nil); err != nil {
		t.Fatal(err)
	}

	if err := r.PinRunInput(ctx, id); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.InputPinned != 1 {
		t.Errorf("after Pin: got %d, want 1", got.InputPinned)
	}

	if err := r.UnpinRunInput(ctx, id); err != nil {
		t.Fatal(err)
	}
	got, _ = r.GetRun(ctx, id)
	if got.InputPinned != 0 {
		t.Errorf("after Unpin: got %d, want 0", got.InputPinned)
	}
}

func TestClearRunInput(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	id := uuid.New().String()
	if _, err := r.StartRunWithID(ctx, id, "t", "", "manual", "task"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRunInput(ctx, id, "k", 1, time.Now().Unix(), []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if err := r.ClearRunInput(ctx, id); err != nil {
		t.Fatal(err)
	}
	got, _ := r.GetRun(ctx, id)
	if got.InputStorageKey != "" {
		t.Errorf("InputStorageKey not cleared: %q", got.InputStorageKey)
	}
}

func TestSweepStalePins_ClearsFinishedPinnedRows(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	// Live + pinned: must NOT be cleared.
	live := uuid.New().String()
	if _, err := r.StartRunWithID(ctx, live, "task-a", "", "manual", "task"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRunInput(ctx, live, "k1", 100, time.Now().Unix(), nil); err != nil {
		t.Fatal(err)
	}
	if err := r.PinRunInput(ctx, live); err != nil {
		t.Fatal(err)
	}

	// Finished + pinned: MUST be cleared.
	dead := uuid.New().String()
	if _, err := r.StartRunWithID(ctx, dead, "task-b", "", "manual", "task"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRunInput(ctx, dead, "k2", 100, time.Now().Unix(), nil); err != nil {
		t.Fatal(err)
	}
	if err := r.PinRunInput(ctx, dead); err != nil {
		t.Fatal(err)
	}
	if err := r.FinishRun(ctx, dead, StatusFailure); err != nil {
		t.Fatal(err)
	}

	// Finished + already-unpinned: must remain unpinned (no double-write).
	finishedClean := uuid.New().String()
	if _, err := r.StartRunWithID(ctx, finishedClean, "task-c", "", "manual", "task"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRunInput(ctx, finishedClean, "k3", 100, time.Now().Unix(), nil); err != nil {
		t.Fatal(err)
	}
	if err := r.FinishRun(ctx, finishedClean, StatusSuccess); err != nil {
		t.Fatal(err)
	}

	cleared, err := r.SweepStalePins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cleared != 1 {
		t.Errorf("cleared = %d, want 1", cleared)
	}

	// Verify state.
	got, _ := r.GetRun(ctx, live)
	if got.InputPinned != 1 {
		t.Errorf("live pin cleared (should be retained)")
	}
	got, _ = r.GetRun(ctx, dead)
	if got.InputPinned != 0 {
		t.Errorf("dead pin not cleared")
	}
	got, _ = r.GetRun(ctx, finishedClean)
	if got.InputPinned != 0 {
		t.Errorf("finishedClean pin spuriously set")
	}
}

func TestChunkStrings(t *testing.T) {
	cases := []struct {
		name string
		n    int
		in   []string
		want [][]string
	}{
		{"empty", 2, nil, nil},
		{"exact multiple", 2, []string{"a", "b", "c", "d"}, [][]string{{"a", "b"}, {"c", "d"}}},
		{"remainder", 2, []string{"a", "b", "c"}, [][]string{{"a", "b"}, {"c"}}},
		{"fits in one chunk", 5, []string{"a", "b"}, [][]string{{"a", "b"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chunkStrings(tc.in, tc.n)
			if len(got) != len(tc.want) {
				t.Fatalf("chunkStrings(%v, %d) = %v, want %v", tc.in, tc.n, got, tc.want)
			}
			for i := range got {
				if len(got[i]) != len(tc.want[i]) {
					t.Fatalf("chunk %d = %v, want %v", i, got[i], tc.want[i])
				}
				for j := range got[i] {
					if got[i][j] != tc.want[i][j] {
						t.Fatalf("chunk %d = %v, want %v", i, got[i], tc.want[i])
					}
				}
			}
		})
	}
}

// TestGetRunInputKeys_ClearRunInputs_Batch is the regression test for #819:
// the batched forms must resolve storage keys and clear columns for every
// row in one call each, matching what N sequential GetRun/ClearRunInput
// calls would have done, and must skip rows that never had an input without
// erroring.
func TestGetRunInputKeys_ClearRunInputs_Batch(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	withInput1 := uuid.New().String()
	withInput2 := uuid.New().String()
	withoutInput := uuid.New().String()

	for _, id := range []string{withInput1, withInput2, withoutInput} {
		if _, err := r.StartRunWithID(ctx, id, "task-a", "", "manual", "task"); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.SetRunInput(ctx, withInput1, "key-1", 10, time.Now().Unix(), nil); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRunInput(ctx, withInput2, "key-2", 20, time.Now().Unix(), nil); err != nil {
		t.Fatal(err)
	}
	// withoutInput deliberately never gets SetRunInput.

	ids := []string{withInput1, withInput2, withoutInput}
	keys, err := r.GetRunInputKeys(ctx, ids)
	if err != nil {
		t.Fatalf("GetRunInputKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("got %d keys, want 2: %#v", len(keys), keys)
	}
	if keys[withInput1] != "key-1" {
		t.Errorf("keys[%s] = %q, want key-1", withInput1, keys[withInput1])
	}
	if keys[withInput2] != "key-2" {
		t.Errorf("keys[%s] = %q, want key-2", withInput2, keys[withInput2])
	}
	if _, ok := keys[withoutInput]; ok {
		t.Errorf("keys should not contain %s (no input)", withoutInput)
	}

	if err := r.ClearRunInputs(ctx, ids); err != nil {
		t.Fatalf("ClearRunInputs: %v", err)
	}
	for _, id := range []string{withInput1, withInput2} {
		got, err := r.GetRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.InputStorageKey != "" {
			t.Errorf("run %s InputStorageKey not cleared: %q", id, got.InputStorageKey)
		}
	}
}

// TestClearRunInputs_ChunksAcrossManyRows verifies ClearRunInputs and
// GetRunInputKeys still work correctly when the row count exceeds a single
// IN (...) clause's chunk size (#819). It shrinks the package's
// maxInClauseVars for the duration of the test so GetRunInputKeys and
// ClearRunInputs themselves are forced to issue multiple real SQL round
// trips and reassemble the results — not just chunkStrings in isolation —
// without needing 500+ real rows to hit the production chunk size.
func TestClearRunInputs_ChunksAcrossManyRows(t *testing.T) {
	orig := maxInClauseVars
	maxInClauseVars = 3
	t.Cleanup(func() { maxInClauseVars = orig })

	r := newTestRegistry(t)
	ctx := context.Background()

	const n = 7
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id := uuid.New().String()
		ids[i] = id
		if _, err := r.StartRunWithID(ctx, id, "task-a", "", "manual", "task"); err != nil {
			t.Fatal(err)
		}
		if err := r.SetRunInput(ctx, id, fmt.Sprintf("key-%d", i), 1, time.Now().Unix(), nil); err != nil {
			t.Fatal(err)
		}
	}

	// n=7 rows over a chunk size of 3 forces GetRunInputKeys/ClearRunInputs
	// below to each issue 3 real chunks (3+3+1), not 1.
	if got, want := len(chunkStrings(ids, maxInClauseVars)), 3; got != want {
		t.Fatalf("chunkStrings(ids, %d) produced %d chunks, want %d — test setup no longer forces multiple chunks", maxInClauseVars, got, want)
	}

	keys, err := r.GetRunInputKeys(ctx, ids)
	if err != nil {
		t.Fatalf("GetRunInputKeys: %v", err)
	}
	if len(keys) != n {
		t.Fatalf("got %d keys, want %d", len(keys), n)
	}

	if err := r.ClearRunInputs(ctx, ids); err != nil {
		t.Fatalf("ClearRunInputs: %v", err)
	}
	for _, id := range ids {
		got, err := r.GetRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.InputStorageKey != "" {
			t.Errorf("run %s InputStorageKey not cleared", id)
		}
	}
}

func TestClearRunInputs_EmptyIsNoOp(t *testing.T) {
	r := newTestRegistry(t)
	if err := r.ClearRunInputs(context.Background(), nil); err != nil {
		t.Fatalf("ClearRunInputs(nil): %v", err)
	}
}

// failNthExecDB wraps a db.DB and fails the n-th Exec call whose query
// contains match, succeeding on every other call (including later calls
// matching the same substring). Used to simulate one chunk of a batched
// write failing without the others.
type failNthExecDB struct {
	db.DB
	match string
	n     int
	seen  int
}

func (f *failNthExecDB) Exec(ctx context.Context, query string, args ...any) error {
	if strings.Contains(query, f.match) {
		f.seen++
		if f.seen == f.n {
			return errors.New("simulated exec failure")
		}
	}
	return f.DB.Exec(ctx, query, args...)
}

// TestClearRunInputs_ContinuesPastFailingChunk is the regression test for the
// code-review follow-up on #819: ClearRunInputs must attempt every chunk even
// after one fails, rather than bailing out and leaving every row in every
// later chunk with a dangling input_storage_key (their blobs are already
// deleted by the delete_inputs IPC handler before ClearRunInputs is ever
// called — see pkg/ipc/server.go).
func TestClearRunInputs_ContinuesPastFailingChunk(t *testing.T) {
	orig := maxInClauseVars
	maxInClauseVars = 2
	t.Cleanup(func() { maxInClauseVars = orig })

	real, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { real.Close() })
	r := New(real)
	ctx := context.Background()

	// 6 ids over a chunk size of 2 makes 3 chunks: [0,1] [2,3] [4,5].
	const n = 6
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id := uuid.New().String()
		ids[i] = id
		if _, err := r.StartRunWithID(ctx, id, "task-a", "", "manual", "task"); err != nil {
			t.Fatal(err)
		}
		if err := r.SetRunInput(ctx, id, fmt.Sprintf("key-%d", i), 1, time.Now().Unix(), nil); err != nil {
			t.Fatal(err)
		}
	}

	// Fail the 2nd UPDATE (the middle chunk, ids[2:4]) only.
	r.db = &failNthExecDB{DB: real, match: "UPDATE runs SET input_storage_key", n: 2}

	err = r.ClearRunInputs(ctx, ids)
	if err == nil {
		t.Fatal("expected an error from the failing middle chunk")
	}

	// Swap back to the real db to read state without going through the fake.
	r.db = real

	cleared := func(id string) bool {
		got, gerr := r.GetRun(ctx, id)
		if gerr != nil {
			t.Fatal(gerr)
		}
		return got.InputStorageKey == ""
	}

	for _, id := range []string{ids[0], ids[1]} {
		if !cleared(id) {
			t.Errorf("chunk 1 run %s should be cleared", id)
		}
	}
	for _, id := range []string{ids[2], ids[3]} {
		if cleared(id) {
			t.Errorf("chunk 2 run %s should NOT be cleared (its UPDATE failed)", id)
		}
	}
	// The key point of this test: chunk 3 must still have been attempted
	// (and succeeded) despite chunk 2 failing first.
	for _, id := range []string{ids[4], ids[5]} {
		if !cleared(id) {
			t.Errorf("chunk 3 run %s should be cleared — ClearRunInputs must not stop after chunk 2 fails", id)
		}
	}
}

func TestSweepStalePins_NoOpWhenNoPins(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	cleared, err := r.SweepStalePins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cleared != 0 {
		t.Errorf("cleared = %d, want 0", cleared)
	}
}
