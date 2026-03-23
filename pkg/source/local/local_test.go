package local

import (
	"context"
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
	s, err := New("test", dir, zap.NewNop())
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
