package taskset

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/source"
	"go.uber.org/zap"
)

const emptyFixtureTaskSet = `apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: fixture
spec:
  entries: {}
`

func TestSource_Remove_DeletesClonesAndEndsDevMode(t *testing.T) {
	remote := newFixtureRemote(t, "main", map[string]string{"taskset.yaml": emptyFixtureTaskSet})
	src := newTestSourceWithRemote(t, "ns", remote, "main")
	ctx := context.Background()

	for _, runID := range []string{"run-a", "run-b"} {
		if err := src.SetDevMode(ctx, true, DevModeOpts{Branch: "fix/" + runID, Base: "main", RunID: runID}); err != nil {
			t.Fatalf("enable %s: %v", runID, err)
		}
	}
	nsDir := filepath.Join(src.DataDir(), "dev-clones", src.Namespace())

	src.Remove()

	if src.DevMode() {
		t.Error("DevMode() = true after Remove")
	}
	if src.DevRootPath() != "" {
		t.Errorf("DevRootPath = %q after Remove, want empty", src.DevRootPath())
	}
	assertNoClones(t, nsDir)
	err := src.SetDevMode(ctx, true, DevModeOpts{Branch: "fix/late", Base: "main", RunID: "run-late"})
	if !errors.Is(err, ErrSourceRemoved) {
		t.Fatalf("SetDevMode after Remove = %v, want ErrSourceRemoved", err)
	}
	if _, err := os.Stat(filepath.Join(nsDir, "run-late")); !os.IsNotExist(err) {
		t.Error("SetDevMode after Remove created a clone")
	}
}

// A clone still running when Remove lands has its directory removed once it
// finishes, and does not re-enable dev mode.
func TestSource_Remove_DuringInFlightClone(t *testing.T) {
	remote := newFixtureRemote(t, "main", map[string]string{"taskset.yaml": emptyFixtureTaskSet})
	src := newTestSourceWithRemote(t, "ns", remote, "main")
	ctx := context.Background()
	const runID = "run-inflight"
	opts := DevModeOpts{Branch: "fix/inflight", Base: "main", RunID: runID}
	cloneDir := filepath.Join(src.DataDir(), "dev-clones", src.Namespace(), runID)

	// Reproduce SetDevMode's state while enableClone runs outside the lock.
	src.mu.Lock()
	src.clones = map[string]cloneState{runID: {cloneDir: cloneDir}}
	src.mu.Unlock()

	src.Remove()

	devPath, cloneErr := src.enableClone(ctx, opts)
	if cloneErr != nil {
		t.Fatalf("enableClone: %v", cloneErr)
	}
	if _, err := os.Stat(cloneDir); err != nil {
		t.Fatalf("test setup: clone dir missing after enableClone: %v", err)
	}
	if err := src.finishClone(ctx, opts, devPath, nil, func() {}); !errors.Is(err, ErrSourceRemoved) {
		t.Fatalf("finishClone after Remove = %v, want ErrSourceRemoved", err)
	}
	if _, err := os.Stat(cloneDir); !os.IsNotExist(err) {
		t.Errorf("late-finishing clone left on disk: err = %v", err)
	}
	if src.DevMode() {
		t.Error("late-finishing clone re-enabled dev mode")
	}
}

// A dev-mode change on a source whose watch loop has exited must not send on
// the closed event channel.
func TestSource_SetDevModeAfterStop_DoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	deployDir := writeTaskDir(t, dir, "deploy")
	buildDir := writeTaskDir(t, dir, "build")
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    deploy:
      ref:
        path: `+filepath.Join(deployDir, "task.yaml")+`
`)
	devPath := writeTaskSetFile(t, dir, "dev-taskset.yaml", `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    deploy:
      ref:
        path: `+filepath.Join(deployDir, "task.yaml")+`
    build:
      ref:
        path: `+filepath.Join(buildDir, "task.yaml")+`
`)

	src := newTestSource(t, "infra", tsPath)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := src.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()
	deadline := time.After(2 * time.Second)
	for open := true; open; {
		select {
		case _, open = <-ch:
		case <-deadline:
			t.Fatal("event channel never closed after cancel")
		}
	}

	if err := src.SetDevMode(context.Background(), true, DevModeOpts{LocalPath: devPath}); err != nil {
		t.Fatalf("SetDevMode: %v", err)
	}
}

// A root taskset nested below the repo root must not leave the rest of the
// clone behind.
func TestSource_Remove_NestedRootDeletesWholeClone(t *testing.T) {
	remote := newFixtureRemote(t, "main", map[string]string{"tasks/taskset.yaml": emptyFixtureTaskSet})
	src := NewSource(remote, "ns", &Ref{URL: remote, Branch: "main", Path: "tasks/taskset.yaml"}, "", t.TempDir(), false, 30*time.Second, zap.NewNop())
	ctx := context.Background()
	if err := src.SetDevMode(ctx, true, DevModeOpts{Branch: "fix/nested", Base: "main", RunID: "run-nested"}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	nsDir := filepath.Join(src.DataDir(), "dev-clones", src.Namespace())

	src.Remove()

	assertNoClones(t, nsDir)
}

// Disabling a nested-root clone deletes the clone root, not the subdirectory
// holding the taskset.
func TestSource_DisableClone_NestedRootDeletesWholeClone(t *testing.T) {
	remote := newFixtureRemote(t, "main", map[string]string{"tasks/taskset.yaml": emptyFixtureTaskSet})
	src := NewSource(remote, "ns", &Ref{URL: remote, Branch: "main", Path: "tasks/taskset.yaml"}, "", t.TempDir(), false, 30*time.Second, zap.NewNop())
	ctx := context.Background()
	const runID = "run-nested"
	if err := src.SetDevMode(ctx, true, DevModeOpts{Branch: "fix/nested", Base: "main", RunID: runID}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	cloneDir := filepath.Join(src.DataDir(), "dev-clones", src.Namespace(), runID)

	if err := src.SetDevMode(ctx, false, DevModeOpts{RunID: runID}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := os.Stat(cloneDir); !os.IsNotExist(err) {
		t.Errorf("clone root still exists after disable: err = %v", err)
	}
}

// A clone dropped by disable-all while in progress is deleted when it finishes,
// and its run ID stays busy until then.
func TestSource_DisableAllDuringInFlightClone(t *testing.T) {
	remote := newFixtureRemote(t, "main", map[string]string{"taskset.yaml": emptyFixtureTaskSet})
	src := newTestSourceWithRemote(t, "ns", remote, "main")
	ctx := context.Background()
	const runID = "run-inflight"
	opts := DevModeOpts{Branch: "fix/inflight", Base: "main", RunID: runID}
	cloneDir := filepath.Join(src.DataDir(), "dev-clones", src.Namespace(), runID)

	src.mu.Lock()
	src.clones = map[string]cloneState{runID: {cloneDir: cloneDir}}
	src.mu.Unlock()

	if err := src.SetDevMode(ctx, false, DevModeOpts{}); err != nil {
		t.Fatalf("disable all: %v", err)
	}
	if err := src.SetDevMode(ctx, true, opts); !errors.Is(err, ErrDevModeBusy) {
		t.Fatalf("re-enable while abandoned clone in flight = %v, want ErrDevModeBusy", err)
	}
	devPath, cloneErr := src.enableClone(ctx, opts)
	if cloneErr != nil {
		t.Fatalf("enableClone: %v", cloneErr)
	}
	if err := src.finishClone(ctx, opts, devPath, nil, func() {}); err == nil || errors.Is(err, ErrSourceRemoved) {
		t.Fatalf("finishClone after disable-all = %v, want a concurrent-disable error", err)
	}
	if _, err := os.Stat(cloneDir); !os.IsNotExist(err) {
		t.Errorf("abandoned clone left on disk: err = %v", err)
	}
	if src.DevMode() {
		t.Error("abandoned clone enabled dev mode")
	}
}

// Remove waits for the watch loop of a stopped source before tearing down.
func TestSource_Remove_WaitsForWatchLoop(t *testing.T) {
	dir := t.TempDir()
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", emptyFixtureTaskSet)
	src := newTestSource(t, "ns", tsPath)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := src.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()
	src.Remove()
	for {
		select {
		case _, open := <-ch:
			if !open {
				return
			}
		default:
			t.Fatal("Remove returned before the watch loop closed its channel")
		}
	}
}

// Disabling dev mode on a removed source is a no-op, not an error.
func TestSource_DisableAfterRemove_NoError(t *testing.T) {
	src := newTestSource(t, "ns", "/tmp/fixture-taskset.yaml")
	src.Remove()
	if err := src.SetDevMode(context.Background(), false, DevModeOpts{}); err != nil {
		t.Fatalf("disable after Remove = %v, want nil", err)
	}
	if err := src.SetDevMode(context.Background(), true, DevModeOpts{LocalPath: "/tmp/x.yaml"}); !errors.Is(err, ErrSourceRemoved) {
		t.Fatalf("enable after Remove = %v, want ErrSourceRemoved", err)
	}
}

// Remove cancels a clone still in progress and waits for it to settle, so
// nothing is left under dev-clones afterwards.
func TestSource_Remove_CancelsAndWaitsForInFlightClone(t *testing.T) {
	src := newTestSource(t, "ns", "/tmp/fixture-taskset.yaml")
	const runID = "run-inflight"
	opts := DevModeOpts{Branch: "fix/inflight", RunID: runID}
	cloneDir := filepath.Join(src.DataDir(), "dev-clones", src.Namespace(), runID)

	// Reproduce SetDevMode's state while enableClone runs outside the lock.
	cloneCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	src.mu.Lock()
	src.clones = map[string]cloneState{runID: {cloneDir: cloneDir, cancel: cancel, done: done}}
	src.mu.Unlock()

	go func() {
		<-cloneCtx.Done()
		// A clone writes its directory right up to the point it sees the cancel.
		_ = os.MkdirAll(filepath.Join(cloneDir, ".git"), 0o755)
		_ = src.finishClone(context.Background(), opts, "", cloneCtx.Err(), func() {
			cancel()
			close(done)
		})
	}()

	removed := make(chan struct{})
	go func() { src.Remove(); close(removed) }()
	select {
	case <-removed:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("Remove did not cancel the in-progress clone")
	}
	select {
	case <-done:
	default:
		t.Fatal("Remove returned before the in-progress clone settled")
	}
	assertNoClones(t, filepath.Join(src.DataDir(), "dev-clones", src.Namespace()))
}

// Concurrent syncs must emit their diffs in the order they advance the
// snapshot, so replaying the event stream yields the final snapshot.
func TestSource_ConcurrentSyncs_EventsMatchSnapshot(t *testing.T) {
	dir := t.TempDir()
	deployDir := writeTaskDir(t, dir, "deploy")
	buildDir := writeTaskDir(t, dir, "build")
	entry := func(name, d string) string {
		return "    " + name + ":\n      ref:\n        path: " + filepath.Join(d, "task.yaml") + "\n"
	}
	head := "apiVersion: dicode/v1\nkind: TaskSet\nmetadata:\n  name: infra\nspec:\n  entries:\n"
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", head+entry("deploy", deployDir))
	devPath := writeTaskSetFile(t, dir, "dev-taskset.yaml", head+entry("deploy", deployDir)+entry("build", buildDir))

	src := newTestSource(t, "infra", tsPath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := src.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	seen := map[string]bool{}
	consumed := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		defer close(consumed)
		for {
			select {
			case ev := <-ch:
				seen[ev.TaskID] = ev.Kind != "removed"
			case <-stop:
				for {
					select {
					case ev := <-ch:
						seen[ev.TaskID] = ev.Kind != "removed"
					default:
						return
					}
				}
			}
		}
	}()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if (g+i)%2 == 0 {
					_ = src.SetDevMode(context.Background(), true, DevModeOpts{LocalPath: devPath})
				} else {
					_ = src.SetDevMode(context.Background(), false, DevModeOpts{})
				}
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	<-consumed

	src.mu.Lock()
	want := map[string]bool{}
	for id := range src.snapshot {
		want[id] = true
	}
	src.mu.Unlock()
	for id, present := range seen {
		if present != want[id] {
			t.Errorf("task %q: event stream says present=%v, snapshot says %v", id, present, want[id])
		}
	}
}

// Remove also waits for a clone that an earlier disable-all abandoned.
func TestSource_Remove_WaitsForCloneAbandonedByDisableAll(t *testing.T) {
	src := newTestSource(t, "ns", "/tmp/fixture-taskset.yaml")
	const runID = "run-abandoned"
	opts := DevModeOpts{Branch: "fix/abandoned", RunID: runID}
	cloneDir := filepath.Join(src.DataDir(), "dev-clones", src.Namespace(), runID)

	cloneCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	src.mu.Lock()
	src.clones = map[string]cloneState{runID: {cloneDir: cloneDir, cancel: cancel, done: done}}
	src.mu.Unlock()

	if err := src.SetDevMode(context.Background(), false, DevModeOpts{}); err != nil {
		t.Fatalf("disable all: %v", err)
	}
	release := make(chan struct{})
	go func() {
		<-cloneCtx.Done()
		<-release
		_ = os.MkdirAll(filepath.Join(cloneDir, ".git"), 0o755)
		_ = src.finishClone(context.Background(), opts, "", cloneCtx.Err(), func() { close(done) })
	}()

	removed := make(chan struct{})
	go func() { src.Remove(); close(removed) }()
	select {
	case <-removed:
		close(release)
		t.Fatal("Remove returned before the abandoned clone settled")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-removed:
	case <-time.After(5 * time.Second):
		t.Fatal("Remove never returned")
	}
	assertNoClones(t, filepath.Join(src.DataDir(), "dev-clones", src.Namespace()))
}

// A sync that started before the one that last swapped the snapshot must
// neither swap it back nor emit.
func TestSource_StaleSync_Dropped(t *testing.T) {
	dir := t.TempDir()
	deployDir := writeTaskDir(t, dir, "deploy")
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", "apiVersion: dicode/v1\nkind: TaskSet\nmetadata:\n  name: infra\nspec:\n  entries:\n    deploy:\n      ref:\n        path: "+filepath.Join(deployDir, "task.yaml")+"\n")
	src := newTestSource(t, "infra", tsPath)

	// Pretend a sync handed out after this one has already swapped.
	src.mu.Lock()
	src.appliedSeq = src.syncSeq + 2
	src.mu.Unlock()

	ch := make(chan source.Event, 8)
	if err := src.sync(context.Background(), ch); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(ch) != 0 {
		t.Errorf("stale sync emitted %d events", len(ch))
	}
	src.mu.Lock()
	n := len(src.snapshot)
	src.mu.Unlock()
	if n != 0 {
		t.Errorf("stale sync swapped the snapshot (%d tasks)", n)
	}

	if err := src.sync(context.Background(), ch); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(ch) != 1 {
		t.Errorf("fresh sync emitted %d events, want 1", len(ch))
	}
}

// assertNoClones fails if any clone is left under the namespace directory.
func assertNoClones(t *testing.T, nsDir string) {
	t.Helper()
	entries, err := os.ReadDir(nsDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read %s: %v", nsDir, err)
	}
	if len(entries) != 0 {
		t.Errorf("clones left under %s: %d entries", nsDir, len(entries))
	}
}

// Remove clears the source's load failures, so a removed source stops
// reporting failing entries.
func TestSource_Remove_ClearsLoadFailures(t *testing.T) {
	dir := t.TempDir()
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", "apiVersion: dicode/v1\nkind: TaskSet\nmetadata:\n  name: infra\nspec:\n  entries:\n    broken:\n      ref:\n        path: "+filepath.Join(dir, "missing", "task.yaml")+"\n")
	src := newTestSource(t, "infra", tsPath)
	ch := make(chan source.Event, 8)
	if err := src.sync(context.Background(), ch); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(src.LoadFailures()) == 0 {
		t.Fatal("test setup: expected a load failure for the missing entry")
	}

	src.Remove()
	if n := len(src.LoadFailures()); n != 0 {
		t.Fatalf("LoadFailures after Remove = %d entries, want 0", n)
	}
	if err := src.sync(context.Background(), ch); err != nil {
		t.Fatalf("sync after Remove: %v", err)
	}
	if n := len(src.LoadFailures()); n != 0 {
		t.Fatalf("sync after Remove republished %d load failures", n)
	}
}

// Disabling a clone still in progress leaves its directory to finishClone,
// which deletes it once the cancelled clone has stopped writing.
func TestSource_DisableInFlightClone_LeavesDirToFinishClone(t *testing.T) {
	src := newTestSource(t, "ns", "/tmp/fixture-taskset.yaml")
	const runID = "run-inflight"
	opts := DevModeOpts{Branch: "fix/inflight", RunID: runID}
	cloneDir := filepath.Join(src.DataDir(), "dev-clones", src.Namespace(), runID)
	if err := os.MkdirAll(cloneDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src.mu.Lock()
	src.clones = map[string]cloneState{runID: {cloneDir: cloneDir}}
	src.mu.Unlock()

	if err := src.SetDevMode(context.Background(), false, DevModeOpts{RunID: runID}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := os.Stat(cloneDir); err != nil {
		t.Fatalf("disable deleted an in-progress clone's dir: %v", err)
	}
	_ = src.finishClone(context.Background(), opts, "", context.Canceled, func() {})
	if _, err := os.Stat(cloneDir); !os.IsNotExist(err) {
		t.Errorf("finishClone left the abandoned clone dir: err = %v", err)
	}
}

// Disabling the only completed clone ends dev mode even while another clone
// is still in progress.
func TestSource_DisableLastCompletedClone_WithCloneInProgress_EndsDevMode(t *testing.T) {
	remote := newFixtureRemote(t, "main", map[string]string{"taskset.yaml": emptyFixtureTaskSet})
	src := newTestSourceWithRemote(t, "ns", remote, "main")
	ctx := context.Background()
	if err := src.SetDevMode(ctx, true, DevModeOpts{Branch: "fix/a", Base: "main", RunID: "run-a"}); err != nil {
		t.Fatalf("enable run-a: %v", err)
	}
	pendingDir := filepath.Join(src.DataDir(), "dev-clones", src.Namespace(), "run-b")
	src.mu.Lock()
	src.clones["run-b"] = cloneState{cloneDir: pendingDir}
	src.mu.Unlock()

	if err := src.SetDevMode(ctx, false, DevModeOpts{RunID: "run-a"}); err != nil {
		t.Fatalf("disable run-a: %v", err)
	}
	if src.DevMode() {
		t.Error("DevMode() = true with no completed clone left")
	}
	if p := src.DevRootPath(); p != "" {
		t.Errorf("DevRootPath = %q, want empty", p)
	}

	_ = src.finishClone(ctx, DevModeOpts{Branch: "fix/b", RunID: "run-b"}, "", errors.New("clone failed"), func() {})
	if src.DevMode() {
		t.Error("DevMode() = true after the in-progress clone failed")
	}
}

// A failed clone keeps its run ID busy, and Remove waits, until its directory
// has been deleted.
func TestSource_FailedClone_RunIDBusyUntilDirRemoved(t *testing.T) {
	src := newTestSource(t, "ns", "/tmp/fixture-taskset.yaml")
	const runID = "run-failed"
	opts := DevModeOpts{Branch: "fix/failed", RunID: runID}
	cloneDir := filepath.Join(src.DataDir(), "dev-clones", src.Namespace(), runID)
	if err := os.MkdirAll(cloneDir, 0o755); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	src.beforeRemoveCloneDir = func(string) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
	}
	done := make(chan struct{})
	src.mu.Lock()
	src.clones = map[string]cloneState{runID: {cloneDir: cloneDir, done: done}}
	src.mu.Unlock()

	go func() {
		_ = src.finishClone(context.Background(), opts, "", errors.New("clone failed"), func() { close(done) })
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("finishClone never reached the directory removal")
	}

	if err := src.SetDevMode(context.Background(), true, opts); !errors.Is(err, ErrDevModeBusy) {
		close(release)
		t.Fatalf("enable during failed-clone cleanup = %v, want ErrDevModeBusy", err)
	}
	removed := make(chan struct{})
	go func() { src.Remove(); close(removed) }()
	select {
	case <-removed:
		close(release)
		t.Fatal("Remove returned before the failed clone's directory was deleted")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-removed:
	case <-time.After(5 * time.Second):
		t.Fatal("Remove never returned")
	}
	if _, err := os.Stat(cloneDir); !os.IsNotExist(err) {
		t.Errorf("failed clone left on disk: err = %v", err)
	}
}
