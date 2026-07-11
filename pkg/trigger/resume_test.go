package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/approval"
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

	resumeCalls      int
	seenResumeState  []byte
	seenResumeInput  []byte
	seenResumeParams map[string]string
	seenInput        any
}

func (s *suspendExec) Execute(_ context.Context, _ *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if opts.ResumeState == nil {
		return &pkgruntime.RunResult{
			RunID:          opts.RunID,
			Suspended:      true,
			ResumeState:    []byte(`{"step":"ask_name"}`),
			ResumeSchema:   []byte(`{"type":"object","properties":{"project_name":{"type":"string"}}}`),
			ResumeDeadline: s.firstDeadline,
		}, nil
	}
	s.resumeCalls++
	s.seenResumeState = append([]byte(nil), opts.ResumeState...)
	s.seenResumeInput = append([]byte(nil), opts.ResumeInput...)
	s.seenResumeParams = opts.Params
	s.seenInput = opts.Input
	if s.suspendAgain {
		return &pkgruntime.RunResult{
			RunID:        opts.RunID,
			Suspended:    true,
			ResumeState:  []byte(`{"step":"ask_framework"}`),
			ResumeSchema: []byte(`{"type":"object","properties":{"framework":{"type":"string"}}}`),
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

// TestResumeRun_ContinuationSharesRoot verifies #569: a resume continuation's
// root_run_id is the ORIGINAL suspended run's ID (which is its own root,
// since it has no parent), not the continuation's own ID. This is what lets
// the WebUI collapse a whole suspend/resume conversation into one group.
func TestResumeRun_ContinuationSharesRoot(t *testing.T) {
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
	if orig.RootRunID != origID {
		t.Fatalf("suspended run RootRunID = %q, want self %q", orig.RootRunID, origID)
	}

	newID, err := eng.ResumeRun(context.Background(), orig.ResumeToken, []byte(`{"project_name":"acme"}`))
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}
	cont := waitStatus(t, reg, newID, registry.StatusSuccess)
	if cont.RootRunID != origID {
		t.Errorf("continuation RootRunID = %q, want original run %q", cont.RootRunID, origID)
	}

	group, err := reg.ListRunGroup(context.Background(), origID, 50)
	if err != nil {
		t.Fatalf("ListRunGroup: %v", err)
	}
	if len(group) != 2 {
		t.Fatalf("len(group) = %d, want 2 (original + continuation): %+v", len(group), group)
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

func TestResumeRun_FireGuardPendingKeepsSuspensionResumable(t *testing.T) {
	exec := &suspendExec{}
	eng, reg := newSuspendEnv(t, exec)
	spec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}, Enabled: true}
	_ = reg.Register(spec)

	origID, err := eng.FireManual(context.Background(), "wiz", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	orig := waitStatus(t, reg, origID, registry.StatusSuspended)

	// The author edits the task, so the trust-on-change approval gate holds it
	// pending: the fire guard now vetoes any run of "wiz".
	pending := true
	eng.SetFireGuard(func(taskID string) error {
		if pending && taskID == "wiz" {
			return fmt.Errorf("%w: %s", approval.ErrPending, taskID)
		}
		return nil
	})

	_, err = eng.ResumeRun(context.Background(), orig.ResumeToken, nil)
	if !errors.Is(err, ErrResumePending) {
		t.Fatalf("ResumeRun err = %v, want ErrResumePending", err)
	}
	// The underlying guard veto is preserved for callers that inspect it.
	if !errors.Is(err, approval.ErrPending) {
		t.Errorf("ResumeRun err = %v, want wrapped approval.ErrPending", err)
	}

	// The token must NOT have been consumed: the run stays suspended and keeps
	// its token, so it remains resumable once the task is re-approved.
	still, _ := reg.GetRun(context.Background(), origID)
	if still.Status != registry.StatusSuspended {
		t.Errorf("run status = %q, want suspended (token must not be consumed)", still.Status)
	}
	if still.ResumeToken != orig.ResumeToken {
		t.Errorf("resume token changed/cleared: %q", still.ResumeToken)
	}

	// Re-approving the task (guard admits again) lets the SAME token resume.
	pending = false
	newID, err := eng.ResumeRun(context.Background(), orig.ResumeToken, []byte(`{"project_name":"acme"}`))
	if err != nil {
		t.Fatalf("ResumeRun after re-approval: %v", err)
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

// TestSuspendedDaemonRun_KeepsSlotAndReportsSuspended verifies that a daemon
// body which suspends (a) does not count as a crash-loop exit, (b) reports the
// distinct DaemonSuspended state instead of a stale "running", and (c) keeps
// its #470 run slot reserved so the "one body in flight" invariant holds across
// the suspended gap.
func TestSuspendedDaemonRun_KeepsSlotAndReportsSuspended(t *testing.T) {
	eng, reg := newSuspendEnv(t, &suspendExec{})
	spec := &task.Spec{ID: "wiz-daemon", Name: "wiz-daemon", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Daemon: true, Restart: "never"}, Enabled: true}

	// Seed a suspended run row for the daemon.
	ctx := context.Background()
	runID := "daemon-run-1"
	if _, err := reg.StartRunWithID(ctx, runID, spec.ID, "", string(registry.TriggerDaemon), registry.RunKindTask); err != nil {
		t.Fatalf("StartRunWithID: %v", err)
	}
	if _, err := reg.SuspendRun(ctx, runID, []byte(`{}`), nil, "tok-daemon", time.Now().UnixMilli(), 0, nil); err != nil {
		t.Fatalf("SuspendRun: %v", err)
	}

	eng.daemonMu.Lock()
	eng.daemonSpecs[spec.ID] = spec
	eng.daemonRuns[spec.ID] = runID
	eng.daemonMu.Unlock()
	eng.setDaemonState(spec.ID, DaemonRunning)

	eng.onDaemonRunFinished(spec, runID)

	// State reflects reality: awaiting input, not the stale "running" (and not
	// DaemonCrashed, which a non-suspended non-success exit under restart=never
	// would produce).
	if got := eng.daemonStates.get(spec.ID); got != DaemonSuspended {
		t.Errorf("daemon state = %q, want suspended", got)
	}
	// The slot stays reserved: a reconciler reload's registerDaemon must see the
	// daemon as still in flight, and a resume must find the slot parked here.
	eng.daemonMu.Lock()
	slot, ok := eng.daemonRuns[spec.ID]
	eng.daemonMu.Unlock()
	if !ok || slot != runID {
		t.Errorf("daemon slot = %q (present=%v), want it kept at %q across the suspended gap", slot, ok, runID)
	}
	if eng.IsCrashLooping(spec.ID) {
		t.Error("suspended daemon run must not count toward crash-loop")
	}
}

// TestResumeRun_PreservesFireTimeParams pins #502: a run fired with a param
// override that suspends must resume with the SAME override, not the spec
// defaults. Without preservation the continuation's opts.Params is nil and
// MergeParams(spec.Params, nil) silently reverts ctx.params mid-wizard.
func TestResumeRun_PreservesFireTimeParams(t *testing.T) {
	exec := &suspendExec{}
	eng, reg := newSuspendEnv(t, exec)
	spec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno,
		Params:  []task.Param{{Name: "project_name", Default: "spec-default"}},
		Trigger: task.TriggerConfig{Manual: true}, Enabled: true}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	override := map[string]string{"project_name": "user-choice"}
	origID, err := eng.FireManual(context.Background(), "wiz", override)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	orig := waitStatus(t, reg, origID, registry.StatusSuspended)
	if len(orig.ResumeParams) == 0 {
		t.Fatal("suspended run did not persist fire-time params")
	}

	newID, err := eng.ResumeRun(context.Background(), orig.ResumeToken, []byte(`{}`))
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}
	waitStatus(t, reg, newID, registry.StatusSuccess)

	exec.mu.Lock()
	defer exec.mu.Unlock()
	if got := exec.seenResumeParams["project_name"]; got != "user-choice" {
		t.Errorf("continuation param project_name = %q, want %q (reverted to spec default?)", got, "user-choice")
	}
}

// TestResumeRun_PreservesChainDepth pins #502: the chain-depth ceiling must
// survive a suspend hop. A run fired at depth 3 that suspends and resumes into
// a continuation which suspends again must carry depth 3 forward — if the hop
// reset it, the persisted depth of the second suspension would be 0.
func TestResumeRun_PreservesChainDepth(t *testing.T) {
	exec := &suspendExec{suspendAgain: true}
	eng, reg := newSuspendEnv(t, exec)
	spec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Enabled: true}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	origID, err := eng.fireAsync(context.Background(), spec,
		pkgruntime.RunOptions{Input: map[string]any{"_chain_depth": 3}}, registry.TriggerChain)
	if err != nil {
		t.Fatalf("fireAsync: %v", err)
	}
	orig := waitStatus(t, reg, origID, registry.StatusSuspended)
	if got := decodeCarryDepth(t, orig.ResumeParams); got != 3 {
		t.Fatalf("original suspension chain depth = %d, want 3", got)
	}

	newID, err := eng.ResumeRun(context.Background(), orig.ResumeToken, []byte(`{"project_name":"acme"}`))
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}
	step2 := waitStatus(t, reg, newID, registry.StatusSuspended)
	if got := decodeCarryDepth(t, step2.ResumeParams); got != 3 {
		t.Errorf("chain depth after suspend hop = %d, want 3 (ceiling reset?)", got)
	}
}

func decodeCarryDepth(t *testing.T, blob []byte) int {
	t.Helper()
	if len(blob) == 0 {
		t.Fatal("resume_params blob is empty")
	}
	var c resumeCarry
	if err := json.Unmarshal(blob, &c); err != nil {
		t.Fatalf("decode resume_params: %v", err)
	}
	return c.ChainDepth
}

// TestSuspendedDaemon_ReloadFencesStaleResume is the double-start regression
// (#502 item 2 / #470). A daemon body suspends; a reconciler content reload
// (eng.Register) tears the daemon down and restarts a fresh body, re-pointing
// the run slot. Resuming the now-stale pre-reload suspension must be fenced off
// — otherwise its continuation would run as a SECOND concurrent body next to
// the reloaded one. The interlock is resumeDaemonBody's slot compare-and-swap:
// the slot no longer points at the suspended run, so no continuation spawns.
func TestSuspendedDaemon_ReloadFencesStaleResume(t *testing.T) {
	eng, reg := newSuspendEnv(t, &suspendExec{})
	spec := &task.Spec{ID: "wiz-daemon", Name: "wiz-daemon", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Daemon: true, Restart: "never"}, Enabled: true}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}

	// Bring the daemon up; its body suspends immediately.
	if err := eng.Register(spec); err != nil {
		t.Fatalf("eng.Register: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		return eng.DaemonState(spec.ID) == DaemonSuspended
	}, "daemon never parked in DaemonSuspended")

	eng.daemonMu.Lock()
	firstRun, ok := eng.daemonRuns[spec.ID]
	eng.daemonMu.Unlock()
	if !ok {
		t.Fatal("suspended daemon dropped its run slot")
	}
	orig, err := reg.GetRun(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	staleToken := orig.ResumeToken

	// Reconciler content reload: eng.Register tears down + restarts. The fresh
	// body re-points the slot (reserved before the body fires, #470 race 1), so
	// the pre-reload suspension is now stale.
	if err := eng.Register(spec); err != nil {
		t.Fatalf("reload eng.Register: %v", err)
	}
	eng.daemonMu.Lock()
	freshRun, ok := eng.daemonRuns[spec.ID]
	eng.daemonMu.Unlock()
	if !ok || freshRun == firstRun {
		t.Fatalf("reload did not start a fresh body: slot=%q present=%v (pre-reload %q)", freshRun, ok, firstRun)
	}

	// Resuming the STALE suspension must fail — it must not start a second body.
	if _, err := eng.ResumeRun(context.Background(), staleToken, nil); err == nil {
		t.Fatal("stale daemon resume after reload must fail — it would start a second concurrent body")
	}

	// The slot still belongs to the fresh body; no continuation adopted it.
	eng.daemonMu.Lock()
	slot := eng.daemonRuns[spec.ID]
	eng.daemonMu.Unlock()
	if slot != freshRun {
		t.Fatalf("slot = %q after the fenced resume, want the fresh body %q left untouched", slot, freshRun)
	}
}

// blockingResumeDaemonExec suspends the first (non-resume) run and blocks the
// resume continuation inside Execute until release is closed, so a test can
// observe engine state while the continuation is provably in flight.
type blockingResumeDaemonExec struct {
	started    chan string // continuation run ID, sent once its body starts
	release    chan struct{}
	firstRuns  atomic.Int32
	resumeRuns atomic.Int32
}

func (b *blockingResumeDaemonExec) Execute(_ context.Context, _ *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	if opts.ResumeState == nil {
		b.firstRuns.Add(1)
		return &pkgruntime.RunResult{
			RunID:        opts.RunID,
			Suspended:    true,
			ResumeState:  []byte(`{"step":"one"}`),
			ResumeSchema: []byte(`{}`),
		}, nil
	}
	b.resumeRuns.Add(1)
	b.started <- opts.RunID
	<-b.release
	return &pkgruntime.RunResult{RunID: opts.RunID, ReturnValue: "done"}, nil
}

// TestResumeRun_DaemonContinuation_KeepsOneBodyInFlight verifies the resume half
// of the #470 invariant: a suspended daemon body's continuation adopts the run
// slot (so it participates in "one body in flight") and the state reflects a
// live body again — asserted while the continuation is deterministically parked
// mid-execution.
func TestResumeRun_DaemonContinuation_KeepsOneBodyInFlight(t *testing.T) {
	exec := &blockingResumeDaemonExec{started: make(chan string, 1), release: make(chan struct{})}
	eng, reg := newSuspendEnv(t, exec)
	spec := &task.Spec{ID: "wiz-daemon", Name: "wiz-daemon", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Daemon: true, Restart: "never"}, Enabled: true}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}

	if err := eng.Register(spec); err != nil {
		t.Fatalf("eng.Register: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		return eng.DaemonState(spec.ID) == DaemonSuspended
	}, "daemon never parked in DaemonSuspended")

	eng.daemonMu.Lock()
	firstRun := eng.daemonRuns[spec.ID]
	eng.daemonMu.Unlock()
	orig, err := reg.GetRun(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	contID, err := eng.ResumeRun(context.Background(), orig.ResumeToken, nil)
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}
	if contID == firstRun {
		t.Fatal("continuation must have a fresh run ID")
	}

	// The continuation body is now in flight (blocked in Execute).
	if started := <-exec.started; started != contID {
		t.Fatalf("continuation body run %q != returned continuation ID %q", started, contID)
	}

	// It adopted the daemon slot, and the state reflects a live body again.
	eng.daemonMu.Lock()
	slot := eng.daemonRuns[spec.ID]
	eng.daemonMu.Unlock()
	if slot != contID {
		t.Fatalf("continuation did not adopt the slot: slot=%q, want %q", slot, contID)
	}
	if got := eng.DaemonState(spec.ID); got != DaemonRunning {
		t.Fatalf("state = %q while continuation in flight, want running", got)
	}

	// Release; the continuation completes and frees the slot.
	close(exec.release)
	waitStatus(t, reg, contID, registry.StatusSuccess)
	waitUntil(t, 5*time.Second, func() bool {
		eng.daemonMu.Lock()
		defer eng.daemonMu.Unlock()
		_, reserved := eng.daemonRuns[spec.ID]
		return !reserved
	}, "continuation finished but the daemon slot leaked")
	if got := exec.resumeRuns.Load(); got != 1 {
		t.Fatalf("continuation bodies = %d, want exactly 1", got)
	}
}
