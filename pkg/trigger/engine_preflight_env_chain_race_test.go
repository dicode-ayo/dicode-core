package trigger

import (
	"context"
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

// chainTargetCountingExecutor counts how many times each task ID is dispatched.
// The on_failure_chain target dispatches through fireAsync, which hands the
// run to its Executor; counting Execute calls per task ID lets the test
// assert whether the on_failure_chain edge fired at all.
type chainTargetCountingExecutor struct {
	calls sync.Map // taskID -> *atomic.Int64
}

func (c *chainTargetCountingExecutor) Execute(_ context.Context, spec *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	v, _ := c.calls.LoadOrStore(spec.ID, &atomic.Int64{})
	v.(*atomic.Int64).Add(1)
	return &pkgruntime.RunResult{RunID: opts.RunID}, nil
}

func (c *chainTargetCountingExecutor) count(taskID string) int64 {
	v, ok := c.calls.Load(taskID)
	if !ok {
		return 0
	}
	return v.(*atomic.Int64).Load()
}

// TestRunTask_PreflightEnvFailure_ChainDepthPreserved pins the fix for #334:
// a parent run whose preflight-env resolution fails dispatches its
// on_failure_chain synchronously, so the chain consumer reads the parent's
// actual runChainDepth (not 0 post-cleanup, which would allow one hop past
// MaxDepth).
//
// Fixture: parent task `consumer` has permissions.env that references a
// missing provider task — preflightEnv returns ErrProviderUnavailable, so
// runTask hits the preflight short-circuit at engine.go:~2285. We fire the
// parent via fireAsync with Input{_chain_depth: 2}, matching what FireChain
// stamps on a chain-fired hop at the MaxDepth ceiling. After preflight
// fails, FireChain must observe depth=2 and suppress the on_failure_chain
// edge (nextDepth=3 > maxDepth=2).
//
// Race window the fix closes: the previous async `go FireChain(...)` could
// be scheduled after startRun's deferred cleanup() ran
// `runChainDepth.Delete(opts.RunID)`. FireChain would then read depth=0,
// compute nextDepth=1, and fire the fallback target — one hop past the
// ceiling. The synchronous call guarantees FireChain reads the depth
// before cleanup deletes it.
func TestRunTask_PreflightEnvFailure_ChainDepthPreserved(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)

	exec := &chainTargetCountingExecutor{}
	eng := New(reg, exec, zap.NewNop())
	// Non-nil chain so preflightEnv does not short-circuit on
	// `e.secrets == nil`. The empty chain is fine: the test path uses
	// from: task:<id> which never invokes chain.Resolve.
	eng.SetSecrets(secrets.Chain{})

	// Fallback target for the parent's on_failure_chain edge. If the
	// race fires (fix absent), this task records a dispatch.
	fallback := &task.Spec{
		ID:      "fallback",
		Name:    "fallback",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 10 * time.Second,
		Enabled: true,
	}
	if err := reg.Register(fallback); err != nil {
		t.Fatalf("register fallback: %v", err)
	}

	// Parent task: env references an unregistered provider, so
	// preflightEnv returns a typed ErrProviderUnavailable and runTask
	// hits the preflight short-circuit. on_failure_chain points at
	// fallback so the suppression assertion is meaningful.
	consumer := &task.Spec{
		ID:      "consumer",
		Name:    "consumer",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 10 * time.Second,
		Enabled: true,
		Permissions: task.Permissions{
			Env: []task.EnvEntry{
				{Name: "API_KEY", From: "task:missing-provider"},
			},
		},
		OnFailureChain: &task.OnFailureChainSpec{
			Task: "fallback",
			// Default max_depth = 2; leave unset.
		},
	}
	if err := reg.Register(consumer); err != nil {
		t.Fatalf("register consumer: %v", err)
	}

	// Drive the parent through fireAsync with Input{_chain_depth: 2}.
	// fireAsync stores 2 in runChainDepth[parent.RunID] before the run
	// goroutine starts (engine.go:~2417). When runTask's preflight-env
	// short-circuit fires FireChain, the synchronous call must see
	// depth=2 → nextDepth=3 > maxDepth=2 → suppress.
	runID, err := eng.fireAsync(context.Background(), consumer,
		pkgruntime.RunOptions{
			Input: map[string]any{"_chain_depth": 2},
		}, registry.TriggerManual)
	if err != nil {
		t.Fatalf("fireAsync: %v", err)
	}

	// Wait for the parent run to reach a terminal state. WaitRun blocks
	// on the runDone channel, which is closed only after cleanup() — by
	// which time the synchronous FireChain has already run.
	if _, err := eng.WaitRun(context.Background(), runID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	// The parent must have terminated as failed (preflight short-circuit
	// recorded preStatus=failure with provider_unavailable reason).
	run, err := reg.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != registry.StatusFailure {
		t.Errorf("parent run status = %q, want %q (preflightEnv should have failed)", run.Status, registry.StatusFailure)
	}

	// Give any errant async chain dispatch a window to slip through —
	// the fix's contract is that FireChain ran synchronously *before*
	// runTask returned, so this sleep cannot mask a true regression
	// (the fallback Execute would already have been recorded).
	time.Sleep(200 * time.Millisecond)

	if got := exec.count("fallback"); got != 0 {
		t.Errorf("on_failure_chain must be suppressed at depth=%d > max_depth=2; fallback fired %d times (#334)",
			3, got)
	}
}
