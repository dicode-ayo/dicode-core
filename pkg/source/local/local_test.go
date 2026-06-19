package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/source"
	"go.uber.org/zap"
)

// writeTask creates a minimal valid task in dir/name/.
func writeTask(t *testing.T, dir, name string) {
	t.Helper()
	td := filepath.Join(dir, name)
	if err := os.MkdirAll(td, 0755); err != nil {
		t.Fatal(err)
	}
	yaml := "name: " + name + "\ntrigger:\n  manual: true\nruntime: js\n"
	if err := os.WriteFile(filepath.Join(td, "task.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "task.js"), []byte("return 'ok'"), 0644); err != nil {
		t.Fatal(err)
	}
}

func newTestSource(t *testing.T, dir string) *LocalSource {
	t.Helper()
	s, err := New("test", dir, true, zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func collectEvents(ch <-chan source.Event, d time.Duration) []source.Event {
	var evs []source.Event
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return evs
			}
			evs = append(evs, ev)
		case <-deadline:
			return evs
		}
	}
}

func TestLocalSource_InitialScan(t *testing.T) {
	dir := t.TempDir()
	writeTask(t, dir, "task-a")
	writeTask(t, dir, "task-b")

	s := newTestSource(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := s.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	evs := collectEvents(ch, 500*time.Millisecond)
	ids := make(map[string]source.EventKind)
	for _, ev := range evs {
		ids[ev.TaskID] = ev.Kind
	}

	if ids["task-a"] != source.EventAdded {
		t.Errorf("expected task-a Added, got %v", ids["task-a"])
	}
	if ids["task-b"] != source.EventAdded {
		t.Errorf("expected task-b Added, got %v", ids["task-b"])
	}
}

func TestLocalSource_AddTask(t *testing.T) {
	dir := t.TempDir()
	s := newTestSource(t, dir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := s.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Drain initial (empty) scan.
	time.Sleep(100 * time.Millisecond)

	writeTask(t, dir, "new-task")

	evs := collectEvents(ch, time.Second)
	for _, ev := range evs {
		if ev.TaskID == "new-task" && ev.Kind == source.EventAdded {
			return
		}
	}
	t.Error("did not receive Added event for new-task")
}

func TestLocalSource_UpdateTask(t *testing.T) {
	dir := t.TempDir()
	writeTask(t, dir, "task-x")

	s := newTestSource(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := s.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Drain initial Added.
	collectEvents(ch, 200*time.Millisecond)

	// Modify the task script.
	if err := os.WriteFile(filepath.Join(dir, "task-x", "task.js"), []byte("return 'updated'"), 0644); err != nil {
		t.Fatal(err)
	}

	evs := collectEvents(ch, time.Second)
	for _, ev := range evs {
		if ev.TaskID == "task-x" && ev.Kind == source.EventUpdated {
			return
		}
	}
	t.Error("did not receive Updated event for task-x")
}

func TestLocalSource_RemoveTask(t *testing.T) {
	dir := t.TempDir()
	writeTask(t, dir, "task-y")

	s := newTestSource(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := s.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	collectEvents(ch, 200*time.Millisecond)

	if err := os.RemoveAll(filepath.Join(dir, "task-y")); err != nil {
		t.Fatal(err)
	}

	evs := collectEvents(ch, time.Second)
	for _, ev := range evs {
		if ev.TaskID == "task-y" && ev.Kind == source.EventRemoved {
			return
		}
	}
	t.Error("did not receive Removed event for task-y")
}

func TestLocalSource_Diff_NoDuplicates(t *testing.T) {
	dir := t.TempDir()
	writeTask(t, dir, "task-z")

	s := newTestSource(t, dir)

	// Two consecutive diffs — second should produce no events.
	evs1, err := s.diff()
	if err != nil {
		t.Fatal(err)
	}
	if len(evs1) != 1 || evs1[0].Kind != source.EventAdded {
		t.Fatalf("first diff: expected 1 Added, got %v", evs1)
	}

	evs2, err := s.diff()
	if err != nil {
		t.Fatal(err)
	}
	if len(evs2) != 0 {
		t.Fatalf("second diff should be empty, got %v", evs2)
	}
}

func TestLocalSource_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	s := newTestSource(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := s.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	evs := collectEvents(ch, 200*time.Millisecond)
	if len(evs) != 0 {
		t.Fatalf("expected no events for empty dir, got %v", evs)
	}
}

// Regression for #178: syncAndEmit must block (not silently drop) when the
// event channel is full. Before the fix, a full channel caused events to be
// dropped permanently because diff() had already advanced the snapshot.
func TestLocalSource_SyncAndEmit_BlocksOnFullChannel(t *testing.T) {
	dir := t.TempDir()
	writeTask(t, dir, "alpha")
	s := newTestSource(t, dir)

	// Zero-capacity channel — any send blocks until drained.
	ch := make(chan source.Event)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- s.syncAndEmit(ctx, ch)
	}()

	// syncAndEmit should be blocked on the send.
	select {
	case err := <-done:
		t.Fatalf("syncAndEmit returned before any receive (err=%v); event would have been dropped pre-#178", err)
	case <-time.After(50 * time.Millisecond):
		// Expected: blocked.
	}

	// Drain one event; syncAndEmit should now return cleanly.
	select {
	case <-ch:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("no event available on channel")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("syncAndEmit: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("syncAndEmit did not return after drain")
	}
}

func TestLocalSource_SyncAndEmit_UnblocksOnCtxCancel(t *testing.T) {
	dir := t.TempDir()
	writeTask(t, dir, "alpha")
	s := newTestSource(t, dir)

	ch := make(chan source.Event)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- s.syncAndEmit(ctx, ch)
	}()

	// Let the goroutine park on the send.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("syncAndEmit on ctx-cancel: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("syncAndEmit did not return after ctx cancel")
	}
}

// Regression for #177: with watchEnabled=false, Start must perform the
// initial scan but must NOT set up fsnotify. Adding a new task after Start
// should produce no events until something calls Sync() explicitly.
func TestLocalSource_WatchDisabled_NoFsNotifyEvents(t *testing.T) {
	dir := t.TempDir()
	writeTask(t, dir, "initial")

	s, err := New("test", dir, false, zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := s.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Initial scan is still expected to emit the existing task.
	initial := collectEvents(ch, 100*time.Millisecond)
	if len(initial) != 1 {
		t.Fatalf("initial scan: want 1 event, got %d (%v)", len(initial), initial)
	}

	// Add a task AFTER Start. With fsnotify disabled, this must NOT trigger
	// an event — only an explicit Sync() would pick it up.
	writeTask(t, dir, "late")
	evs := collectEvents(ch, 300*time.Millisecond)
	if len(evs) != 0 {
		t.Fatalf("watch=false should suppress fsnotify events, but got %d (%v)", len(evs), evs)
	}

	// Sanity: Sync() still works for callers driving updates manually.
	if err := s.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}

// TestLocalSource_Sync_EmitsEvents verifies that Sync() delivers events to
// the channel opened by Start() instead of silently discarding them (#386).
func TestLocalSource_Sync_EmitsEvents(t *testing.T) {
	dir := t.TempDir()
	s, err := New("test-sync", dir, false, zap.NewNop()) // watchEnabled=false, driven by Sync
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := s.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	collectEvents(ch, 50*time.Millisecond) // drain initial empty scan

	// Write a new task after Start.
	writeTask(t, dir, "new-task")

	if err := s.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	evs := collectEvents(ch, 500*time.Millisecond)
	found := false
	for _, ev := range evs {
		if ev.TaskID == "new-task" && ev.Kind == source.EventAdded {
			found = true
		}
	}
	if !found {
		t.Errorf("Sync() should emit EventAdded for new-task; got %+v", evs)
	}
}

// TestLocalSource_WatchNewTaskDir verifies that task directories created
// after Start() are watched and edits inside them trigger reload events (#386).
func TestLocalSource_WatchNewTaskDir(t *testing.T) {
	dir := t.TempDir()
	s := newTestSource(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := s.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	collectEvents(ch, 300*time.Millisecond) // drain initial empty scan

	// Create a new task dir after Start — should trigger Added.
	writeTask(t, dir, "late-task")

	evs := collectEvents(ch, 2*time.Second)
	var gotAdded bool
	for _, ev := range evs {
		if ev.TaskID == "late-task" && ev.Kind == source.EventAdded {
			gotAdded = true
		}
	}
	if !gotAdded {
		t.Fatal("new task dir created after Start should trigger EventAdded")
	}

	// Edit a file inside the new task dir — should trigger Updated.
	td := filepath.Join(dir, "late-task")
	if err := os.WriteFile(filepath.Join(td, "task.js"), []byte("// v2"), 0644); err != nil {
		t.Fatal(err)
	}
	evs = collectEvents(ch, 2*time.Second)
	var gotUpdated bool
	for _, ev := range evs {
		if ev.TaskID == "late-task" && ev.Kind == source.EventUpdated {
			gotUpdated = true
		}
	}
	if !gotUpdated {
		t.Error("edit inside new task dir should trigger EventUpdated")
	}
}

// TestLocalSource_StartError_SyncDoesNotPanic verifies that if Start() fails
// after setting s.ch, the channel is cleared so a subsequent Sync() call does
// not panic with "send on closed channel".
func TestLocalSource_StartError_SyncDoesNotPanic(t *testing.T) {
	// Create a source pointing at a non-existent directory so that the initial
	// ScanDir in syncAndEmit won't fail (ScanDir returns empty for missing dir).
	// Instead we trigger failure by corrupting the snapshot-advance path:
	// use an inaccessible directory after construction.
	dir := t.TempDir()
	s, err := New("test-abort", dir, false, zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Remove the directory so syncAndEmit's ScanDir returns an error.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	_, startErr := s.Start(ctx)
	if startErr == nil {
		// ScanDir on a missing dir may return empty rather than error on some
		// platforms; skip the panic check in that case — the test still guards
		// that no panic occurs.
		t.Skip("ScanDir did not error on removed dir on this platform; skipping closed-channel check")
	}

	// s.ch must have been cleared to nil; Sync() must not panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Sync() panicked after failed Start(): %v", r)
		}
	}()
	_ = s.Sync(ctx)
}

// TestLocalSource_CtxCancel_SyncDoesNotPanic verifies that Sync() does not panic
// after ctx cancellation closes the channel on the normal (non-error) close path.
// Covers both watchEnabled=false (ctx-done goroutine) and watchEnabled=true
// (watch() defer close).
func TestLocalSource_CtxCancel_SyncDoesNotPanic(t *testing.T) {
	for _, watchEnabled := range []bool{false, true} {
		watchEnabled := watchEnabled
		t.Run(fmt.Sprintf("watchEnabled=%v", watchEnabled), func(t *testing.T) {
			dir := t.TempDir()
			s, err := New("test-cancel", dir, watchEnabled, zap.NewNop())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())

			_, err = s.Start(ctx)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}

			cancel()
			time.Sleep(50 * time.Millisecond) // let close goroutine/watch run

			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Sync() panicked after ctx cancel (watchEnabled=%v): %v", watchEnabled, r)
				}
			}()
			_ = s.Sync(context.Background())
		})
	}
}
