package trigger

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// suspendExec is a fake Executor that suspends on a first (non-resume) run and,
// on a resume run, records the seeded state/input and either succeeds or (when
// suspendAgain is set) suspends again to model a multi-step wizard.
type suspendExec struct {
	mu sync.Mutex

	// firstDeadline is the ResumeDeadline stamped on the initial suspend result
	// (0 = let the engine apply its 24h default).
	firstDeadline int64
	suspendAgain  bool

	resumeCalls     int
	seenResumeState []byte
	seenResumeInput []byte
}

func (s *suspendExec) Execute(_ context.Context, _ *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if opts.ResumeState == nil {
		return &pkgruntime.RunResult{
			RunID:          opts.RunID,
			Suspended:      true,
			ResumeState:    []byte(`{"step":"ask_name"}`),
			ResumeForm:     []byte(`{"fields":[{"name":"project_name"}]}`),
			ResumeDeadline: s.firstDeadline,
		}, nil
	}
	s.resumeCalls++
	s.seenResumeState = append([]byte(nil), opts.ResumeState...)
	s.seenResumeInput = append([]byte(nil), opts.ResumeInput...)
	if s.suspendAgain {
		return &pkgruntime.RunResult{
			RunID:       opts.RunID,
			Suspended:   true,
			ResumeState: []byte(`{"step":"ask_framework"}`),
			ResumeForm:  []byte(`{"fields":[{"name":"framework"}]}`),
		}, nil
	}
	return &pkgruntime.RunResult{RunID: opts.RunID, ReturnValue: "wizard-done"}, nil
}

func newSuspendEnv(t *testing.T, exec pkgruntime.Executor) (*Engine, *registry.Registry) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	eng := New(reg, exec, zap.NewNop())
	return eng, reg
}

func waitStatus(t *testing.T, reg *registry.Registry, runID, want string) *registry.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := reg.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if run.Status == want {
			return run
		}
		time.Sleep(5 * time.Millisecond)
	}
	run, _ := reg.GetRun(context.Background(), runID)
	t.Fatalf("run %s status = %q, want %q", runID, run.Status, want)
	return nil
}

func TestSuspend_PersistsRunWithTokenAndDeadline_NoChain(t *testing.T) {
	eng, reg := newSuspendEnv(t, &suspendExec{})

	specA := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}, Enabled: true}
	specB := &task.Spec{ID: "after", Name: "after", Runtime: task.RuntimeDeno, Enabled: true,
		Trigger: task.TriggerConfig{Chain: &task.ChainTrigger{From: "wiz", On: "success"}}}
	if err := reg.Register(specA); err != nil {
		t.Fatalf("register A: %v", err)
	}
	if err := reg.Register(specB); err != nil {
		t.Fatalf("register B: %v", err)
	}
	eng.Register(specB) // arm the chain

	before := time.Now().Add(23 * time.Hour).UnixMilli()
	runID, err := eng.FireManual(context.Background(), "wiz", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	run := waitStatus(t, reg, runID, registry.StatusSuspended)
	if run.ResumeToken == "" {
		t.Error("suspended run has no resume token")
	}
	if len(run.ResumeToken) != 64 {
		t.Errorf("resume token length = %d, want 64 hex chars", len(run.ResumeToken))
	}
	if run.ResumeDeadline < before {
		t.Errorf("resume deadline %d not defaulted to ~24h from now", run.ResumeDeadline)
	}
	if string(run.ResumeState) != `{"step":"ask_name"}` {
		t.Errorf("resume state = %q", run.ResumeState)
	}
	if run.FinishedAt != nil {
		t.Error("suspended run must not have finished_at set")
	}

	// The success chain must NOT fire for a suspended (non-terminal) run.
	time.Sleep(200 * time.Millisecond)
	if runs, _ := reg.ListRuns(context.Background(), "after", 5); len(runs) != 0 {
		t.Errorf("success chain fired for suspended run: %d runs", len(runs))
	}
}

func TestResumeRun_SeedsContinuationAndConsumesToken(t *testing.T) {
	exec := &suspendExec{}
	eng, reg := newSuspendEnv(t, exec)

	spec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}, Enabled: true}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	origID, err := eng.FireManual(context.Background(), "wiz", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	orig := waitStatus(t, reg, origID, registry.StatusSuspended)

	input := []byte(`{"project_name":"acme"}`)
	newID, err := eng.ResumeRun(context.Background(), orig.ResumeToken, input)
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}
	if newID == origID {
		t.Fatal("continuation run must have a fresh ID")
	}

	cont := waitStatus(t, reg, newID, registry.StatusSuccess)
	if cont.ParentRunID != origID {
		t.Errorf("continuation parent = %q, want %q", cont.ParentRunID, origID)
	}
	if cont.TriggerSource != registry.TriggerResume {
		t.Errorf("continuation trigger = %q, want resume", cont.TriggerSource)
	}

	// Original run consumed → resumed (terminal).
	after, _ := reg.GetRun(context.Background(), origID)
	if after.Status != registry.StatusResumed {
		t.Errorf("original status = %q, want resumed", after.Status)
	}

	// Continuation was seeded with the stored state and the caller's input.
	exec.mu.Lock()
	defer exec.mu.Unlock()
	if string(exec.seenResumeState) != `{"step":"ask_name"}` {
		t.Errorf("continuation resume_state = %q", exec.seenResumeState)
	}
	if string(exec.seenResumeInput) != string(input) {
		t.Errorf("continuation resume_input = %q, want %q", exec.seenResumeInput, input)
	}
}

func TestResumeRun_TokenIsSingleUse(t *testing.T) {
	eng, reg := newSuspendEnv(t, &suspendExec{})
	spec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}, Enabled: true}
	_ = reg.Register(spec)

	origID, err := eng.FireManual(context.Background(), "wiz", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	orig := waitStatus(t, reg, origID, registry.StatusSuspended)

	if _, err := eng.ResumeRun(context.Background(), orig.ResumeToken, nil); err != nil {
		t.Fatalf("first ResumeRun: %v", err)
	}
	// Replay of the same token must be rejected.
	if _, err := eng.ResumeRun(context.Background(), orig.ResumeToken, nil); !errors.Is(err, ErrResumeNotSuspended) {
		t.Errorf("second ResumeRun err = %v, want ErrResumeNotSuspended", err)
	}
}

func TestResumeRun_ExpiredTokenRejectedAndSwept(t *testing.T) {
	// Suspend with a deadline already in the past.
	eng, reg := newSuspendEnv(t, &suspendExec{firstDeadline: time.Now().Add(-time.Hour).UnixMilli()})
	spec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}, Enabled: true}
	_ = reg.Register(spec)

	origID, err := eng.FireManual(context.Background(), "wiz", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	orig := waitStatus(t, reg, origID, registry.StatusSuspended)

	if _, err := eng.ResumeRun(context.Background(), orig.ResumeToken, nil); !errors.Is(err, ErrResumeExpired) {
		t.Errorf("ResumeRun err = %v, want ErrResumeExpired", err)
	}
	// The expired run is swept to cancelled/resume_timeout as a side effect.
	after, _ := reg.GetRun(context.Background(), origID)
	if after.Status != registry.StatusCancelled {
		t.Errorf("expired run status = %q, want cancelled", after.Status)
	}
	if after.FailureReason != registry.ReasonResumeTimeout {
		t.Errorf("expired run reason = %q, want %q", after.FailureReason, registry.ReasonResumeTimeout)
	}
}

func TestResumeRun_DeregisteredTaskKeepsSuspensionResumable(t *testing.T) {
	exec := &suspendExec{}
	eng, reg := newSuspendEnv(t, exec)
	spec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}, Enabled: true}
	_ = reg.Register(spec)

	origID, err := eng.FireManual(context.Background(), "wiz", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	orig := waitStatus(t, reg, origID, registry.StatusSuspended)

	// Task disappears (deregistered / reloaded away) before the user resumes.
	reg.Unregister("wiz")

	if _, err := eng.ResumeRun(context.Background(), orig.ResumeToken, nil); err == nil {
		t.Fatal("expected error resuming a run whose task is gone")
	}
	// The token must NOT have been consumed: the run is still suspended and
	// keeps its token, so it stays resumable once the task is back.
	still, _ := reg.GetRun(context.Background(), origID)
	if still.Status != registry.StatusSuspended {
		t.Errorf("run status = %q, want suspended (token must not be consumed)", still.Status)
	}
	if still.ResumeToken != orig.ResumeToken {
		t.Errorf("resume token changed/cleared: %q", still.ResumeToken)
	}

	// Once the task is re-registered, the same token still resumes.
	if err := reg.Register(spec); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	newID, err := eng.ResumeRun(context.Background(), orig.ResumeToken, []byte(`{"project_name":"acme"}`))
	if err != nil {
		t.Fatalf("ResumeRun after re-register: %v", err)
	}
	cont := waitStatus(t, reg, newID, registry.StatusSuccess)
	if cont.ParentRunID != origID {
		t.Errorf("continuation parent = %q, want %q", cont.ParentRunID, origID)
	}
}

func TestResumeRun_ConcurrentSingleWinner(t *testing.T) {
	eng, reg := newSuspendEnv(t, &suspendExec{})
	spec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}, Enabled: true}
	_ = reg.Register(spec)

	origID, err := eng.FireManual(context.Background(), "wiz", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	orig := waitStatus(t, reg, origID, registry.StatusSuspended)

	const racers = 8
	var wg sync.WaitGroup
	var winners, notSuspended int32
	results := make(chan error, racers)
	wg.Add(racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			<-start
			_, rerr := eng.ResumeRun(context.Background(), orig.ResumeToken, nil)
			results <- rerr
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for rerr := range results {
		switch {
		case rerr == nil:
			atomic.AddInt32(&winners, 1)
		case errors.Is(rerr, ErrResumeNotSuspended):
			atomic.AddInt32(&notSuspended, 1)
		default:
			t.Errorf("unexpected ResumeRun error: %v", rerr)
		}
	}
	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1 continuation spawned", winners)
	}
	if notSuspended != racers-1 {
		t.Errorf("ErrResumeNotSuspended count = %d, want %d", notSuspended, racers-1)
	}

	after, _ := reg.GetRun(context.Background(), origID)
	if after.Status != registry.StatusResumed {
		t.Errorf("original status = %q, want resumed", after.Status)
	}
}

func TestResumeRun_UnknownToken(t *testing.T) {
	eng, _ := newSuspendEnv(t, &suspendExec{})
	if _, err := eng.ResumeRun(context.Background(), "does-not-exist", nil); !errors.Is(err, ErrResumeTokenNotFound) {
		t.Errorf("err = %v, want ErrResumeTokenNotFound", err)
	}
}

func TestResumeRun_MultiStepWizardChains(t *testing.T) {
	// The continuation suspends again, minting its own fresh token.
	exec := &suspendExec{suspendAgain: true}
	eng, reg := newSuspendEnv(t, exec)
	spec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}, Enabled: true}
	_ = reg.Register(spec)

	origID, _ := eng.FireManual(context.Background(), "wiz", nil)
	orig := waitStatus(t, reg, origID, registry.StatusSuspended)

	newID, err := eng.ResumeRun(context.Background(), orig.ResumeToken, []byte(`{"project_name":"acme"}`))
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}
	// Step 2 run suspends again with a distinct token.
	step2 := waitStatus(t, reg, newID, registry.StatusSuspended)
	if step2.ResumeToken == "" || step2.ResumeToken == orig.ResumeToken {
		t.Errorf("step2 token %q must be fresh (orig %q)", step2.ResumeToken, orig.ResumeToken)
	}
}

// TestSuspendedDaemonRun_NeutralForCrashloop verifies a daemon body that
// suspends is a neutral outcome: it does not flip the daemon to crashed/stopped
// and does not count as a crash-loop exit.
func TestSuspendedDaemonRun_NeutralForCrashloop(t *testing.T) {
	eng, reg := newSuspendEnv(t, &suspendExec{})
	spec := &task.Spec{ID: "wiz-daemon", Name: "wiz-daemon", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Daemon: true, Restart: "never"}, Enabled: true}

	// Seed a suspended run row for the daemon.
	ctx := context.Background()
	runID := "daemon-run-1"
	if _, err := reg.StartRunWithID(ctx, runID, spec.ID, "", string(registry.TriggerDaemon), registry.RunKindTask); err != nil {
		t.Fatalf("StartRunWithID: %v", err)
	}
	if err := reg.SuspendRun(ctx, runID, []byte(`{}`), nil, "tok-daemon", time.Now().UnixMilli(), 0); err != nil {
		t.Fatalf("SuspendRun: %v", err)
	}

	eng.daemonMu.Lock()
	eng.daemonSpecs[spec.ID] = spec
	eng.daemonRuns[spec.ID] = runID
	eng.daemonMu.Unlock()
	eng.setDaemonState(spec.ID, DaemonRunning)

	eng.onDaemonRunFinished(spec, runID)

	// With restart=never, a non-suspended non-success exit would flip state to
	// DaemonCrashed. A suspended run must leave the state untouched.
	if got := eng.daemonStates.get(spec.ID); got != DaemonRunning {
		t.Errorf("daemon state = %q, want running (suspended is neutral)", got)
	}
	if eng.IsCrashLooping(spec.ID) {
		t.Error("suspended daemon run must not count toward crash-loop")
	}
}
