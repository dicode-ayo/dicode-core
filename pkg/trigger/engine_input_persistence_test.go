package trigger

import (
	"context"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

// fakeRunner is a minimal in-memory TaskRunner for testing the engine's
// persist hook without needing the local-storage task registered.
type fakeRunner struct {
	store map[string]string
}

func (r *fakeRunner) RunTaskSync(_ context.Context, _ string, params map[string]string) (any, error) {
	switch params["op"] {
	case "put":
		r.store[params["key"]] = params["value"]
		return map[string]any{"ok": true}, nil
	case "get":
		v, ok := r.store[params["key"]]
		if !ok {
			return map[string]any{"ok": true, "value": ""}, nil
		}
		return map[string]any{"ok": true, "value": v}, nil
	case "delete":
		delete(r.store, params["key"])
		return map[string]any{"ok": true}, nil
	}
	return nil, nil
}

// newFakeInputStore returns an InputStore backed by the fakeRunner with a
// deterministic 32-byte key.
func newFakeInputStore(runner *fakeRunner, storageTaskID string) *registry.InputStore {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return registry.NewInputStore(registry.NewInputCrypto(key), runner, storageTaskID)
}

func TestEngine_PersistsInputOnRunStart(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	runner := &fakeRunner{store: map[string]string{}}
	is := newFakeInputStore(runner, "fake-storage")
	e.engine.SetInputStore(is)

	spec := writeTask(t, dir, "user-task",
		`export default async ({ params }: any) => params.get("greeting");`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(spec)

	runID, err := e.engine.FireManual(context.Background(), "user-task", map[string]string{"greeting": "hello"})
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	run := waitForTerminal(t, e.engine, runID, 30*time.Second)

	// Verify the runs row has input_storage_key set.
	got, err := e.reg.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.InputStorageKey == "" {
		t.Errorf("InputStorageKey not set on run %s (status=%s)", runID, run.Status)
	}
	// The storage backend must have the blob.
	if _, ok := runner.store[got.InputStorageKey]; !ok {
		t.Errorf("input not persisted in fakeRunner.store; keys = %v", keys(runner.store))
	}
	// stored_at within the last 30 seconds.
	if got.InputStoredAt == 0 || time.Now().Unix()-got.InputStoredAt > 30 {
		t.Errorf("InputStoredAt looks wrong: %d", got.InputStoredAt)
	}
	// Size should be > 0.
	if got.InputSize <= 0 {
		t.Errorf("InputSize should be > 0, got %d", got.InputSize)
	}
}

func TestEngine_SkipsPersistenceForStorageTask(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	runner := &fakeRunner{store: map[string]string{}}
	const storageID = "buildin/local-storage-test"
	is := newFakeInputStore(runner, storageID)
	e.engine.SetInputStore(is)

	spec := writeTask(t, dir, storageID,
		`export default async () => ({ ok: true });`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(spec)

	runID, err := e.engine.FireManual(context.Background(), storageID, nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	waitForTerminal(t, e.engine, runID, 30*time.Second)

	got, err := e.reg.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.InputStorageKey != "" {
		t.Errorf("expected no persistence for storage task; got key %q", got.InputStorageKey)
	}
}

func TestEngine_SkipsPersistenceForCleanupTask(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	runner := &fakeRunner{store: map[string]string{}}
	is := newFakeInputStore(runner, "fake-storage")
	e.engine.SetInputStore(is)

	spec := writeTask(t, dir, "buildin/run-inputs-cleanup",
		`export default async () => ({ cleaned: 0 });`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(spec)

	runID, err := e.engine.FireManual(context.Background(), "buildin/run-inputs-cleanup", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	waitForTerminal(t, e.engine, runID, 30*time.Second)

	got, err := e.reg.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.InputStorageKey != "" {
		t.Errorf("expected no persistence for cleanup task; got key %q", got.InputStorageKey)
	}
}

func TestEngine_SkipsPersistenceWhenOptedOut(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	runner := &fakeRunner{store: map[string]string{}}
	is := newFakeInputStore(runner, "fake-storage")
	e.engine.SetInputStore(is)

	disabled := false
	spec := writeTask(t, dir, "opt-out-task",
		`export default async () => "skipped";`,
		task.TriggerConfig{Manual: true})
	spec.RunInputs = &task.RunInputsTaskOverride{Enabled: &disabled}
	_ = e.reg.Register(spec)

	runID, err := e.engine.FireManual(context.Background(), "opt-out-task", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	waitForTerminal(t, e.engine, runID, 30*time.Second)

	got, err := e.reg.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.InputStorageKey != "" {
		t.Errorf("expected no persistence for opted-out task; got key %q", got.InputStorageKey)
	}
}

func TestEngine_NilInputStore_NoOp(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	// Explicitly do NOT set an InputStore.

	spec := writeTask(t, dir, "plain-task",
		`export default async () => "ok";`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(spec)

	runID, err := e.engine.FireManual(context.Background(), "plain-task", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	waitForTerminal(t, e.engine, runID, 30*time.Second)

	got, err := e.reg.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.InputStorageKey != "" {
		t.Errorf("expected no persistence with nil InputStore; got key %q", got.InputStorageKey)
	}
}

// keys returns the keys of a map for use in error messages.
func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
