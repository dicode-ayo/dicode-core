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
	"github.com/dicode/dicode/pkg/taskset"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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

// TestReconciler_InvalidTask_RecordsLoadFailure is the pkg/registry-level
// regression lock for #649's reconciler.handle() path: a source event whose
// task.yaml fails to load must be recorded via Registry.LoadFailure rather
// than only logged, so the webui can still surface it even though the task
// never registers.
func TestReconciler_InvalidTask_RecordsLoadFailure(t *testing.T) {
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

	fails := reg.LoadFailures()
	f, ok := fails["bad-task"]
	if !ok {
		t.Fatalf("want a recorded load failure for bad-task, got %v", fails)
	}
	if f.Source != "test" {
		t.Errorf("Source = %q, want %q", f.Source, "test")
	}
	if f.Error == "" {
		t.Error("Error should be non-empty")
	}
}

// TestReconciler_LoadFailureClearedOnRecovery covers both halves of #649's
// "don't leave stale failure state behind" requirement: a task that fails to
// load and then loads cleanly must have its failure record cleared on
// successful registration, and a task that fails to load and is then
// genuinely removed must not leave a ghost failure record behind either.
func TestReconciler_LoadFailureClearedOnRecovery(t *testing.T) {
	dir := t.TempDir()
	td := writeTask(t, dir, "flaky-task")

	fs := newFakeSource("test")
	reg, rec := newTestReconciler(t, fs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rec.Run(ctx)

	// Break it first.
	_ = os.WriteFile(filepath.Join(td, "task.yaml"), []byte("trigger:\n  manual: true\n"), 0644)
	fs.ch <- source.Event{Kind: source.EventAdded, TaskID: "flaky-task", TaskDir: td, Source: "test"}
	time.Sleep(50 * time.Millisecond)
	if _, ok := reg.LoadFailures()["flaky-task"]; !ok {
		t.Fatal("expected a load failure to be recorded after the broken load")
	}

	// Fix it and re-emit — Register must clear the failure record.
	_ = os.WriteFile(filepath.Join(td, "task.yaml"), []byte("name: flaky-task\ntrigger:\n  manual: true\nruntime: deno\n"), 0644)
	fs.ch <- source.Event{Kind: source.EventUpdated, TaskID: "flaky-task", TaskDir: td, Source: "test"}
	time.Sleep(50 * time.Millisecond)

	if _, ok := reg.Get("flaky-task"); !ok {
		t.Fatal("task should now be registered")
	}
	if _, ok := reg.LoadFailures()["flaky-task"]; ok {
		t.Error("load failure should be cleared once the task registers successfully")
	}
}

func TestReconciler_LoadFailureClearedOnRemoved(t *testing.T) {
	dir := t.TempDir()
	td := filepath.Join(dir, "gone-task")
	_ = os.MkdirAll(td, 0755)
	_ = os.WriteFile(filepath.Join(td, "task.yaml"), []byte("trigger:\n  manual: true\n"), 0644)

	fs := newFakeSource("test")
	reg, rec := newTestReconciler(t, fs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rec.Run(ctx)

	fs.ch <- source.Event{Kind: source.EventAdded, TaskID: "gone-task", TaskDir: td, Source: "test"}
	time.Sleep(50 * time.Millisecond)
	if _, ok := reg.LoadFailures()["gone-task"]; !ok {
		t.Fatal("expected a load failure to be recorded")
	}

	fs.ch <- source.Event{Kind: source.EventRemoved, TaskID: "gone-task", Source: "test"}
	time.Sleep(50 * time.Millisecond)

	if _, ok := reg.LoadFailures()["gone-task"]; ok {
		t.Error("load failure should be cleared once the entry is genuinely removed")
	}
}

func TestReconciler_OnRegisterCallback(t *testing.T) {
	dir := t.TempDir()
	td := writeTask(t, dir, "cb-task")

	fs := newFakeSource("test")
	_, rec := newTestReconciler(t, fs)

	var mu sync.Mutex
	var called task.Kinded
	rec.OnRegister = func(k task.Kinded) {
		mu.Lock()
		called = k
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
	if got == nil || got.TaskID() != "cb-task" {
		t.Errorf("OnRegister not called, got %v", got)
	}
}

// TestReconcilerLoadsPipelineKind covers the reconciler boundary only: a raw
// source event whose task.yaml declares kind: PipelineTask is loaded as a
// *task.PipelineTask, stored in the registry under the event's TaskID, and
// surfaced through the kind-aware OnRegister hook. Engine-side acceptance
// (registerPipeline's stage-ref + cycle validation) is exercised in
// pkg/trigger/registerpipeline_test.go; it can't be cross-tested here because
// pkg/trigger imports pkg/registry, not the reverse, so wiring a real engine
// into a package registry test would be an import cycle.
func TestReconcilerLoadsPipelineKind(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "task.yaml"), []byte(`apiVersion: dicode/v1
kind: PipelineTask
name: P
subtype: sequential
trigger:
  manual: true
stages:
  - task: buildin/template
`), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, rec := newTestReconciler(t)

	var registered task.Kinded
	rec.OnRegister = func(k task.Kinded) { registered = k }

	rec.handle(source.Event{Kind: source.EventAdded, TaskID: "demo/p", TaskDir: dir})

	if registered == nil || registered.KindOf() != task.KindPipelineTask {
		t.Fatalf("expected pipeline registration via OnRegister, got %v", registered)
	}
	if k, ok := reg.GetKinded("demo/p"); !ok || k.KindOf() != task.KindPipelineTask {
		t.Fatalf("registry missing pipeline under event TaskID: %v %v", k, ok)
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
		Kinded:  consumer,
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

// TestReconciler_RetryPendingAfterProviderRegisters verifies that a consumer
// task referencing a provider via `from: task:my-provider` is queued (not
// registered) when it arrives before its provider, and is automatically
// registered once the provider registers via the pending-retry mechanism.
func TestReconciler_RetryPendingAfterProviderRegisters(t *testing.T) {
	fs := newFakeSource("test")
	reg, rec := newTestReconciler(t, fs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rec.Run(ctx)

	time.Sleep(20 * time.Millisecond) // let Run() set up merged channel

	provider := &task.Spec{
		ID:      "my-provider",
		Name:    "my-provider",
		Trigger: task.TriggerConfig{Manual: true},
	}
	consumer := &task.Spec{
		ID:   "my-consumer",
		Name: "my-consumer",
		Permissions: task.Permissions{
			Env: []task.EnvEntry{{Name: "PG_URL", From: "task:my-provider"}},
		},
		Trigger: task.TriggerConfig{Manual: true},
	}

	// Consumer arrives first — provider not yet registered.
	fs.ch <- source.Event{Kind: source.EventAdded, TaskID: "my-consumer", Kinded: consumer, Source: "test"}
	time.Sleep(100 * time.Millisecond)

	if _, ok := reg.Get("my-consumer"); ok {
		t.Fatal("consumer should be pending (provider not yet registered), not in registry")
	}

	// Register the provider — this should trigger a retry of the pending consumer.
	fs.ch <- source.Event{Kind: source.EventAdded, TaskID: "my-provider", Kinded: provider, Source: "test"}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			_, provOk := reg.Get("my-provider")
			_, conOk := reg.Get("my-consumer")
			t.Fatalf("timeout: provider=%v consumer=%v; consumer should have been retried automatically", provOk, conOk)
		case <-time.After(20 * time.Millisecond):
			if _, ok := reg.Get("my-consumer"); ok {
				return
			}
		}
	}
}

// TestReconciler_RemovedWhilePendingNotRetried verifies that a task removed
// (EventRemoved) while it sits in the pending-retry queue is NOT re-registered
// when its provider eventually registers.
func TestReconciler_RemovedWhilePendingNotRetried(t *testing.T) {
	fs := newFakeSource("test")
	reg, rec := newTestReconciler(t, fs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rec.Run(ctx)

	time.Sleep(20 * time.Millisecond)

	provider := &task.Spec{
		ID:      "ghost-provider",
		Name:    "ghost-provider",
		Trigger: task.TriggerConfig{Manual: true},
	}
	consumer := &task.Spec{
		ID:   "ghost-consumer",
		Name: "ghost-consumer",
		Permissions: task.Permissions{
			Env: []task.EnvEntry{{Name: "X", From: "task:ghost-provider"}},
		},
		Trigger: task.TriggerConfig{Manual: true},
	}

	// Consumer queued — provider not yet registered.
	fs.ch <- source.Event{Kind: source.EventAdded, TaskID: "ghost-consumer", Kinded: consumer, Source: "test"}
	time.Sleep(80 * time.Millisecond)

	// Consumer is removed before the provider arrives.
	fs.ch <- source.Event{Kind: source.EventRemoved, TaskID: "ghost-consumer", Source: "test"}
	time.Sleep(80 * time.Millisecond)

	// Provider registers — must NOT cause removed consumer to reappear.
	fs.ch <- source.Event{Kind: source.EventAdded, TaskID: "ghost-provider", Kinded: provider, Source: "test"}
	time.Sleep(300 * time.Millisecond)

	if _, ok := reg.Get("ghost-consumer"); ok {
		t.Fatal("removed-while-pending consumer was re-registered after provider arrived")
	}
	if _, ok := reg.Get("ghost-provider"); !ok {
		t.Fatal("ghost-provider should be registered")
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

// TestReconciler_QueuedWarnLoggedOnce verifies that an unresolved provider
// dependency warns exactly once even when the queued-retry check re-runs on
// every subsequent registration (#521). Repeat re-checks drop to debug so one
// unresolved task does not emit N identical WARN lines during startup.
func TestReconciler_QueuedWarnLoggedOnce(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)

	fs := newFakeSource("test")
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := New(d)
	rc := NewReconciler(reg, []source.Source{fs}, "", zap.New(core))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rc.Run(ctx)

	time.Sleep(20 * time.Millisecond) // let Run() set up merged channel

	consumer := &task.Spec{
		ID:   "waiting-consumer",
		Name: "waiting-consumer",
		Permissions: task.Permissions{
			Env: []task.EnvEntry{{Name: "PG_URL", From: "task:missing-provider"}},
		},
		Trigger: task.TriggerConfig{Manual: true},
	}
	// Consumer queued: provider never registers, so it stays unresolved.
	fs.ch <- source.Event{Kind: source.EventAdded, TaskID: "waiting-consumer", Kinded: consumer, Source: "test"}
	time.Sleep(50 * time.Millisecond)

	// Register several unrelated tasks. Each successful registration re-runs
	// retryPending, re-checking the still-unresolved consumer.
	for i := 0; i < 5; i++ {
		other := &task.Spec{
			ID:      "other-" + string(rune('a'+i)),
			Name:    "other",
			Trigger: task.TriggerConfig{Manual: true},
		}
		fs.ch <- source.Event{Kind: source.EventAdded, TaskID: other.ID, Kinded: other, Source: "test"}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	const warnMsg = "task references unknown provider; queued for retry after next registration"
	warnCount := logs.FilterMessage(warnMsg).Len()
	if warnCount != 1 {
		t.Fatalf("expected exactly 1 WARN for the unresolved provider, got %d", warnCount)
	}
	// The repeat re-checks must still leave a trail at debug level.
	if debugCount := logs.FilterMessage("task still references unknown provider; remains queued for retry").Len(); debugCount == 0 {
		t.Fatal("expected repeat queued re-checks to be logged at debug, got none")
	}
}

// ---------------------------------------------------------------------------
// Source.ID() collision regression coverage (issue #621)
// ---------------------------------------------------------------------------
//
// pkg/webui's apiAddSource/apiRemoveSource and pkg/daemon's
// buildTaskSetSourceFromEntry all construct the id passed into
// taskset.NewSource, and rc.cancels below is keyed by exactly that value
// (via src.ID()). Before taskset.SourceID existed, that id was just
// ref.URL (or ref.Path when URL was empty) — with no name component — so
// two different dicode.yaml entries (or an existing entry and a
// dynamically-added one) referencing the identical path or URL collided:
// the second AddSource silently overwrote the first source's entry in
// rc.cancels, orphaning its cancel func, and a later RemoveSource(id) for
// either name could tear down the wrong source's context.

// TestReconciler_NameQualifiedSourceIDs_RemoveDoesNotClobberOther is the
// regression guard: two sources built with taskset.SourceID for different
// names sharing the same underlying ref path get distinct cancel-bookkeeping
// entries, and removing one leaves the other's entirely untouched. Using the
// pre-fix formula (id := ref.Path, ignoring name) for both sources here would
// collide, cancels would hold a single entry after both AddSource calls, and
// the len(rc.cancels) == 2 assertion below would fail — this test would not
// have passed against that code.
func TestReconciler_NameQualifiedSourceIDs_RemoveDoesNotClobberOther(t *testing.T) {
	sharedRef := &taskset.Ref{Path: "/tmp/shared/taskset.yaml"}
	idA := taskset.SourceID("e2e-tests", sharedRef)
	idB := taskset.SourceID("e2e-add-local-123", sharedRef)
	if idA == idB {
		t.Fatalf("test setup invalid: idA and idB must differ, both = %q", idA)
	}

	fsA := newFakeSource(idA)
	fsB := newFakeSource(idB)
	_, rec := newTestReconciler(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = rec.Run(ctx) }()
	<-rec.Ready()

	if err := rec.AddSource(fsA); err != nil {
		t.Fatalf("AddSource(fsA): %v", err)
	}
	if err := rec.AddSource(fsB); err != nil {
		t.Fatalf("AddSource(fsB): %v", err)
	}

	rec.mu.Lock()
	n := len(rec.cancels)
	_, hasA := rec.cancels[idA]
	_, hasB := rec.cancels[idB]
	rec.mu.Unlock()
	if n != 2 || !hasA || !hasB {
		t.Fatalf("expected both sources tracked independently in rc.cancels (len=2, idA and idB both present); got len=%d hasA=%v hasB=%v — a Source.ID() collision would clobber one entry", n, hasA, hasB)
	}

	rec.RemoveSource(idA)

	rec.mu.Lock()
	n = len(rec.cancels)
	_, hasA = rec.cancels[idA]
	_, hasB = rec.cancels[idB]
	rec.mu.Unlock()
	if n != 1 || hasA || !hasB {
		t.Fatalf("expected only idB to remain after RemoveSource(idA); got len=%d hasA=%v hasB=%v", n, hasA, hasB)
	}
}

// TestReconciler_CollidingSourceIDs_SecondAddClobbersFirstCancel documents the
// hazard class itself: the reconciler has no duplicate-ID guard, so if two
// sources are ever constructed with the SAME id (as every call site did
// before taskset.SourceID was introduced, whenever two entries shared a path
// or URL), the second AddSource silently overwrites the first's cancel func
// in rc.cancels. This is not a call the reconciler can safely make on its own
// (a legitimate re-add-after-remove also reuses the same id) — the fix is
// upstream, at the call sites that mint ids (see the name-qualified test
// above). This test exists so a future change to startSource that
// reintroduces this exact silent-overwrite behavior is caught if the
// call-site fix above is ever reverted or bypassed.
func TestReconciler_CollidingSourceIDs_SecondAddClobbersFirstCancel(t *testing.T) {
	const collidingID = "/tmp/shared/taskset.yaml" // pre-fix formula: id == ref.Path, no name

	fsA := newFakeSource(collidingID)
	fsB := newFakeSource(collidingID)
	_, rec := newTestReconciler(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = rec.Run(ctx) }()
	<-rec.Ready()

	if err := rec.AddSource(fsA); err != nil {
		t.Fatalf("AddSource(fsA): %v", err)
	}
	if err := rec.AddSource(fsB); err != nil {
		t.Fatalf("AddSource(fsB): %v", err)
	}

	rec.mu.Lock()
	n := len(rec.cancels)
	rec.mu.Unlock()
	if n != 1 {
		t.Fatalf("expected the colliding ids to overwrite one another in rc.cancels (len=1, documenting the hazard); got len=%d", n)
	}
}
