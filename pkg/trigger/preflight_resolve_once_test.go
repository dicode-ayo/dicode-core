package trigger

import (
	"context"
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

// fakeDenoRuntimeForChannel satisfies DenoRuntimeAPI, which (as of issue
// #719) is an empty marker interface Run only nil-checks before dispatch —
// it carries no methods since the secret-output channel now flows per-run
// through RunOptions.SecretOutputCh rather than being swapped onto the
// runtime. Kept as a distinct type (rather than reusing struct{}) so the
// SetDenoRuntime call site here reads the same as production wiring.
type fakeDenoRuntimeForChannel struct{}

// providerCountingExecutor counts how many times each task ID is dispatched.
// When the provider task is dispatched, it pushes the canned secret map onto
// opts.SecretOutputCh — the per-run channel the engine's ProviderRunner
// (Engine.Run) now passes directly via RunOptions (issue #719), mimicking
// the IPC-server-side behaviour of a real provider task's
// dicode.output(map, { secret: true }) call. Consumer task dispatches are
// recorded too — and the most recent opts.PreResolvedEnv is captured so the
// test can verify the engine threaded it through.
//
// Real runtimes mark the run finished via FinishRunWithResult called by dispatch;
// this mock just returns and lets dispatch handle it.
type providerCountingExecutor struct {
	providerID  string
	providerVal map[string]string

	providerCalls           atomic.Int64
	consumerCalls           atomic.Int64
	lastConsumerPreResolved atomic.Pointer[pkgruntime.RunOptions]
}

func (p *providerCountingExecutor) Execute(_ context.Context, spec *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	if spec.ID == p.providerID {
		p.providerCalls.Add(1)
		// Mimic the IPC-server-side behaviour: when the provider task calls
		// dicode.output(map, { secret: true }), the per-run secretOut
		// channel receives the map. The engine's ProviderRunner blocks on
		// that channel; we publish to it here so its WaitRun unblocks
		// cleanly.
		if opts.SecretOutputCh != nil {
			select {
			case opts.SecretOutputCh <- p.providerVal:
			default:
				// Channel was unbuffered or full — fall back to a goroutine
				// so we don't deadlock the executor goroutine.
				go func(c chan map[string]string) { c <- p.providerVal }(opts.SecretOutputCh)
			}
		}
		return &pkgruntime.RunResult{RunID: opts.RunID}, nil
	}
	// Consumer dispatch.
	p.consumerCalls.Add(1)
	captured := opts
	p.lastConsumerPreResolved.Store(&captured)
	return &pkgruntime.RunResult{RunID: opts.RunID}, nil
}

// TestPreflight_ProviderFiresOnceAcrossPreflightAndDispatch is the regression
// test for issue #235. Before the fix, env resolution ran twice per consumer
// launch (once in Engine.preflightEnv, once inside the runtime), so a fresh
// provider lookup spawned the provider task twice. After the fix the engine
// hands the *Resolved to dispatch via opts.PreResolvedEnv and the runtime
// reuses it instead of re-running the resolver.
//
// The contract this test pins:
//  1. provider task fires exactly once per consumer launch
//  2. consumer dispatch sees a non-nil opts.PreResolvedEnv carrying the
//     resolved values
func TestPreflight_ProviderFiresOnceAcrossPreflightAndDispatch(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)

	exec := &providerCountingExecutor{
		providerID:  "doppler",
		providerVal: map[string]string{"PG_URL": "postgres://x"},
	}

	eng := New(reg, exec, zap.NewNop())
	eng.SetSecrets(secrets.Chain{}) // non-nil so preflightEnv doesn't short-circuit
	eng.SetDenoRuntime(&fakeDenoRuntimeForChannel{})

	provider := &task.Spec{
		ID:       "doppler",
		Name:     "doppler",
		Runtime:  task.RuntimeDeno,
		Trigger:  task.TriggerConfig{Manual: true},
		Provider: &task.ProviderConfig{CacheTTL: 0},
		Timeout:  10 * time.Second,
	}
	if err := reg.Register(provider); err != nil {
		t.Fatalf("register provider: %v", err)
	}

	consumer := &task.Spec{
		ID:      "consumer",
		Name:    "consumer",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 10 * time.Second,
		Permissions: task.Permissions{
			Env: []task.EnvEntry{
				{Name: "PG_URL", From: "task:doppler"},
			},
		},
	}
	if err := reg.Register(consumer); err != nil {
		t.Fatalf("register consumer: %v", err)
	}

	// fireSync drives the full runTask flow synchronously: preflight + dispatch.
	runID, result, err := eng.fireSync(context.Background(), consumer, pkgruntime.RunOptions{}, "manual")
	if err != nil {
		t.Fatalf("fireSync: %v", err)
	}
	if result != nil && result.Error != nil {
		t.Fatalf("consumer run errored: %v", result.Error)
	}
	if runID == "" {
		t.Fatal("empty run ID")
	}

	if got := exec.providerCalls.Load(); got != 1 {
		t.Errorf("regression: provider must fire exactly once per consumer launch, got %d", got)
	}
	if got := exec.consumerCalls.Load(); got != 1 {
		t.Errorf("expected 1 consumer dispatch, got %d", got)
	}

	pre := exec.lastConsumerPreResolved.Load()
	if pre == nil {
		t.Fatal("consumer dispatch did not capture opts")
	}
	if pre.PreResolvedEnv == nil {
		t.Fatal("opts.PreResolvedEnv must be forwarded to dispatch (issue #235)")
	}
	if got := pre.PreResolvedEnv.Env["PG_URL"]; got != "postgres://x" {
		t.Errorf("PreResolvedEnv missing PG_URL value: got %q", got)
	}
}
