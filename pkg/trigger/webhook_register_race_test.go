package trigger

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

type immediateExec struct{}

func (immediateExec) Execute(_ context.Context, _ *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	return &pkgruntime.RunResult{RunID: opts.RunID}, nil
}

// raceEnv builds a minimal Engine + Registry backed by an in-memory sqlite DB,
// wired to immediateExec so no real subprocess is ever launched. Shared by the
// webhook and cron register-race test files (pkg/trigger/cron_register_race_test.go)
// so the two don't maintain near-identical copies of the same setup.
func raceEnv(t *testing.T) (*Engine, *registry.Registry, db.DB) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	eng := New(reg, immediateExec{}, zap.NewNop())
	eng.RegisterExecutor(task.RuntimeDocker, immediateExec{})
	eng.SetDB(d)
	return eng, reg, d
}

// raceSpec builds a minimal task.Spec for the register-race tests, wired to
// the docker runtime + raceEnv's immediateExec so no real subprocess is ever
// launched. Shared by cronSpec and webhookSpec, which differ only in which
// trigger shape they set.
func raceSpec(id string, enabled bool, trigger task.TriggerConfig) *task.Spec {
	return &task.Spec{
		ID: id, Name: id, Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: trigger,
		Enabled: enabled,
	}
}

func webhookSpec(id, path string, enabled bool) *task.Spec {
	return raceSpec(id, enabled, task.TriggerConfig{Webhook: path})
}

// The invariant behind the fix: re-registering a task must never take its own
// webhook path out of the routing map, even transiently. Registration is
// serialised by registerMu but lookups only take e.mu, so a path deleted here
// and re-added a moment later is a live 404 for any request in between.
func TestUnregisterTriggersKeeping_RetainsReclaimedPath(t *testing.T) {
	eng, _, _ := raceEnv(t)
	spec := webhookSpec("keep", "/hooks/keep", true)
	if err := eng.Register(spec); err != nil {
		t.Fatalf("Register: %v", err)
	}

	eng.unregisterTriggersKeeping(spec.ID, spec.Trigger.Webhook, "")

	eng.mu.Lock()
	got, ok := eng.webhooks["/hooks/keep"]
	eng.mu.Unlock()
	if !ok || got != "keep" {
		t.Fatalf("webhooks[/hooks/keep] = %q,%v; the re-claimed path must never be removed", got, ok)
	}
}

// The complement: a path that is NOT being re-claimed is still torn down, so a
// renamed, disabled, or removed task frees its route.
func TestUnregisterTriggersKeeping_DropsUnclaimedPath(t *testing.T) {
	eng, _, _ := raceEnv(t)
	spec := webhookSpec("drop", "/hooks/old", true)
	if err := eng.Register(spec); err != nil {
		t.Fatalf("Register: %v", err)
	}

	eng.unregisterTriggersKeeping(spec.ID, "/hooks/new", "")

	eng.mu.Lock()
	_, stillThere := eng.webhooks["/hooks/old"]
	eng.mu.Unlock()
	if stillThere {
		t.Fatal("webhooks[/hooks/old] survived; a path not being re-claimed must be dropped")
	}
}

// A disabled task arms no triggers, so re-registering it must free the route
// rather than keep it (Register passes keep="" when !Enabled).
func TestRegister_DisabledTaskReleasesWebhookPath(t *testing.T) {
	eng, _, _ := raceEnv(t)
	if err := eng.Register(webhookSpec("toggle", "/hooks/toggle", true)); err != nil {
		t.Fatalf("Register enabled: %v", err)
	}
	if err := eng.Register(webhookSpec("toggle", "/hooks/toggle", false)); err != nil {
		t.Fatalf("Register disabled: %v", err)
	}

	eng.mu.Lock()
	_, stillThere := eng.webhooks["/hooks/toggle"]
	eng.mu.Unlock()
	if stillThere {
		t.Fatal("a disabled task kept its webhook route")
	}
}

// End-to-end: hammer the endpoint while the task is re-registered underneath it.
// Every fire must be served. Before the fix this 404s intermittently, which is
// what made TestShutdownDrainsInFlightSyncWebhookRun flake — its body never ran.
func TestWebhookNeverFourOhFoursDuringReRegister(t *testing.T) {
	eng, reg, _ := raceEnv(t)
	spec := webhookSpec("hot", "/hooks/hot", true)
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}
	if err := eng.Register(spec); err != nil {
		t.Fatalf("eng.Register: %v", err)
	}

	handler := eng.WebhookHandler()
	var wg sync.WaitGroup
	var mu sync.Mutex
	notFound := 0

	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				eng.Register(spec) //nolint:errcheck
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			req := httptest.NewRequest(http.MethodPost, "/hooks/hot", nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code == http.StatusNotFound {
				mu.Lock()
				notFound++
				mu.Unlock()
			}
		}
		close(stop)
	}()
	wg.Wait()

	if notFound > 0 {
		t.Errorf("%d/300 fires 404'd while the task stayed registered throughout", notFound)
	}
}
