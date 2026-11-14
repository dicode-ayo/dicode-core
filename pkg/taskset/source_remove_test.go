package taskset

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	if _, err := os.Stat(nsDir); !os.IsNotExist(err) {
		t.Errorf("dev-clones dir still exists after Remove: err = %v", err)
	}
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
	placeholder := cloneState{cloneDir: cloneDir}
	src.mu.Lock()
	src.clones = map[string]cloneState{runID: placeholder}
	src.mu.Unlock()

	src.Remove()

	devPath, cloneErr := src.enableClone(ctx, opts)
	if cloneErr != nil {
		t.Fatalf("enableClone: %v", cloneErr)
	}
	if _, err := os.Stat(cloneDir); err != nil {
		t.Fatalf("test setup: clone dir missing after enableClone: %v", err)
	}
	if err := src.finishClone(ctx, opts, placeholder, devPath, nil); err == nil {
		t.Fatal("finishClone after Remove = nil, want an error")
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
