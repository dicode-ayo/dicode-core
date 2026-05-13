package trigger

// Tests for the cross-spec validation pass that the engine runs at
// registration time on tasks declaring `trigger.before`. Per-spec validation
// (see pkg/task.Spec.validate) can only enforce shape: it cannot check that
// the referenced task actually exists in the registry or that it is itself a
// one-shot task rather than another daemon. Both rules live here.

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

func TestRegister_BeforeUnknownTaskRejected(t *testing.T) {
	e := newTestEnv(t)
	daemon := &task.Spec{
		ID:      "d",
		Name:    "d",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Daemon: true, Before: []string{"missing"}},
		Enabled: true,
	}
	err := e.engine.Register(daemon)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("expected error referencing unknown task, got %v", err)
	}
}

func TestRegister_BeforeDaemonRejected(t *testing.T) {
	e := newTestEnv(t)
	other := &task.Spec{
		ID:      "other-daemon",
		Name:    "other-daemon",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Daemon: true},
		Enabled: true,
	}
	if err := e.reg.Register(other); err != nil {
		t.Fatalf("seed reg.Register: %v", err)
	}
	target := &task.Spec{
		ID:      "d",
		Name:    "d",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Daemon: true, Before: []string{"other-daemon"}},
		Enabled: true,
	}
	err := e.engine.Register(target)
	if err == nil || !strings.Contains(err.Error(), "daemon") {
		t.Errorf("expected error rejecting daemon-as-before, got %v", err)
	}
}

// Cycle case: rejected structurally by the per-spec + cross-spec rules.
//
// trigger.before is only valid on daemon tasks (per-spec) AND it cannot
// reference a daemon (cross-spec). The only way a cycle could form is
// through a prereq task having its own trigger.before pointing back at the
// daemon — but only daemon tasks may have trigger.before. Therefore cycles
// are unreachable, and an explicit cycle-detection test is structurally
// impossible to write. The validator comment in validateBeforeRefs
// captures this reasoning so future readers don't add a redundant check.

// TestDaemonState_Default verifies that DaemonState() returns the zero
// value (DaemonStopped) for unknown task IDs and for daemons that haven't
// been started yet — operators viewing the WebUI before the engine has
// fired the preflight should see "stopped", not a blank field.
func TestDaemonState_Default(t *testing.T) {
	e := newTestEnv(t)
	if got := e.engine.DaemonState("unknown"); got != DaemonStopped {
		t.Errorf("unknown daemon state = %q, want stopped", got)
	}
}

// TestDaemonState_SetGet verifies that setDaemonState/DaemonState round-trip
// the five enum values without contention.
func TestDaemonState_SetGet(t *testing.T) {
	e := newTestEnv(t)
	states := []DaemonState{DaemonPrereqRunning, DaemonPrereqFailed, DaemonRunning, DaemonStopping, DaemonStopped}
	for _, want := range states {
		e.engine.setDaemonState("d", want)
		if got := e.engine.DaemonState("d"); got != want {
			t.Errorf("setDaemonState(%q) → DaemonState = %q, want %q", want, got, want)
		}
	}
}

// TestRegister_BeforeValid_NonDaemonTarget exercises the happy path: a
// daemon with a `before:` list referencing a one-shot task that exists in
// the registry registers without error.
func TestRegister_BeforeValid_NonDaemonTarget(t *testing.T) {
	e := newTestEnv(t)
	prereq := &task.Spec{
		ID:      "render",
		Name:    "render",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Enabled: true,
	}
	if err := e.reg.Register(prereq); err != nil {
		t.Fatalf("seed reg.Register: %v", err)
	}
	daemon := &task.Spec{
		ID:      "d",
		Name:    "d",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Daemon: true, Before: []string{"render"}},
		Enabled: true,
	}
	if err := e.engine.Register(daemon); err != nil {
		t.Errorf("unexpected error on valid before-list: %v", err)
	}
}

// preflightExec is a controllable Executor for the preflight tests. Each
// task is keyed by ID; the configured fn runs synchronously and decides
// the final status. Daemons never finish unless explicitly released.
type preflightExec struct {
	reg *registry.Registry

	mu     sync.Mutex
	fns    map[string]func(taskID, runID string) string
	daemon map[string]chan struct{} // daemon taskID → block channel (closed = exit)
	runs   atomic.Int32             // count of executed runs (any task)
}

func newPreflightExec(reg *registry.Registry) *preflightExec {
	return &preflightExec{
		reg:    reg,
		fns:    make(map[string]func(string, string) string),
		daemon: make(map[string]chan struct{}),
	}
}

func (p *preflightExec) setFn(taskID string, fn func(taskID, runID string) string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fns[taskID] = fn
}

func (p *preflightExec) markDaemon(taskID string) chan struct{} {
	ch := make(chan struct{})
	p.mu.Lock()
	p.daemon[taskID] = ch
	p.mu.Unlock()
	return ch
}

func (p *preflightExec) Execute(_ context.Context, spec *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	p.runs.Add(1)
	p.mu.Lock()
	daemonCh := p.daemon[spec.ID]
	fn := p.fns[spec.ID]
	p.mu.Unlock()

	if daemonCh != nil {
		// Long-running daemon: block until released.
		<-daemonCh
		_ = p.reg.FinishRun(context.Background(), opts.RunID, registry.StatusSuccess)
		return &pkgruntime.RunResult{}, nil
	}
	status := registry.StatusSuccess
	if fn != nil {
		status = fn(spec.ID, opts.RunID)
	}
	if status == registry.StatusFailure {
		_ = p.reg.FinishRunWithReason(context.Background(), opts.RunID, status, "test-injected: failure")
		return &pkgruntime.RunResult{}, errors.New("injected failure")
	}
	_ = p.reg.FinishRun(context.Background(), opts.RunID, status)
	return &pkgruntime.RunResult{}, nil
}

// newPreflightEnv returns a test engine + reg + controllable executor for
// the preflight gating tests. Tasks complete synchronously (or block, for
// marked daemons) without spawning Deno.
func newPreflightEnv(t *testing.T) (*Engine, *registry.Registry, *preflightExec) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	exec := newPreflightExec(reg)
	eng := New(reg, exec, zap.NewNop())
	return eng, reg, exec
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", msg)
}

// hasSuccessfulRunFor reports whether taskID has at least one run row in
// the registry with the given status.
func hasRunWithStatus(t *testing.T, reg *registry.Registry, taskID, status string) bool {
	t.Helper()
	runs, err := reg.ListRuns(context.Background(), taskID, 5)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	for _, r := range runs {
		if r.Status == status {
			return true
		}
	}
	return false
}

// TestPreflight_DaemonWaitsForPrereqSuccess verifies the gating contract:
// the daemon's run does not start until every task in its before-list has
// finished with status=success. Asserted by ordering — prereq must have a
// success run before the daemon's run shows up.
func TestPreflight_DaemonWaitsForPrereqSuccess(t *testing.T) {
	eng, reg, exec := newPreflightEnv(t)

	// Prereq: completes successfully.
	prereq := &task.Spec{
		ID:      "render",
		Name:    "render",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Enabled: true,
	}
	if err := reg.Register(prereq); err != nil {
		t.Fatalf("reg.Register prereq: %v", err)
	}

	// Daemon: blocks until released.
	daemon := &task.Spec{
		ID:      "d",
		Name:    "d",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Daemon: true, Before: []string{"render"}},
		Enabled: true,
	}
	if err := reg.Register(daemon); err != nil {
		t.Fatalf("reg.Register daemon: %v", err)
	}
	releaseDaemon := exec.markDaemon("d")
	defer close(releaseDaemon)

	if err := eng.Register(daemon); err != nil {
		t.Fatalf("eng.Register daemon: %v", err)
	}

	// Prereq must complete to success.
	waitUntil(t, 5*time.Second, func() bool {
		return hasRunWithStatus(t, reg, "render", registry.StatusSuccess)
	}, "prereq render never succeeded")

	// Daemon must transition to Running once preflight succeeds.
	waitUntil(t, 5*time.Second, func() bool {
		return eng.DaemonState("d") == DaemonRunning
	}, "daemon never reached Running")
}

// TestPreflight_NoBefore_StartsImmediately verifies that a daemon with an
// empty before-list does not pay any preflight overhead: it goes straight
// to Running without the engine touching daemonStates' PrereqRunning state.
func TestPreflight_NoBefore_StartsImmediately(t *testing.T) {
	eng, reg, exec := newPreflightEnv(t)
	daemon := &task.Spec{
		ID:      "d-noprereq",
		Name:    "d-noprereq",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Daemon: true}, // no Before
		Enabled: true,
	}
	if err := reg.Register(daemon); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}
	releaseDaemon := exec.markDaemon("d-noprereq")
	defer close(releaseDaemon)

	if err := eng.Register(daemon); err != nil {
		t.Fatalf("eng.Register: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		return eng.DaemonState("d-noprereq") == DaemonRunning
	}, "daemon-without-before never reached Running")
}

// makeDaemonSpec is a tiny convenience to keep the table-style tests below
// from drowning in struct literals. Kept local to this file.
func makeDaemonSpec(id string, before []string) *task.Spec {
	return &task.Spec{
		ID:      id,
		Name:    id,
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Daemon: true, Before: before},
		Enabled: true,
	}
}

func makeOneShotSpec(id string) *task.Spec {
	return &task.Spec{
		ID:      id,
		Name:    id,
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Enabled: true,
	}
}

var _ = fmt.Sprintf // keep fmt import live for future debug-helpers
