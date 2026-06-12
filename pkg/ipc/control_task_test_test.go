package ipc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// Tests for the cli.task.test control-socket handler. Exercises the
// method-dispatch path in isolation without going over the socket —
// existing handler tests (control_test.go) cover the socket plumbing.

func newControlServerForTaskTest(t *testing.T) (*ControlServer, *registry.Registry) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	reg := registry.New(d)
	cs := &ControlServer{reg: reg, log: zap.NewNop()}
	return cs, reg
}

// registerTaskWithTest writes a minimal Deno task + passing test file
// into a fresh temp dir and registers it. assertEquals is defined inline
// rather than pulled in via tasks/sdk-test.ts (whose jsr:@std/yaml import
// needs network access) — go test must pass offline.
func registerTaskWithTest(t *testing.T, reg *registry.Registry, id string) *task.Spec {
	t.Helper()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "task.yaml"), []byte("name: "+id+"\ntrigger:\n  manual: true\nruntime: deno\n"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "task.ts"), []byte(`export default async function main() { return { ok: true } }`+"\n"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "task.test.ts"), []byte(`
function assertEquals(a: unknown, b: unknown) {
  if (a !== b) throw new Error("assertEquals: " + String(a) + " !== " + String(b));
}
Deno.test("smoke", () => { assertEquals(1, 1); });
`), 0644)

	spec := &task.Spec{
		ID:      id,
		Name:    id,
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 5 * time.Second,
		TaskDir: dir,
	}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}
	return spec
}

func TestControl_TaskTest_MissingTaskID(t *testing.T) {
	cs, _ := newControlServerForTaskTest(t)
	_, err := cs.handleTaskTest(context.Background(), Request{})
	if err == nil || !strings.Contains(err.Error(), "taskID required") {
		t.Errorf("err = %v, want 'taskID required'", err)
	}
}

func TestControl_TaskTest_UnknownTask(t *testing.T) {
	cs, _ := newControlServerForTaskTest(t)
	_, err := cs.handleTaskTest(context.Background(), Request{TaskID: "does/not/exist"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want 'not found'", err)
	}
}

func TestControl_TaskTest_NoTestFile(t *testing.T) {
	cs, reg := newControlServerForTaskTest(t)
	// Register a task without a test file.
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "task.yaml"), []byte("name: foo\ntrigger:\n  manual: true\nruntime: deno\n"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "task.ts"), []byte("export default () => ({})\n"), 0644)
	spec := &task.Spec{
		ID: "foo/no-test", Name: "foo", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 5 * time.Second, TaskDir: dir,
	}
	_ = reg.Register(spec)

	_, err := cs.handleTaskTest(context.Background(), Request{TaskID: "foo/no-test"})
	if err == nil || !strings.Contains(err.Error(), "no test file") {
		t.Errorf("err = %v, want ErrNoTestFile", err)
	}
}

// TestControl_TaskTest_HappyPath runs a real Deno test end-to-end through
// the handler. Slow (spawns Deno) but validates the full chain.
func TestControl_TaskTest_HappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping deno-spawning test in -short mode")
	}
	cs, reg := newControlServerForTaskTest(t)
	registerTaskWithTest(t, reg, "test/smoke")

	res, err := cs.handleTaskTest(context.Background(), Request{TaskID: "test/smoke"})
	if err != nil {
		t.Fatalf("handleTaskTest: %v (output=%q)", err, res.Output)
	}
	if res.Passed != 1 || res.Failed != 0 {
		t.Errorf("passed=%d failed=%d, want 1/0 (output=%q)", res.Passed, res.Failed, res.Output)
	}
	if res.Runtime != "deno" {
		t.Errorf("runtime=%q, want deno", res.Runtime)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit=%d, want 0", res.ExitCode)
	}
	if res.DurMs <= 0 {
		t.Errorf("duration not populated: %d", res.DurMs)
	}
}
