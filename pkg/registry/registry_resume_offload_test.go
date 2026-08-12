package registry

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestSuspendRun_WithBlobRef_WritesOffloadColumnsNotInline is the core
// round-trip lock-in for #570: SuspendRun's blobRef param, when non-nil,
// must (a) leave resume_state NULL (never write the caller-supplied inline
// state alongside the offload), and (b) populate the three offload columns.
// This is the case the pre-#570 code path could never exercise — SuspendRun
// took no blobRef parameter at all.
func TestSuspendRun_WithBlobRef_WritesOffloadColumnsNotInline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newTestRegistry(t)

	runID, err := r.StartRun(ctx, "task-offload", "")
	if err != nil {
		t.Fatal(err)
	}

	ref := &ResumeStateBlobRef{StorageKey: "resume-state/" + runID, Size: 12345, StoredAt: 1714400000}
	// Pass a non-nil inline state too, to prove the blobRef path ignores it.
	if _, err := r.SuspendRun(ctx, runID, []byte(`{"should":"not persist inline"}`), nil, "tok", time.Now().UnixMilli(), 0, nil, ref); err != nil {
		t.Fatal(err)
	}

	run, err := r.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != StatusSuspended {
		t.Errorf("status = %q, want %q", run.Status, StatusSuspended)
	}
	if run.ResumeState != nil {
		t.Errorf("resume_state = %q, want nil (state must live in the blob, not inline)", run.ResumeState)
	}
	if run.ResumeStateStorageKey != ref.StorageKey {
		t.Errorf("resume_state_storage_key = %q, want %q", run.ResumeStateStorageKey, ref.StorageKey)
	}
	if run.ResumeStateSize != ref.Size {
		t.Errorf("resume_state_size = %d, want %d", run.ResumeStateSize, ref.Size)
	}
	if run.ResumeStateStoredAt != ref.StoredAt {
		t.Errorf("resume_state_stored_at = %d, want %d", run.ResumeStateStoredAt, ref.StoredAt)
	}
}

// TestSuspendRun_NilBlobRef_LeavesOffloadColumnsEmpty is the small-state
// fast-path counterpart: without a blobRef, the offload columns stay unset
// and resume_state carries the inline blob exactly as before #570.
func TestSuspendRun_NilBlobRef_LeavesOffloadColumnsEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newTestRegistry(t)

	runID, err := r.StartRun(ctx, "task-inline", "")
	if err != nil {
		t.Fatal(err)
	}
	state := []byte(`{"step":"ask_name"}`)
	if _, err := r.SuspendRun(ctx, runID, state, nil, "tok", time.Now().UnixMilli(), 0, nil, nil); err != nil {
		t.Fatal(err)
	}

	run, err := r.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if string(run.ResumeState) != string(state) {
		t.Errorf("resume_state = %q, want %q", run.ResumeState, state)
	}
	if run.ResumeStateStorageKey != "" {
		t.Errorf("resume_state_storage_key = %q, want empty", run.ResumeStateStorageKey)
	}
	if run.ResumeStateSize != 0 {
		t.Errorf("resume_state_size = %d, want 0", run.ResumeStateSize)
	}
	if run.ResumeStateStoredAt != 0 {
		t.Errorf("resume_state_stored_at = %d, want 0", run.ResumeStateStoredAt)
	}
}

func TestListExpiredResumeStates_ExcludesSuspendedRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newTestRegistry(t)

	now := time.Now().Unix()

	// expired + terminal (resumed) → eligible.
	resumedID := uuid.New().String()
	// expired + terminal (cancelled) → eligible.
	cancelledID := uuid.New().String()
	// expired but STILL suspended → must never be swept (would strand a
	// future resume — this is the core safety property of #570's GC).
	stillSuspendedID := uuid.New().String()
	// fresh (not past retention) → not eligible regardless of status.
	freshID := uuid.New().String()
	// no offloaded blob at all → never returned.
	noBlobID := uuid.New().String()

	for _, id := range []string{resumedID, cancelledID, stillSuspendedID, freshID, noBlobID} {
		if _, err := r.StartRunWithID(ctx, id, "task-a", "", "manual", "task"); err != nil {
			t.Fatal(err)
		}
	}

	mustSuspendWithBlob := func(id string, storedAt int64) {
		t.Helper()
		ref := &ResumeStateBlobRef{StorageKey: "resume-state/" + id, Size: 999, StoredAt: storedAt}
		if _, err := r.SuspendRun(ctx, id, nil, nil, "tok-"+id, time.Now().UnixMilli(), 0, nil, ref); err != nil {
			t.Fatal(err)
		}
	}
	mustSuspendWithBlob(resumedID, now-3600)
	mustSuspendWithBlob(cancelledID, now-3600)
	mustSuspendWithBlob(stillSuspendedID, now-3600)
	mustSuspendWithBlob(freshID, now+3600)

	// Move resumedID/cancelledID out of `suspended` — GC only ever considers
	// rows that have left the live-suspension state.
	if err := r.MarkRunResumed(ctx, resumedID); err != nil {
		t.Fatal(err)
	}
	if err := r.FinishRun(ctx, cancelledID, StatusCancelled); err != nil {
		t.Fatal(err)
	}

	expired, err := r.ListExpiredResumeStates(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range expired {
		got[e.RunID] = true
	}
	if len(expired) != 2 || !got[resumedID] || !got[cancelledID] {
		t.Fatalf("expired = %#v, want exactly [%s %s]", expired, resumedID, cancelledID)
	}
	if got[stillSuspendedID] {
		t.Error("a still-suspended row's blob must never be reported as expired (dangling-reference risk)")
	}
	if got[freshID] {
		t.Error("a fresh (not past retention) row must not be reported as expired")
	}
	if got[noBlobID] {
		t.Error("a row with no offloaded blob must never appear")
	}
}

func TestClearResumeStateBlob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newTestRegistry(t)

	id := uuid.New().String()
	if _, err := r.StartRunWithID(ctx, id, "t", "", "manual", "task"); err != nil {
		t.Fatal(err)
	}
	ref := &ResumeStateBlobRef{StorageKey: "resume-state/" + id, Size: 10, StoredAt: time.Now().Unix()}
	if _, err := r.SuspendRun(ctx, id, nil, nil, "tok", time.Now().UnixMilli(), 0, nil, ref); err != nil {
		t.Fatal(err)
	}

	if err := r.ClearResumeStateBlob(ctx, id); err != nil {
		t.Fatal(err)
	}
	run, err := r.GetRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if run.ResumeStateStorageKey != "" || run.ResumeStateSize != 0 || run.ResumeStateStoredAt != 0 {
		t.Errorf("offload columns not cleared: key=%q size=%d storedAt=%d",
			run.ResumeStateStorageKey, run.ResumeStateSize, run.ResumeStateStoredAt)
	}
}
