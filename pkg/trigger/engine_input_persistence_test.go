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

// TestEngine_DeleteInputWiredThroughRuntime verifies the Finding 1 regression:
// dicode.runs.delete_input must be able to reach the InputStore from within
// a real Deno task. Before the fix, SetInputStore was only called on the
// engine; the per-run IPC server never received the store, so delete_input
// would clear the DB columns but leave the blob orphaned in storage.
//
// This test:
//  1. Persists a run's input via the engine's startRun persist hook.
//  2. Fires a second task (the "cleanup" task) with RunsDeleteInput permission,
//     passing the first run's ID as a param.
//  3. The cleanup task calls dicode.runs.delete_input(runID).
//  4. Asserts that the blob was removed from the fakeRunner.store AND that
//     the DB columns (InputStorageKey, InputSize, InputStoredAt) are cleared.
func TestEngine_DeleteInputWiredThroughRuntime(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	runner := &fakeRunner{store: map[string]string{}}
	const storageID = "fake-storage"
	is := newFakeInputStore(runner, storageID)
	// Wire the InputStore into both the engine (persist-hook) and the deno
	// runtime (per-run IPC server). This is the wiring that Finding 1 fixed;
	// calling only e.engine.SetInputStore was insufficient — the IPC server
	// never received the store, so delete_input orphaned blobs.
	e.engine.SetInputStore(is)
	e.denoRT.SetInputStore(is)

	// Step 1: fire a task whose input will be persisted.
	dataSpec := writeTask(t, dir, "data-task",
		`export default async ({ params }: any) => params.get("val");`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(dataSpec)

	dataRunID, err := e.engine.FireManual(context.Background(), "data-task", map[string]string{"val": "hello"})
	if err != nil {
		t.Fatalf("FireManual data-task: %v", err)
	}
	waitForTerminal(t, e.engine, dataRunID, 30*time.Second)

	// Confirm the blob was persisted.
	dataRun, err := e.reg.GetRun(context.Background(), dataRunID)
	if err != nil {
		t.Fatalf("GetRun data-task: %v", err)
	}
	if dataRun.InputStorageKey == "" {
		t.Fatal("expected InputStorageKey to be set after FireManual; store not wired?")
	}
	storageKey := dataRun.InputStorageKey
	if _, ok := runner.store[storageKey]; !ok {
		t.Fatalf("blob not in fakeRunner.store; keys = %v", keys(runner.store))
	}

	// Step 2: register a task with RunsDeleteInput permission that calls
	// dicode.runs.delete_input on the runID passed as a param.
	cleanupScript := `
export default async function main({ params, dicode }: any) {
  const runID = await params.get("run_id");
  await dicode.runs.delete_input(runID);
  return { ok: true };
}`
	cleanupSpec := writeTask(t, dir, "cleanup-task", cleanupScript, task.TriggerConfig{Manual: true})
	cleanupSpec.Permissions.Dicode = &task.DicodePermissions{RunsDeleteInput: true}
	_ = e.reg.Register(cleanupSpec)

	cleanupRunID, err := e.engine.FireManual(context.Background(), "cleanup-task", map[string]string{"run_id": dataRunID})
	if err != nil {
		t.Fatalf("FireManual cleanup-task: %v", err)
	}
	cleanupRun := waitForTerminal(t, e.engine, cleanupRunID, 30*time.Second)
	if cleanupRun.Status != "success" {
		t.Fatalf("cleanup-task failed (status=%s): expected success", cleanupRun.Status)
	}

	// Step 3: assert the blob is gone from storage (not just from the DB).
	if _, ok := runner.store[storageKey]; ok {
		t.Errorf("blob still present in fakeRunner.store after delete_input; InputStore not wired into IPC server")
	}

	// Step 4: assert the DB columns are cleared.
	afterRun, err := e.reg.GetRun(context.Background(), dataRunID)
	if err != nil {
		t.Fatalf("GetRun after delete: %v", err)
	}
	if afterRun.InputStorageKey != "" {
		t.Errorf("InputStorageKey not cleared after delete_input; got %q", afterRun.InputStorageKey)
	}
	if afterRun.InputSize != 0 {
		t.Errorf("InputSize not cleared after delete_input; got %d", afterRun.InputSize)
	}
	if afterRun.InputStoredAt != 0 {
		t.Errorf("InputStoredAt not cleared after delete_input; got %d", afterRun.InputStoredAt)
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
