package registry

import (
	"context"
	"testing"
	"time"

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
		if _, err := r.StartRunWithID(ctx, id, "task-a", "", "manual"); err != nil {
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
	if _, err := r.StartRunWithID(ctx, id, "t", "", "manual"); err != nil {
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
	if _, err := r.StartRunWithID(ctx, id, "t", "", "manual"); err != nil {
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
	if _, err := r.StartRunWithID(ctx, live, "task-a", "", "manual"); err != nil {
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
	if _, err := r.StartRunWithID(ctx, dead, "task-b", "", "manual"); err != nil {
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
	if _, err := r.StartRunWithID(ctx, finishedClean, "task-c", "", "manual"); err != nil {
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
