package trigger

// Regression test for #529: the sync-webhook fireSync path (fireWebhookTask,
// the `wait=true` default of the webhook trigger) reserved no runWG slot, so
// the shutdown drain in Engine.Start (engine.go, #520/#525) did not wait for
// it — a sync webhook run outlasting http.Server.Shutdown's ~5s cap could hit
// FinishRun/chain writes against a closed DB. Fixed by tracking the top-level
// sync fire in fireWebhookTask itself (webhook.go), mirroring fireAsync's own
// trackRun/runWG.Done pattern.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

// TestShutdownDrainsInFlightSyncWebhookRun proves a sync-webhook-triggered run
// holds the shutdown drain open until it finishes: Start must not return while
// the webhook handler is still blocked inside fireSync, and once the run
// completes the drain must let Start return promptly with the run finalized.
func TestShutdownDrainsInFlightSyncWebhookRun(t *testing.T) {
	exec := &drainExec{started: make(chan struct{}), block: make(chan struct{})}
	eng, reg := newDrainEnv(t, exec)

	spec := &task.Spec{
		ID:      "drain-sync-webhook",
		Name:    "drain-sync-webhook",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Webhook: "/hooks/drain-sync"},
		Enabled: true,
	}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}
	if err := eng.Register(spec); err != nil {
		t.Fatalf("eng.Register: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	startDone := make(chan error, 1)
	go func() { startDone <- eng.Start(ctx) }()

	handler := eng.WebhookHandler()
	webhookDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/hooks/drain-sync", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		webhookDone <- w
	}()

	select {
	case <-exec.started:
	case <-time.After(drainWait):
		t.Fatal("sync webhook task body never started")
	}

	cancel()

	// The webhook's executor is still blocked on exec.block: shutdown must not
	// let Start return until it's released. Before the fix, fireSync reserved
	// no runWG slot, so Start returned here regardless of the in-flight run.
	select {
	case <-startDone:
		t.Fatal("Start returned while the sync-webhook run was still in flight — shutdown did not drain fireSync")
	case <-time.After(300 * time.Millisecond):
	}

	close(exec.block)

	select {
	case <-startDone:
	case <-time.After(drainWait):
		t.Fatal("Start did not return after the sync-webhook run finished")
	}

	var w *httptest.ResponseRecorder
	select {
	case w = <-webhookDone:
	case <-time.After(drainWait):
		t.Fatal("webhook handler never returned")
	}
	if w.Code >= 400 {
		t.Fatalf("webhook fire failed: %d %s", w.Code, w.Body.String())
	}

	runID := w.Header().Get("X-Run-Id")
	if runID == "" {
		t.Fatal("X-Run-Id header not set — cannot look up run")
	}
	run, err := reg.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun(%q): %v", runID, err)
	}
	if run.Status == registry.StatusRunning || run.Status == "" {
		t.Fatalf("run %q status = %q; Start returned before finalization drained", runID, run.Status)
	}
}

// TestShutdownRefusesNewSyncWebhookAfterShutdownLatched proves the drain is a
// fence, not just a wait: once shutdown has latched, a new sync-webhook fire
// must be refused (503-equivalent) rather than start a run that races the
// drain's Wait or writes after DB close.
func TestShutdownRefusesNewSyncWebhookAfterShutdownLatched(t *testing.T) {
	exec := &drainExec{started: make(chan struct{})}
	eng, reg := newDrainEnv(t, exec)

	spec := &task.Spec{
		ID:      "drain-sync-webhook-refused",
		Name:    "drain-sync-webhook-refused",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Webhook: "/hooks/drain-sync-refused"},
		Enabled: true,
	}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}
	if err := eng.Register(spec); err != nil {
		t.Fatalf("eng.Register: %v", err)
	}

	eng.beginShutdown()

	handler := eng.WebhookHandler()
	req := httptest.NewRequest(http.MethodPost, "/hooks/drain-sync-refused", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code < 500 {
		t.Fatalf("webhook fire after shutdown latched = %d, want a server-error rejection", w.Code)
	}
	if exec.count(spec.ID) != 0 {
		t.Errorf("task executed %d time(s) after shutdown latched; fire must be refused before dispatch", exec.count(spec.ID))
	}
	runs, err := reg.ListRuns(context.Background(), spec.ID, 10)
	if err != nil {
		t.Fatalf("ListRuns(%q): %v", spec.ID, err)
	}
	if len(runs) != 0 {
		t.Errorf("task %q has %d run row(s); a refused fire must create none", spec.ID, len(runs))
	}
}
