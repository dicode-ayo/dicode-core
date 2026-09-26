package registry

import (
	"context"
	"errors"
	"testing"

	"github.com/dicode/dicode/pkg/db"
	"github.com/google/uuid"
)

// stubFetcher satisfies the fetcher interface for ownership tests.
// It does not touch real storage.
type stubFetcher struct{}

func (stubFetcher) Fetch(_ context.Context, _, _ string, _ int64) (PersistedInput, error) {
	return PersistedInput{Source: "stub"}, nil
}

// insertRun inserts a minimal run row with the given runID and taskID.
// parent_run_id is NULL.
func insertRun(t *testing.T, d db.DB, runID, taskID string) {
	t.Helper()
	err := d.Exec(context.Background(),
		`INSERT INTO runs(id, task_id, status, started_at, parent_run_id, trigger_source, input_storage_key, input_stored_at)
		 VALUES (?,?,?,?,NULL,?,?,?)`,
		runID, taskID, "success", 0, "manual", "key1", 0)
	if err != nil {
		t.Fatalf("insertRun: %v", err)
	}
}

func TestReplay_OwnershipAllowsSelfTask(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	reg := New(d)

	runID := uuid.New().String()
	insertRun(t, d, runID, "task-a")

	r := &Replayer{registry: reg, store: stubFetcher{}, runner: &fakeReplayRunner{}}

	// caller task-a replays its own run — allowed.
	if _, err := r.Replay(context.Background(), runID, "", "task-a", ""); err != nil {
		t.Errorf("self-task replay rejected: %v", err)
	}
}

func TestReplay_OwnershipBlocksCrossTask(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	reg := New(d)

	runID := uuid.New().String()
	insertRun(t, d, runID, "task-a")

	r := &Replayer{registry: reg, store: stubFetcher{}, runner: &fakeReplayRunner{}}

	// caller task-b, no lineage → must be rejected.
	_, replayErr := r.Replay(context.Background(), runID, "", "task-b", "")
	if !errors.Is(replayErr, ErrReplayNotPermitted) {
		t.Errorf("expected ErrReplayNotPermitted, got %v", replayErr)
	}
}

func TestReplay_OwnershipAllowsParentRun(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	reg := New(d)

	runID := uuid.New().String()
	insertRun(t, d, runID, "task-a")

	r := &Replayer{registry: reg, store: stubFetcher{}, runner: &fakeReplayRunner{}}

	// caller task-b whose parent_run_id == runID — auto-fix lineage, allowed.
	if _, err := r.Replay(context.Background(), runID, "", "task-b", runID); err != nil {
		t.Errorf("parent-run replay rejected: %v", err)
	}
}

func TestReplay_OwnershipEmptyCallerBypasses(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	reg := New(d)

	runID := uuid.New().String()
	insertRun(t, d, runID, "task-a")

	r := &Replayer{registry: reg, store: stubFetcher{}, runner: &fakeReplayRunner{}}

	// Empty caller fields — REST-handler / backwards-compat bypass.
	if _, err := r.Replay(context.Background(), runID, "", "", ""); err != nil {
		t.Errorf("empty-caller replay rejected: %v", err)
	}
}
