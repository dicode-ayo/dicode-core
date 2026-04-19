package trigger

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// mockSecretsProvider is a simple in-memory implementation of secrets.Provider
// that lets tests flip secret presence at runtime (e.g. simulate a prereq task
// populating the store by calling set() from its Execute hook).
type mockSecretsProvider struct {
	mu   sync.Mutex
	data map[string]string
}

func newMockSecrets(initial map[string]string) *mockSecretsProvider {
	cp := make(map[string]string, len(initial))
	for k, v := range initial {
		cp[k] = v
	}
	return &mockSecretsProvider{data: cp}
}

func (m *mockSecretsProvider) Name() string { return "mock" }

func (m *mockSecretsProvider) Get(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data[key], nil
}

func (m *mockSecretsProvider) set(key, val string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = val
}

// mockExecutor is a task.Runtime-agnostic Executor stand-in. Tests configure
// `run` to mirror the behavior of a real prereq — populate a mock secret,
// throw an error, or just succeed — without requiring a Deno/Docker runtime.
type mockExecutor struct {
	runs atomic.Int64
	run  func(ctx context.Context) *pkgruntime.RunResult
}

func (m *mockExecutor) Execute(ctx context.Context, _ *task.Spec, _ pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	m.runs.Add(1)
	if m.run == nil {
		return &pkgruntime.RunResult{}, nil
	}
	return m.run(ctx), nil
}

type resolveEnv struct {
	eng     *Engine
	reg     *registry.Registry
	secrets *mockSecretsProvider
	exec    *mockExecutor
	cleanup func()
}

func newResolveEnv(t *testing.T) *resolveEnv {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	reg := registry.New(d)
	exec := &mockExecutor{}
	eng := New(reg, exec, zap.NewNop())
	// Register under the real deno runtime key so prereq specs dispatch here.
	// resolveIfMissing doesn't care about the runtime name; dispatch does,
	// and mock specs below use task.RuntimeDeno.
	mock := newMockSecrets(nil)
	eng.SetSecrets(secrets.Chain{mock})
	t.Cleanup(func() { d.Close() })
	return &resolveEnv{eng: eng, reg: reg, secrets: mock, exec: exec, cleanup: func() { d.Close() }}
}

// registerPrereq creates a minimal spec the engine can dispatch via fireSync.
func (r *resolveEnv) registerPrereq(t *testing.T, id string) *task.Spec {
	t.Helper()
	spec := &task.Spec{
		ID:      id,
		Name:    id,
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 10 * time.Second,
	}
	if err := r.reg.Register(spec); err != nil {
		t.Fatalf("register prereq %q: %v", id, err)
	}
	return spec
}

// parentSpec returns a spec whose env entry declares an if_missing directive.
// No registration needed — resolveIfMissing works off the spec passed to it.
func parentSpec(secretName, prereqID string) *task.Spec {
	return &task.Spec{
		ID:      "parent",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Webhook: "/hooks/parent"},
		Permissions: task.Permissions{
			Env: []task.EnvEntry{
				{
					Name:   secretName,
					Secret: secretName,
					IfMissing: &task.IfMissing{
						Task: prereqID,
					},
				},
			},
		},
	}
}

// startParentRun creates a run row the engine can attribute prereq logs to.
func (r *resolveEnv) startParentRun(t *testing.T, ctx context.Context) string {
	t.Helper()
	runID, err := r.reg.StartRun(ctx, "parent", registry.StatusRunning)
	if err != nil {
		t.Fatalf("start parent run: %v", err)
	}
	return runID
}

// ── Tests ────────────────────────────────────────────────────────────────

func TestResolveIfMissing_SecretPresent_SkipsPrereq(t *testing.T) {
	env := newResolveEnv(t)
	env.secrets.set("OPENROUTER_API_KEY", "already-here")

	spec := parentSpec("OPENROUTER_API_KEY", "auth/openrouter-oauth")
	env.registerPrereq(t, "auth/openrouter-oauth")

	if err := env.eng.resolveIfMissing(context.Background(), spec, ""); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if env.exec.runs.Load() != 0 {
		t.Errorf("prereq should not have fired, but ran %d time(s)", env.exec.runs.Load())
	}
}

func TestResolveIfMissing_NoSecretsChain_NoOp(t *testing.T) {
	env := newResolveEnv(t)
	env.eng.secrets = nil // simulate "daemon didn't call SetSecrets"

	spec := parentSpec("X", "some-task")
	if err := env.eng.resolveIfMissing(context.Background(), spec, ""); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if env.exec.runs.Load() != 0 {
		t.Errorf("prereq should not fire when secrets chain is nil, ran %d", env.exec.runs.Load())
	}
}

func TestResolveIfMissing_PrereqTaskNotRegistered(t *testing.T) {
	env := newResolveEnv(t)
	spec := parentSpec("X", "auth/does-not-exist")

	err := env.eng.resolveIfMissing(context.Background(), spec, "")
	if err == nil {
		t.Fatal("expected error for unregistered prereq")
	}
	if !strings.Contains(err.Error(), "is not registered") {
		t.Errorf("error message should mention registration: %v", err)
	}
}

func TestResolveIfMissing_PrereqSucceeds_SecretNowPresent(t *testing.T) {
	env := newResolveEnv(t)
	env.registerPrereq(t, "auth/openrouter-oauth")
	// Simulate the prereq populating the secret as a side-effect.
	env.exec.run = func(context.Context) *pkgruntime.RunResult {
		env.secrets.set("OPENROUTER_API_KEY", "got-it")
		return &pkgruntime.RunResult{}
	}

	spec := parentSpec("OPENROUTER_API_KEY", "auth/openrouter-oauth")
	runID := env.startParentRun(t, context.Background())

	if err := env.eng.resolveIfMissing(context.Background(), spec, runID); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if env.exec.runs.Load() != 1 {
		t.Errorf("expected 1 prereq run, got %d", env.exec.runs.Load())
	}
}

func TestResolveIfMissing_PrereqSucceedsButSecretStillUnset(t *testing.T) {
	env := newResolveEnv(t)
	env.registerPrereq(t, "auth/openrouter-oauth")
	// Prereq "succeeded" but forgot to persist the secret — common bug mode
	// we want to surface clearly instead of silently skipping the parent.
	env.exec.run = func(context.Context) *pkgruntime.RunResult {
		return &pkgruntime.RunResult{}
	}

	spec := parentSpec("OPENROUTER_API_KEY", "auth/openrouter-oauth")
	runID := env.startParentRun(t, context.Background())

	err := env.eng.resolveIfMissing(context.Background(), spec, runID)
	if err == nil {
		t.Fatal("expected error when secret still missing after prereq")
	}
	// Stable error format — the UI's detectSetup depends on this exact prefix.
	// See chat.js::detectSetup for the consumer regex.
	if !strings.Contains(err.Error(), `if_missing: secret "OPENROUTER_API_KEY" requires setup via task "auth/openrouter-oauth"`) {
		t.Errorf("error must use stable if_missing format, got: %v", err)
	}
	if !strings.Contains(err.Error(), "ran but still unset") {
		t.Errorf("expected 'ran but still unset' marker for this branch, got: %v", err)
	}
}

func TestResolveIfMissing_PrereqFails_StableErrorFormat(t *testing.T) {
	env := newResolveEnv(t)
	env.registerPrereq(t, "auth/openrouter-oauth")
	prereqErr := errors.New("Open this URL to authorize: https://example.test/auth")
	env.exec.run = func(context.Context) *pkgruntime.RunResult {
		return &pkgruntime.RunResult{Error: prereqErr}
	}

	spec := parentSpec("OPENROUTER_API_KEY", "auth/openrouter-oauth")
	runID := env.startParentRun(t, context.Background())

	err := env.eng.resolveIfMissing(context.Background(), spec, runID)
	if err == nil {
		t.Fatal("expected error when prereq fails")
	}
	// Stable prefix — UI consumers regex-match on this.
	wantPrefix := `if_missing: secret "OPENROUTER_API_KEY" requires setup via task "auth/openrouter-oauth"`
	if !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Errorf("error must start with stable prefix %q, got: %v", wantPrefix, err)
	}
	// The prereq's error — typically the authorize URL — must be wrapped
	// through so the chat UI can regex it out of the logs.
	if !errors.Is(err, prereqErr) {
		t.Errorf("wrapped prereq error must be discoverable via errors.Is")
	}
}

func TestResolveIfMissing_Singleflight_CollapsesConcurrentCalls(t *testing.T) {
	env := newResolveEnv(t)
	env.registerPrereq(t, "auth/openrouter-oauth")

	// Block the prereq long enough that all goroutines pile up behind the
	// same singleflight entry; release once they're parked, then observe
	// that exec.runs only incremented once.
	release := make(chan struct{})
	env.exec.run = func(ctx context.Context) *pkgruntime.RunResult {
		<-release
		env.secrets.set("OPENROUTER_API_KEY", "filled-after-wait")
		return &pkgruntime.RunResult{}
	}

	spec := parentSpec("OPENROUTER_API_KEY", "auth/openrouter-oauth")

	const callers = 5
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each caller gets its own parent run ID; they all collapse to
			// the same in-flight prereq regardless.
			runID, _ := env.reg.StartRun(context.Background(), "parent", registry.StatusRunning)
			errs <- env.eng.resolveIfMissing(context.Background(), spec, runID)
		}()
	}
	// Give goroutines a moment to reach the singleflight call.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	}
	if got := env.exec.runs.Load(); got != 1 {
		t.Errorf("singleflight should have collapsed %d callers to 1 prereq run, got %d", callers, got)
	}
}
