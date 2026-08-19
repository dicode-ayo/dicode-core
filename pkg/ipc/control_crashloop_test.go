package ipc

// Crash-loop status derivation over the control socket (issue #458).
//
// A crash-looping daemon's latest run row is intermittently a transient
// "running" (the brief spawn-before-crash window), so cli.list / cli.status
// must consult the engine's crash-loop tracker and report "crashlooping"
// instead of the point-in-time snapshot. The engine side is covered in
// pkg/trigger/crashloop_test.go; these tests pin the ipc choke points.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// crashloopMockEngine is mockEngine plus the optional CrashloopReporter
// extension, mirroring how *trigger.Engine satisfies both.
type crashloopMockEngine struct {
	mockEngine
	looping map[string]bool
}

func (m *crashloopMockEngine) IsCrashLooping(taskID string) bool { return m.looping[taskID] }

// newCrashloopControlEnv builds a ControlServer over a real registry seeded
// with one daemon task whose latest run is still "running" — the masking
// snapshot from the issue report.
func newCrashloopControlEnv(t *testing.T, eng EngineRunner) (*ControlServer, string) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)

	spec := &task.Spec{
		ID:      "loopy",
		Name:    "loopy",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Daemon: true},
		Enabled: true,
	}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}
	// Latest run is in-flight: exactly the transient spawn window that must
	// not surface as the task's status while the daemon is crash-looping.
	runID, err := reg.StartRun(context.Background(), spec.ID, "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	dir := t.TempDir()
	cs, err := NewControlServer(
		filepath.Join(dir, "ctrl.sock"), filepath.Join(dir, "ctrl.token"),
		reg, eng, nil, MetricsProvider{}, "test", zap.NewNop(), nil, "", "",
	)
	if err != nil {
		t.Fatalf("NewControlServer: %v", err)
	}
	return cs, runID
}

func TestHandleList_CrashLoopingOverridesTransientRunning(t *testing.T) {
	eng := &crashloopMockEngine{looping: map[string]bool{"loopy": true}}
	cs, _ := newCrashloopControlEnv(t, eng)

	summaries, err := cs.handleList()
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("got %d summaries, want 1", len(summaries))
	}
	if got := summaries[0].LastStatus; got != StatusCrashLooping {
		t.Fatalf("LastStatus = %q, want %q (transient 'running' must never mask a crash loop)",
			got, StatusCrashLooping)
	}
}

func TestHandleList_NotCrashLooping_KeepsLastRunStatus(t *testing.T) {
	eng := &crashloopMockEngine{looping: map[string]bool{}}
	cs, _ := newCrashloopControlEnv(t, eng)

	summaries, err := cs.handleList()
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	if got := summaries[0].LastStatus; got != registry.StatusRunning {
		t.Fatalf("LastStatus = %q, want %q", got, registry.StatusRunning)
	}
}

// TestHandleList_EngineWithoutReporter_DegradesGracefully: a plain
// EngineRunner (no CrashloopReporter) must keep the old behaviour.
func TestHandleList_EngineWithoutReporter_DegradesGracefully(t *testing.T) {
	cs, _ := newCrashloopControlEnv(t, &mockEngine{})

	summaries, err := cs.handleList()
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	if got := summaries[0].LastStatus; got != registry.StatusRunning {
		t.Fatalf("LastStatus = %q, want %q", got, registry.StatusRunning)
	}
}

func TestHandleStatus_CrashLoopingOverridesTransientRunning(t *testing.T) {
	eng := &crashloopMockEngine{looping: map[string]bool{"loopy": true}}
	cs, runID := newCrashloopControlEnv(t, eng)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := cs.handleStatus(ctx, Request{TaskID: "loopy"})
	if err != nil {
		t.Fatalf("handleStatus: %v", err)
	}
	run, ok := res.(*registry.Run)
	if !ok {
		t.Fatalf("handleStatus result is %T, want *registry.Run", res)
	}
	if run.ID != runID {
		t.Fatalf("run.ID = %q, want %q", run.ID, runID)
	}
	if run.Status != StatusCrashLooping {
		t.Fatalf("run.Status = %q, want %q", run.Status, StatusCrashLooping)
	}
}

func TestHandleStatus_NotCrashLooping_KeepsRunStatus(t *testing.T) {
	eng := &crashloopMockEngine{looping: map[string]bool{}}
	cs, _ := newCrashloopControlEnv(t, eng)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := cs.handleStatus(ctx, Request{TaskID: "loopy"})
	if err != nil {
		t.Fatalf("handleStatus: %v", err)
	}
	run := res.(*registry.Run)
	if run.Status != registry.StatusRunning {
		t.Fatalf("run.Status = %q, want %q", run.Status, registry.StatusRunning)
	}
}

// TestHandleStatus_EngineWithoutReporter_DegradesGracefully mirrors the
// handleList degrade test: a plain EngineRunner (no CrashloopReporter) must
// keep the old behaviour on the status path too.
func TestHandleStatus_EngineWithoutReporter_DegradesGracefully(t *testing.T) {
	cs, _ := newCrashloopControlEnv(t, &mockEngine{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := cs.handleStatus(ctx, Request{TaskID: "loopy"})
	if err != nil {
		t.Fatalf("handleStatus: %v", err)
	}
	run := res.(*registry.Run)
	if run.Status != registry.StatusRunning {
		t.Fatalf("run.Status = %q, want %q", run.Status, registry.StatusRunning)
	}
}
