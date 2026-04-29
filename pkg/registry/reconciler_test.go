package registry

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/source"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// fakeSource is a controllable source for testing the reconciler.
type fakeSource struct {
	id string
	ch chan source.Event
}

func newFakeSource(id string) *fakeSource {
	return &fakeSource{id: id, ch: make(chan source.Event, 16)}
}

func (f *fakeSource) ID() string { return f.id }
func (f *fakeSource) Start(_ context.Context) (<-chan source.Event, error) {
	return f.ch, nil
}
func (f *fakeSource) Sync(_ context.Context) error { return nil }

func writeTask(t *testing.T, dir, name string) string {
	t.Helper()
	td := filepath.Join(dir, name)
	if err := os.MkdirAll(td, 0755); err != nil {
		t.Fatal(err)
	}
	yaml := "name: " + name + "\ntrigger:\n  manual: true\nruntime: deno\n"
	_ = os.WriteFile(filepath.Join(td, "task.yaml"), []byte(yaml), 0644)
	_ = os.WriteFile(filepath.Join(td, "task.ts"), []byte("return 'ok'"), 0644)
	return td
}

func newTestReconciler(t *testing.T, sources ...source.Source) (*Registry, *Reconciler) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	r := New(d)
	rec := NewReconciler(r, sources, "", zap.NewNop())
	return r, rec
}

func TestReconciler_Added(t *testing.T) {
	dir := t.TempDir()
	td := writeTask(t, dir, "my-task")

	fs := newFakeSource("test")
	reg, rec := newTestReconciler(t, fs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- rec.Run(ctx) }()

	fs.ch <- source.Event{Kind: source.EventAdded, TaskID: "my-task", TaskDir: td, Source: "test"}

	time.Sleep(100 * time.Millisecond)

	spec, ok := reg.Get("my-task")
	if !ok {
		t.Fatal("task not registered")
	}
	if spec.ID != "my-task" {
		t.Errorf("wrong ID: %s", spec.ID)
	}
}

func TestReconciler_Updated(t *testing.T) {
	dir := t.TempDir()
	td := writeTask(t, dir, "upd-task")

	fs := newFakeSource("test")
	reg, rec := newTestReconciler(t, fs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rec.Run(ctx)

	fs.ch <- source.Event{Kind: source.EventAdded, TaskID: "upd-task", TaskDir: td, Source: "test"}
	time.Sleep(50 * time.Millisecond)

	// Update the task name on disk and emit Updated.
	_ = os.WriteFile(filepath.Join(td, "task.yaml"),
		[]byte("name: upd-task-v2\ntrigger:\n  manual: true\nruntime: deno\n"), 0644)
	fs.ch <- source.Event{Kind: source.EventUpdated, TaskID: "upd-task", TaskDir: td, Source: "test"}
	time.Sleep(50 * time.Millisecond)

	spec, _ := reg.Get("upd-task")
	if spec == nil || spec.Name != "upd-task-v2" {
		t.Errorf("expected updated name, got %v", spec)
	}
}

func TestReconciler_Removed(t *testing.T) {
	dir := t.TempDir()
	td := writeTask(t, dir, "rem-task")

	fs := newFakeSource("test")
	reg, rec := newTestReconciler(t, fs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rec.Run(ctx)

	fs.ch <- source.Event{Kind: source.EventAdded, TaskID: "rem-task", TaskDir: td, Source: "test"}
	time.Sleep(50 * time.Millisecond)

	fs.ch <- source.Event{Kind: source.EventRemoved, TaskID: "rem-task", TaskDir: td, Source: "test"}
	time.Sleep(50 * time.Millisecond)

	if _, ok := reg.Get("rem-task"); ok {
		t.Error("task should be removed")
	}
}

func TestReconciler_InvalidTask_Ignored(t *testing.T) {
	dir := t.TempDir()
	td := filepath.Join(dir, "bad-task")
	_ = os.MkdirAll(td, 0755)
	// task.yaml with missing required field (name)
	_ = os.WriteFile(filepath.Join(td, "task.yaml"), []byte("trigger:\n  manual: true\n"), 0644)

	fs := newFakeSource("test")
	reg, rec := newTestReconciler(t, fs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rec.Run(ctx)

	fs.ch <- source.Event{Kind: source.EventAdded, TaskID: "bad-task", TaskDir: td, Source: "test"}
	time.Sleep(50 * time.Millisecond)

	if _, ok := reg.Get("bad-task"); ok {
		t.Error("invalid task should not be registered")
	}
}

func TestReconciler_OnRegisterCallback(t *testing.T) {
	dir := t.TempDir()
	td := writeTask(t, dir, "cb-task")

	fs := newFakeSource("test")
	_, rec := newTestReconciler(t, fs)

	var mu sync.Mutex
	var called *task.Spec
	rec.OnRegister = func(spec *task.Spec) {
		mu.Lock()
		called = spec
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rec.Run(ctx)

	fs.ch <- source.Event{Kind: source.EventAdded, TaskID: "cb-task", TaskDir: td, Source: "test"}
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got := called
	mu.Unlock()
	if got == nil || got.ID != "cb-task" {
		t.Errorf("OnRegister not called, got %v", got)
	}
}

func TestReconciler_RejectsUnknownTaskProvider(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := New(d)

	consumer := &task.Spec{
		ID:   "consumer",
		Name: "consumer",
		Permissions: task.Permissions{
			Env: []task.EnvEntry{{Name: "PG_URL", From: "task:nonexistent-provider"}},
		},
		Trigger: task.TriggerConfig{Manual: true},
	}

	rc := NewReconciler(reg, nil, "", zap.NewNop())
	rc.runCtx = ctx
	rc.merged = make(chan source.Event, 1)

	rc.handle(source.Event{
		Kind:    source.EventAdded,
		TaskID:  "consumer",
		Spec:    consumer,
		Source:  "test",
		TaskDir: "",
	})

	if _, ok := reg.Get("consumer"); ok {
		t.Fatalf("consumer with unknown task: provider should NOT have been registered")
	}
}

func TestReconciler_MultipleSources(t *testing.T) {
	dir := t.TempDir()
	td1 := writeTask(t, dir, "src1-task")
	td2 := writeTask(t, dir, "src2-task")

	fs1 := newFakeSource("src1")
	fs2 := newFakeSource("src2")
	reg, rec := newTestReconciler(t, fs1, fs2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rec.Run(ctx)

	fs1.ch <- source.Event{Kind: source.EventAdded, TaskID: "src1-task", TaskDir: td1, Source: "src1"}
	fs2.ch <- source.Event{Kind: source.EventAdded, TaskID: "src2-task", TaskDir: td2, Source: "src2"}
	time.Sleep(100 * time.Millisecond)

	if _, ok := reg.Get("src1-task"); !ok {
		t.Error("src1-task not registered")
	}
	if _, ok := reg.Get("src2-task"); !ok {
		t.Error("src2-task not registered")
	}
}

func TestReconciler_InjectsDATADIR(t *testing.T) {
	dataDir := "/var/lib/dicode-test"
	// The reconciler derives spec.ID from filepath.Base(ev.TaskDir), so the
	// task directory name must match the event's TaskID.
	taskDir := filepath.Join(t.TempDir(), "dummy")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}

	taskYAML := `name: dummy
runtime: deno
trigger:
  manual: true
permissions:
  fs:
    - path: "${DATADIR}/some-subdir"
      permission: rw
`
	if err := os.WriteFile(filepath.Join(taskDir, "task.yaml"), []byte(taskYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "task.ts"), []byte("export default async function main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := newFakeSource("test")

	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := New(d)
	rc := NewReconciler(reg, []source.Source{fs}, dataDir, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rc.Run(ctx)

	fs.ch <- source.Event{Kind: source.EventAdded, TaskID: "dummy", TaskDir: taskDir, Source: "test"}
	time.Sleep(100 * time.Millisecond)

	spec, ok := reg.Get("dummy")
	if !ok {
		t.Fatal("task not registered")
	}
	if len(spec.Permissions.FS) == 0 {
		t.Fatal("expected at least one FS permission entry")
	}
	want := dataDir + "/some-subdir"
	if spec.Permissions.FS[0].Path != want {
		t.Errorf("FS[0].Path = %q, want %q (${DATADIR} was not expanded)", spec.Permissions.FS[0].Path, want)
	}
}
