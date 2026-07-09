package webui

// Regression tests for #533: apiReplayRun reaches a storage-task fireSync via
// Replayer.Replay → InputStore.Fetch with no enclosing tracked run, so before
// the fix the shutdown drain in Engine.Start (#520/#525/#529) neither waited for
// an in-flight replay fetch nor refused a new one after shutdown latched — a
// fetch outlasting http.Server.Shutdown's ~5s cap could write against a closed
// DB. Fixed by holding an Engine.DrainSlot around Replay in apiReplayRun.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/trigger"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

type noopExecutor struct{}

func (noopExecutor) Execute(ctx context.Context, spec *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	return &pkgruntime.RunResult{RunID: opts.RunID}, nil
}

// blockingFetcher stands in for the storage-task-backed InputStore.Fetch that
// Replayer.Replay delegates to. When release is non-nil it blocks until closed,
// modelling a fetch (internally a fireSync run) still in flight during shutdown.
type blockingFetcher struct {
	startedOnce sync.Once
	started     chan struct{}
	release     chan struct{}

	mu    sync.Mutex
	calls int
}

func (f *blockingFetcher) Fetch(ctx context.Context, runID, key string, storedAt int64) (registry.PersistedInput, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.started != nil {
		f.startedOnce.Do(func() { close(f.started) })
	}
	if f.release != nil {
		<-f.release
	}
	return registry.PersistedInput{}, nil
}

func (f *blockingFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type stubReplayRunner struct{}

func (stubReplayRunner) FireForReplay(ctx context.Context, taskID, parentRunID string, input any) (string, error) {
	return "replayed-run", nil
}

// newReplayDrainEnv builds a Server wired to a real engine and a Replayer whose
// store is the given fetcher, plus a run row with a persisted-input key so
// Replay reaches store.Fetch. Returns the run ID eligible for replay.
func newReplayDrainEnv(t *testing.T, fetcher *blockingFetcher) (*Server, *trigger.Engine, string) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	reg := registry.New(d)
	eng := trigger.New(reg, noopExecutor{}, zap.NewNop())

	runID, err := reg.StartRunWithID(context.Background(), "run-1", "task-a", "", "manual", "")
	if err != nil {
		t.Fatalf("StartRunWithID: %v", err)
	}
	if err := reg.SetRunInput(context.Background(), runID, "input-key", 4, time.Now().UnixMilli(), nil); err != nil {
		t.Fatalf("SetRunInput: %v", err)
	}

	srv := &Server{engine: eng, replayer: registry.NewReplayer(reg, fetcher, stubReplayRunner{})}
	return srv, eng, runID
}

func replayReq(runID string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+runID+"/replay", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("runID", runID)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// TestShutdownDrainsInFlightReplayFetch proves apiReplayRun holds the shutdown
// drain open until its replay fetch finishes: Start must not return while the
// handler is still blocked inside Replayer.Replay's store.Fetch, and must return
// promptly once the fetch completes.
func TestShutdownDrainsInFlightReplayFetch(t *testing.T) {
	fetcher := &blockingFetcher{started: make(chan struct{}), release: make(chan struct{})}
	srv, eng, runID := newReplayDrainEnv(t, fetcher)

	ctx, cancel := context.WithCancel(context.Background())
	startDone := make(chan error, 1)
	go func() { startDone <- eng.Start(ctx) }()

	replayDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		srv.apiReplayRun(w, replayReq(runID))
		replayDone <- w
	}()

	select {
	case <-fetcher.started:
	case <-time.After(2 * time.Second):
		t.Fatal("replay fetch never started")
	}

	cancel()

	// The handler is still blocked in store.Fetch holding a drain slot: shutdown
	// must not let Start return until it's released. Before the fix apiReplayRun
	// reserved no slot, so Start returned here regardless of the in-flight fetch.
	select {
	case <-startDone:
		t.Fatal("Start returned while the replay fetch was still in flight — shutdown did not drain apiReplayRun")
	case <-time.After(300 * time.Millisecond):
	}

	close(fetcher.release)

	select {
	case <-startDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the replay fetch finished")
	}

	select {
	case w := <-replayDone:
		if w.Code != http.StatusOK {
			t.Fatalf("replay = %d, want 200; body=%s", w.Code, w.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replay handler never returned")
	}
}

// TestShutdownRefusesReplayAfterShutdownLatched proves the drain is a fence, not
// just a wait: once shutdown has latched, a new apiReplayRun is refused (5xx)
// before it dispatches, so it can neither race the drain nor write after DB
// close. Latching is driven through Engine.Start (beginShutdown is unexported).
func TestShutdownRefusesReplayAfterShutdownLatched(t *testing.T) {
	fetcher := &blockingFetcher{}
	srv, eng, runID := newReplayDrainEnv(t, fetcher)

	ctx, cancel := context.WithCancel(context.Background())
	startDone := make(chan error, 1)
	go func() { startDone <- eng.Start(ctx) }()
	cancel()
	select {
	case <-startDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after ctx cancel")
	}

	w := httptest.NewRecorder()
	srv.apiReplayRun(w, replayReq(runID))

	if w.Code < 500 {
		t.Fatalf("replay after shutdown latched = %d, want a server-error rejection; body=%s", w.Code, w.Body.String())
	}
	if fetcher.count() != 0 {
		t.Errorf("store.Fetch called %d time(s) after shutdown latched; replay must be refused before dispatch", fetcher.count())
	}
}
