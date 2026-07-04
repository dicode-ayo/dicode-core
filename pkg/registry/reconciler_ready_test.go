package registry

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/source"
)

// Tests for the first-sync readiness barrier (#464). Ready must close only
// after every initially-configured source's initial scan batch has been
// handled — a consumer woken by Ready must be able to Get the initial tasks.

func TestReconciler_Ready_AfterInitialSync(t *testing.T) {
	dir := t.TempDir()
	td := writeTask(t, dir, "init-task")

	fs := newFakeSource("test")
	// Buffer the initial inventory BEFORE Run starts, mirroring how real
	// sources emit their initial scan synchronously inside Start.
	fs.ch <- source.Event{Kind: source.EventAdded, TaskID: "init-task", TaskDir: td, Source: "test"}

	reg, rec := newTestReconciler(t, fs)

	select {
	case <-rec.Ready():
		t.Fatal("ready closed before Run")
	default:
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rec.Run(ctx)

	select {
	case <-rec.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("reconciler never became ready")
	}
	// The barrier orders after the initial batch: the task must already be
	// registered by the time Ready fires — this is the #464 guarantee.
	if _, ok := reg.Get("init-task"); !ok {
		t.Fatal("initial task not registered when Ready fired")
	}
}

func TestReconciler_Ready_NoSources(t *testing.T) {
	_, rec := newTestReconciler(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rec.Run(ctx)

	select {
	case <-rec.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("zero-source reconciler never became ready")
	}
}

func TestReconciler_Ready_WaitsForAllSources(t *testing.T) {
	dir := t.TempDir()
	td := writeTask(t, dir, "multi-task")

	// One source with a buffered initial event, one that starts empty.
	full := newFakeSource("full")
	full.ch <- source.Event{Kind: source.EventAdded, TaskID: "multi-task", TaskDir: td, Source: "full"}
	empty := newFakeSource("empty")

	reg, rec := newTestReconciler(t, full, empty)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rec.Run(ctx)

	select {
	case <-rec.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("multi-source reconciler never became ready")
	}
	if _, ok := reg.Get("multi-task"); !ok {
		t.Fatal("initial task not registered when Ready fired")
	}
}

// TestReconciler_Ready_NoDeadlockOnEarlyShutdown pins the shutdown-safety
// contract: cancelling the run context before the first sync completes must
// not deadlock Run — it returns nil promptly whether or not Ready ever
// closed (consumers of Ready select on their own timeout/context).
func TestReconciler_Ready_NoDeadlockOnEarlyShutdown(t *testing.T) {
	fs := newFakeSource("test")
	_, rec := newTestReconciler(t, fs)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // shut down before the first sync can complete

	done := make(chan error, 1)
	go func() { done <- rec.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error on cancelled ctx: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}

// TestReconciler_Ready_ConcurrentReaders exercises the close-once semantics
// under the race detector: many goroutines block on Ready while the Run loop
// closes it.
func TestReconciler_Ready_ConcurrentReaders(t *testing.T) {
	dir := t.TempDir()
	td := writeTask(t, dir, "race-task")

	fs := newFakeSource("test")
	fs.ch <- source.Event{Kind: source.EventAdded, TaskID: "race-task", TaskDir: td, Source: "test"}
	_, rec := newTestReconciler(t, fs)

	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-rec.Ready():
			case <-time.After(5 * time.Second):
				errs <- "reader timed out waiting for Ready"
			}
		}()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rec.Run(ctx)

	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
}
