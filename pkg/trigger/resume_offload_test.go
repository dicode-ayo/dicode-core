package trigger

import (
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"testing"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// mockStorageRunner is a minimal in-memory registry.TaskRunner double for the
// resume-state offload's storage task, used in place of the real
// buildin/local-storage Deno task. Mirrors pkg/registry's own unexported
// mockRunner (can't reuse it — different package, unexported).
type mockStorageRunner struct {
	mu      sync.Mutex
	store   map[string]string
	putErr  error // when set, every "put" fails — models a storage-task outage
	puts    int
	gets    int
	deletes int
}

func newMockStorageRunner() *mockStorageRunner {
	return &mockStorageRunner{store: map[string]string{}}
}

func (m *mockStorageRunner) RunTaskSync(_ context.Context, _ string, params map[string]string) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch params["op"] {
	case "put":
		m.puts++
		if m.putErr != nil {
			return nil, m.putErr
		}
		m.store[params["key"]] = params["value"]
		return map[string]any{"ok": true}, nil
	case "get":
		m.gets++
		v, ok := m.store[params["key"]]
		if !ok {
			return map[string]any{"ok": true, "value": ""}, nil
		}
		return map[string]any{"ok": true, "value": v}, nil
	case "delete":
		m.deletes++
		delete(m.store, params["key"])
		return map[string]any{"ok": true}, nil
	}
	return nil, errors.New("mockStorageRunner: unknown op")
}

func (m *mockStorageRunner) has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.store[key]
	return ok
}

// resumeStateTestCrypto returns a fixed 32-byte InputCrypto for deterministic
// tests — mirrors pkg/registry's newTestInputCrypto (unexported there too).
func resumeStateTestCrypto() *registry.InputCrypto {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return registry.NewInputCrypto(key)
}

// TestSuspend_SmallState_StaysInline_NoOffloadCall locks in the fast path:
// with a ResumeStateStore wired but the state under threshold, suspendRun
// must NOT call the storage task at all, and resume_state stays inline —
// unchanged behavior from before #570.
func TestSuspend_SmallState_StaysInline_NoOffloadCall(t *testing.T) {
	eng, reg := newSuspendEnv(t, &suspendExec{}) // suspends with `{"step":"ask_name"}` (20 bytes)
	runner := newMockStorageRunner()
	eng.SetResumeStateStore(registry.NewResumeStateStore(resumeStateTestCrypto(), runner, "buildin/local-storage", "/data/resume-state"))
	eng.SetResumeStateThresholdBytes(1024) // well above the 20-byte fixture state

	spec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}, Enabled: true}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	runID, err := eng.FireManual(context.Background(), "wiz", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	run := waitStatus(t, reg, runID, registry.StatusSuspended)

	if string(run.ResumeState) != `{"step":"ask_name"}` {
		t.Errorf("resume_state = %q, want inline state", run.ResumeState)
	}
	if run.ResumeStateStorageKey != "" {
		t.Errorf("resume_state_storage_key = %q, want empty (state is under threshold)", run.ResumeStateStorageKey)
	}
	if runner.puts != 0 {
		t.Errorf("storage task put called %d times, want 0 for a small state", runner.puts)
	}
}

// TestSuspend_LargeState_OffloadsAndResumeRehydratesTransparently is the
// required offload-write-then-resume round trip: a state over threshold gets
// written to the storage task BEFORE the row lands suspended (resume_state
// column stays nil, only the offload columns are set), and ResumeRun
// transparently resolves the reference back to the exact original bytes
// before handing it to the continuation — the task-facing contract is
// unchanged by offload having happened.
func TestSuspend_LargeState_OffloadsAndResumeRehydratesTransparently(t *testing.T) {
	exec := &suspendExec{}
	eng, reg := newSuspendEnv(t, exec)
	runner := newMockStorageRunner()
	eng.SetResumeStateStore(registry.NewResumeStateStore(resumeStateTestCrypto(), runner, "buildin/local-storage", "/data/resume-state"))
	eng.SetResumeStateThresholdBytes(4) // the 20-byte fixture state now qualifies as "large"

	spec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}, Enabled: true}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	runID, err := eng.FireManual(context.Background(), "wiz", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	run := waitStatus(t, reg, runID, registry.StatusSuspended)

	// Offloaded: resume_state column is nil, offload columns populated, and
	// the storage task actually received the write.
	if run.ResumeState != nil {
		t.Errorf("resume_state = %q, want nil (offloaded)", run.ResumeState)
	}
	if run.ResumeStateStorageKey == "" {
		t.Fatal("resume_state_storage_key empty, want set")
	}
	if run.ResumeStateStorageKey != "resume-state/"+runID {
		t.Errorf("resume_state_storage_key = %q, want %q", run.ResumeStateStorageKey, "resume-state/"+runID)
	}
	if runner.puts != 1 {
		t.Errorf("storage task put called %d times, want 1", runner.puts)
	}
	if !runner.has(run.ResumeStateStorageKey) {
		t.Fatal("blob not actually present in the storage backend")
	}
	// The stored blob must be ciphertext, not the plaintext state.
	stored, _ := base64.StdEncoding.DecodeString(runner.store[run.ResumeStateStorageKey])
	if string(stored) == `{"step":"ask_name"}` {
		t.Error("stored blob is plaintext, not encrypted")
	}

	newID, err := eng.ResumeRun(context.Background(), run.ResumeToken, []byte(`{"project_name":"acme"}`))
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}
	waitStatus(t, reg, newID, registry.StatusSuccess)

	// The continuation saw the FULL original state — transparently rehydrated,
	// exactly as if it had never been offloaded.
	exec.mu.Lock()
	defer exec.mu.Unlock()
	if string(exec.seenResumeState) != `{"step":"ask_name"}` {
		t.Errorf("continuation resume_state = %q, want the original inline value", exec.seenResumeState)
	}
	if runner.gets != 1 {
		t.Errorf("storage task get called %d times, want 1", runner.gets)
	}
}

// TestSuspend_OffloadWriteFailure_FailsSuspendLoudly is the required
// write-failure-fails-suspend case: if the durable blob write fails, the run
// must end up StatusFailure — never StatusSuspended with a resume_state (or
// storage key) pointing at a blob that was never written. This is the
// concrete regression the "write blob before suspending, fail loudly on
// error" ordering guarantees.
func TestSuspend_OffloadWriteFailure_FailsSuspendLoudly(t *testing.T) {
	eng, reg := newSuspendEnv(t, &suspendExec{})
	runner := newMockStorageRunner()
	runner.putErr = errors.New("storage backend unreachable")
	eng.SetResumeStateStore(registry.NewResumeStateStore(resumeStateTestCrypto(), runner, "buildin/local-storage", "/data/resume-state"))
	eng.SetResumeStateThresholdBytes(4) // force offload

	spec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}, Enabled: true}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	runID, err := eng.FireManual(context.Background(), "wiz", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	run := waitStatus(t, reg, runID, registry.StatusFailure)

	if run.ResumeStateStorageKey != "" {
		t.Errorf("resume_state_storage_key = %q, want empty — a failed write must never leave a dangling reference", run.ResumeStateStorageKey)
	}
	if run.ResumeState != nil {
		t.Errorf("resume_state = %q, want nil on a failed suspend", run.ResumeState)
	}
	if run.ResumeToken != "" {
		t.Errorf("resume_token = %q, want empty — no token should be persisted for a failed suspend", run.ResumeToken)
	}
}

// TestResumeRun_MissingOffloadedBlob_FailsWithoutConsumingToken covers the
// defensive case where the storage-task row exists but the referenced blob
// is gone (e.g. GC'd out from under a still-suspended row, or corrupted
// storage). Resume must fail rather than hand the task an empty/wrong state,
// and — like the deregistered-task and fire-guard-pending cases — must NOT
// consume the single-use token, so a retry after the storage issue is fixed
// can still succeed.
func TestResumeRun_MissingOffloadedBlob_FailsWithoutConsumingToken(t *testing.T) {
	eng, reg := newSuspendEnv(t, &suspendExec{})
	runner := newMockStorageRunner()
	eng.SetResumeStateStore(registry.NewResumeStateStore(resumeStateTestCrypto(), runner, "buildin/local-storage", "/data/resume-state"))
	eng.SetResumeStateThresholdBytes(4)

	spec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}, Enabled: true}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	runID, err := eng.FireManual(context.Background(), "wiz", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	run := waitStatus(t, reg, runID, registry.StatusSuspended)

	// Simulate the blob vanishing (GC race / storage corruption) without
	// touching the runs row.
	runner.mu.Lock()
	delete(runner.store, run.ResumeStateStorageKey)
	runner.mu.Unlock()

	if _, err := eng.ResumeRun(context.Background(), run.ResumeToken, nil); err == nil {
		t.Fatal("expected ResumeRun to fail when the offloaded blob is missing")
	}

	still, gerr := reg.GetRun(context.Background(), runID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if still.Status != registry.StatusSuspended {
		t.Errorf("status = %q, want suspended (token must not be consumed on a fetch failure)", still.Status)
	}
	if still.ResumeToken != run.ResumeToken {
		t.Errorf("resume token changed: %q", still.ResumeToken)
	}
}

// TestResumeRun_EagerlyDeletesOffloadedBlobAfterSuccessfulResume covers the
// GC eager-delete path: once a resume successfully rehydrates and consumes
// the single-use token, the now-orphaned blob is proactively removed rather
// than waiting for the TTL sweep, and the runs row's offload columns are
// cleared to reflect it.
func TestResumeRun_EagerlyDeletesOffloadedBlobAfterSuccessfulResume(t *testing.T) {
	eng, reg := newSuspendEnv(t, &suspendExec{})
	runner := newMockStorageRunner()
	eng.SetResumeStateStore(registry.NewResumeStateStore(resumeStateTestCrypto(), runner, "buildin/local-storage", "/data/resume-state"))
	eng.SetResumeStateThresholdBytes(4)

	spec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}, Enabled: true}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	runID, err := eng.FireManual(context.Background(), "wiz", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	run := waitStatus(t, reg, runID, registry.StatusSuspended)
	blobKey := run.ResumeStateStorageKey

	newID, err := eng.ResumeRun(context.Background(), run.ResumeToken, nil)
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}
	waitStatus(t, reg, newID, registry.StatusSuccess)

	// Give the best-effort post-resume cleanup a moment (it runs inline in
	// ResumeRun before returning, but GetRun below re-fetches to be safe
	// against any future async refactor).
	after, gerr := reg.GetRun(context.Background(), runID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if after.ResumeStateStorageKey != "" {
		t.Errorf("resume_state_storage_key = %q, want cleared after eager GC", after.ResumeStateStorageKey)
	}
	if runner.has(blobKey) {
		t.Error("blob still present in storage backend after eager GC")
	}
	if runner.deletes != 1 {
		t.Errorf("storage task delete called %d times, want 1", runner.deletes)
	}
}

// TestSuspendRun_OrphanedBlobCleanedUpWhenRowLeftRunning locks in a leak
// found during code review: SuspendRun's UPDATE is guarded on status =
// running, so if a concurrent finalize (kill / shutdown drain) has already
// moved the run out of running by the time suspendRun's registry.SuspendRun
// call lands, the row is left untouched (suspended=false, err=nil) — but the
// blob was already durably written to storage in the step before. Nothing
// else can ever find that blob: the eager GC in ResumeRun only runs on an
// actual resume of this row, and the TTL sweep only scans rows that have
// resume_state_storage_key set, which this row never got. suspendRun must
// notice the false/nil return and delete the now-orphaned blob itself.
func TestSuspendRun_OrphanedBlobCleanedUpWhenRowLeftRunning(t *testing.T) {
	eng, reg := newSuspendEnv(t, &suspendExec{})
	runner := newMockStorageRunner()
	eng.SetResumeStateStore(registry.NewResumeStateStore(resumeStateTestCrypto(), runner, "buildin/local-storage", "/data/resume-state"))
	eng.SetResumeStateThresholdBytes(4) // force offload

	runID, err := reg.StartRun(context.Background(), "wiz", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	// Simulate a concurrent finalize (e.g. a kill or shutdown drain) that
	// moved the run out of `running` before the suspend below lands.
	if err := reg.FinishRun(context.Background(), runID, registry.StatusCancelled); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	opts := &pkgruntime.RunOptions{RunID: runID}
	result := &pkgruntime.RunResult{Suspended: true, ResumeState: []byte(`{"step":"ask_name"}`)}
	suspended, serr := eng.suspendRun(opts, result)
	if serr != nil {
		t.Fatalf("suspendRun: unexpected error %v", serr)
	}
	if suspended {
		t.Fatal("suspendRun reported suspended=true for a run that had already left `running`")
	}

	// The blob was durably written before the (no-op) row update — it must
	// not survive as an orphan with nothing left to reference it.
	blobKey := "resume-state/" + runID
	if runner.has(blobKey) {
		t.Error("orphaned resume-state blob was not cleaned up")
	}
	if runner.deletes != 1 {
		t.Errorf("storage task delete called %d times, want 1 (orphan cleanup)", runner.deletes)
	}

	// The row itself must stay exactly as the concurrent finalize left it —
	// suspendRun's cleanup must not resurrect or otherwise touch it.
	after, gerr := reg.GetRun(context.Background(), runID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if after.Status != registry.StatusCancelled {
		t.Errorf("status = %q, want unchanged %q", after.Status, registry.StatusCancelled)
	}
}

// TestSuspendRun_OrphanedBlobCleanedUpOnGenuineSuspendRunError covers the
// other half of the same leak: the first fix only cleaned up when
// registry.SuspendRun returned (false, nil) — a real SuspendRun error
// (suspended=false, err!=nil, e.g. a transient DB failure on the UPDATE
// itself) left the just-persisted blob just as unreachable, but the cleanup
// condition required err == nil and so skipped it. Forces a genuine
// SuspendRun error by closing the underlying DB after the run row exists but
// before suspendRun's UPDATE runs.
func TestSuspendRun_OrphanedBlobCleanedUpOnGenuineSuspendRunError(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	reg := registry.New(d)
	eng := New(reg, &suspendExec{}, zap.NewNop())
	runner := newMockStorageRunner()
	eng.SetResumeStateStore(registry.NewResumeStateStore(resumeStateTestCrypto(), runner, "buildin/local-storage", "/data/resume-state"))
	eng.SetResumeStateThresholdBytes(4) // force offload

	runID, err := reg.StartRun(context.Background(), "wiz", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	if err := d.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	opts := &pkgruntime.RunOptions{RunID: runID}
	result := &pkgruntime.RunResult{Suspended: true, ResumeState: []byte(`{"step":"ask_name"}`)}
	suspended, serr := eng.suspendRun(opts, result)
	if serr == nil {
		t.Fatal("expected a genuine SuspendRun error against a closed DB")
	}
	if suspended {
		t.Fatal("suspendRun reported suspended=true despite the SuspendRun error")
	}

	// The blob was durably written (via the mock storage runner, unaffected
	// by the closed real DB) before the failed UPDATE — it must still be
	// cleaned up rather than left orphaned.
	blobKey := "resume-state/" + runID
	if runner.has(blobKey) {
		t.Error("orphaned resume-state blob was not cleaned up after a genuine SuspendRun error")
	}
	if runner.deletes != 1 {
		t.Errorf("storage task delete called %d times, want 1 (orphan cleanup)", runner.deletes)
	}
}
