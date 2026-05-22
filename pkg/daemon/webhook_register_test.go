package daemon

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/task"
)

// TestRegisterGatewayWebhook_Pipeline is the regression test for GAP 1: a
// kind: PipelineTask with trigger.webhook must get its gateway route wired so
// POST /hooks/<path> reaches the engine WebhookHandler instead of 404ing.
// Before the fix, OnRegister returned early for non-*task.Spec kinds and never
// called gateway.Register for pipelines.
func TestRegisterGatewayWebhook_Pipeline(t *testing.T) {
	gateway := ipc.NewGateway()
	var webhookMu sync.Mutex
	webhookPaths := make(map[string]string)

	hit := false
	webhookH := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	})

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "p", Name: "P", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Webhook: "/hooks/pipe"},
		Stages:  []task.Stage{{Task: "s"}},
	}

	registerGatewayWebhook(gateway, webhookPaths, &webhookMu, webhookH, pipe)

	// The gateway must now route a POST for the pipeline's webhook path.
	w := httptest.NewRecorder()
	gateway.ServeHTTP(w, httptest.NewRequest("POST", "/hooks/pipe", nil))
	if w.Code != http.StatusOK || !hit {
		t.Fatalf("pipeline webhook not routed by gateway: code=%d hit=%v", w.Code, hit)
	}

	// And the path must be recorded under the pipeline ID so OnUnregister
	// cleanup deregisters it.
	webhookMu.Lock()
	got := webhookPaths["p"]
	webhookMu.Unlock()
	if got != "/hooks/pipe" {
		t.Fatalf("webhookPaths[p] = %q, want /hooks/pipe", got)
	}
}

// TestRegisterGatewayWebhook_Task confirms the kind: Task path still wires the
// gateway route (no regression to the original behaviour).
func TestRegisterGatewayWebhook_Task(t *testing.T) {
	gateway := ipc.NewGateway()
	var webhookMu sync.Mutex
	webhookPaths := make(map[string]string)

	hit := false
	webhookH := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	})

	spec := &task.Spec{
		ID: "t", Name: "T", Enabled: true, Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Webhook: "/hooks/task"},
	}

	registerGatewayWebhook(gateway, webhookPaths, &webhookMu, webhookH, spec)

	w := httptest.NewRecorder()
	gateway.ServeHTTP(w, httptest.NewRequest("POST", "/hooks/task", nil))
	if w.Code != http.StatusOK || !hit {
		t.Fatalf("task webhook not routed by gateway: code=%d hit=%v", w.Code, hit)
	}

	webhookMu.Lock()
	got := webhookPaths["t"]
	webhookMu.Unlock()
	if got != "/hooks/task" {
		t.Fatalf("webhookPaths[t] = %q, want /hooks/task", got)
	}
}

// TestRegisterGatewayWebhook_PipelineUnregisterCleanup confirms the OnUnregister
// cleanup contract: because registerGatewayWebhook records the pipeline path
// under its task ID, the same generic cleanup the daemon runs for kind: Task
// (look up webhookPaths[id] → gateway.Unregister) deregisters the pipeline's
// route. This guards against a pipeline webhook lingering after removal.
func TestRegisterGatewayWebhook_PipelineUnregisterCleanup(t *testing.T) {
	gateway := ipc.NewGateway()
	var webhookMu sync.Mutex
	webhookPaths := make(map[string]string)
	webhookH := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "p", Name: "P", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Webhook: "/hooks/pipe"},
		Stages:  []task.Stage{{Task: "s"}},
	}
	registerGatewayWebhook(gateway, webhookPaths, &webhookMu, webhookH, pipe)

	// Mirror the daemon's OnUnregister cleanup (kind-agnostic, keyed by id).
	webhookMu.Lock()
	path := webhookPaths["p"]
	delete(webhookPaths, "p")
	webhookMu.Unlock()
	if path != "" {
		gateway.Unregister(path)
	}

	w := httptest.NewRecorder()
	gateway.ServeHTTP(w, httptest.NewRequest("POST", "/hooks/pipe", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("pipeline webhook route not cleaned up: got %d, want 404", w.Code)
	}
}

// TestRegisterGatewayWebhook_NoWebhook ensures a pipeline/task without a webhook
// trigger claims no gateway route (so manual/cron-only specs aren't routed).
func TestRegisterGatewayWebhook_NoWebhook(t *testing.T) {
	gateway := ipc.NewGateway()
	var webhookMu sync.Mutex
	webhookPaths := make(map[string]string)
	webhookH := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "cron-pipe", Name: "CP", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Cron: "0 0 * * *"},
		Stages:  []task.Stage{{Task: "s"}},
	}
	registerGatewayWebhook(gateway, webhookPaths, &webhookMu, webhookH, pipe)

	webhookMu.Lock()
	_, recorded := webhookPaths["cron-pipe"]
	webhookMu.Unlock()
	if recorded {
		t.Fatal("cron-only pipeline must not claim a gateway webhook path")
	}
	w := httptest.NewRecorder()
	gateway.ServeHTTP(w, httptest.NewRequest("POST", "/hooks/pipe", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unregistered path, got %d", w.Code)
	}
}
