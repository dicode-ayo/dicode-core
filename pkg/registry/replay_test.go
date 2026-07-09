package registry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeReplayRunner records the spec ID + opts the replay primitive fires
// against. Doesn't actually run anything.
type fakeReplayRunner struct {
	calls []replayCall
	err   error
}

type replayCall struct {
	taskID      string
	parentRunID string
	input       any
}

func (f *fakeReplayRunner) FireForReplay(ctx context.Context, taskID, parentRunID string, input any) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.calls = append(f.calls, replayCall{taskID: taskID, parentRunID: parentRunID, input: input})
	return uuid.New().String(), nil
}

func TestReplay_FetchesInputAndFires(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	frozen := time.Unix(1714400000, 0)
	prev := timeNow
	timeNow = func() time.Time { return frozen }
	defer func() { timeNow = prev }()

	// Persist an input via the round-trip helpers from #233.
	mr := &mockRunner{store: map[string]string{}}
	c := newTestInputCrypto(t)
	is := NewInputStore(c, mr, "fake-storage")

	originalRunID := uuid.New().String()
	if _, err := r.StartRunWithID(ctx, originalRunID, "user-task", "", "manual", "task"); err != nil {
		t.Fatal(err)
	}
	in := PersistedInput{Source: "webhook", Method: "POST"}
	key, size, storedAt, err := is.Persist(ctx, originalRunID, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetRunInput(ctx, originalRunID, key, size, storedAt, nil); err != nil {
		t.Fatal(err)
	}

	runner := &fakeReplayRunner{}
	replayer := NewReplayer(r, is, runner)

	newRunID, err := replayer.Replay(ctx, originalRunID, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if newRunID == "" {
		t.Error("new run ID empty")
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(runner.calls))
	}
	call := runner.calls[0]
	if call.taskID != "user-task" {
		t.Errorf("taskID = %q, want user-task", call.taskID)
	}
	if call.parentRunID != originalRunID {
		t.Errorf("parentRunID = %q, want %q", call.parentRunID, originalRunID)
	}
	got, ok := call.input.(PersistedInput)
	if !ok {
		t.Fatalf("input type = %T, want PersistedInput", call.input)
	}
	if got.Source != "webhook" || got.Method != "POST" {
		t.Errorf("input = %#v", got)
	}
}

func TestReplay_TaskNameOverride(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	mr := &mockRunner{store: map[string]string{}}
	is := NewInputStore(newTestInputCrypto(t), mr, "fake-storage")

	originalRunID := uuid.New().String()
	if _, err := r.StartRunWithID(ctx, originalRunID, "user-task", "", "manual", "task"); err != nil {
		t.Fatal(err)
	}
	key, size, storedAt, err := is.Persist(ctx, originalRunID, PersistedInput{Source: "webhook"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetRunInput(ctx, originalRunID, key, size, storedAt, nil); err != nil {
		t.Fatal(err)
	}

	runner := &fakeReplayRunner{}
	replayer := NewReplayer(r, is, runner)

	if _, err := replayer.Replay(ctx, originalRunID, "different-task", "", ""); err != nil {
		t.Fatal(err)
	}
	if runner.calls[0].taskID != "different-task" {
		t.Errorf("taskID = %q, want different-task", runner.calls[0].taskID)
	}
}

func TestReplay_NoStoredInput_ReturnsErrInputUnavailable(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	mr := &mockRunner{store: map[string]string{}}
	is := NewInputStore(newTestInputCrypto(t), mr, "fake-storage")

	originalRunID := uuid.New().String()
	if _, err := r.StartRunWithID(ctx, originalRunID, "user-task", "", "manual", "task"); err != nil {
		t.Fatal(err)
	}
	// Note: no SetRunInput — column is empty.

	runner := &fakeReplayRunner{}
	replayer := NewReplayer(r, is, runner)

	_, err := replayer.Replay(ctx, originalRunID, "", "", "")
	if !errors.Is(err, ErrInputUnavailable) {
		t.Errorf("got %v, want ErrInputUnavailable", err)
	}
}

func TestReplay_RunNotFound(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	is := NewInputStore(newTestInputCrypto(t), &mockRunner{store: map[string]string{}}, "fake-storage")
	runner := &fakeReplayRunner{}
	replayer := NewReplayer(r, is, runner)

	_, err := replayer.Replay(ctx, uuid.New().String(), "", "", "")
	if err == nil {
		t.Error("expected error for unknown run ID")
	}
}

// A suspended run must not be replayable server-side: replay would spawn a
// duplicate execution while the original still holds a live resume token.
// The guard fires before the input check and never invokes the runner.
func TestReplay_SuspendedRunRejected(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	is := NewInputStore(newTestInputCrypto(t), &mockRunner{store: map[string]string{}}, "fake-storage")

	runID := uuid.New().String()
	if _, err := r.StartRunWithID(ctx, runID, "user-task", "", "manual", "task"); err != nil {
		t.Fatal(err)
	}
	key, size, storedAt, err := is.Persist(ctx, runID, PersistedInput{Source: "webhook"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetRunInput(ctx, runID, key, size, storedAt, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SuspendRun(ctx, runID, []byte(`{"step":1}`), nil, "tok", time.Now().UnixMilli(), 0, nil); err != nil {
		t.Fatal(err)
	}

	runner := &fakeReplayRunner{}
	replayer := NewReplayer(r, is, runner)

	_, err = replayer.Replay(ctx, runID, "", "", "")
	if !errors.Is(err, ErrRunNotReplayable) {
		t.Errorf("got %v, want ErrRunNotReplayable", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("runner fired %d times on a suspended run; want 0", len(runner.calls))
	}
}
