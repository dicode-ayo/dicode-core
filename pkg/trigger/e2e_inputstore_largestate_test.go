package trigger

// Drives the run-input offload substrate end-to-end against the REAL
// vendored local-storage Deno task — no mock TaskRunner — with a
// multi-megabyte payload shaped like a suspended AI conversation's cumulative
// state. This is the "validate the reused building block before building on
// it" step for resume_state offload (#570): InputStore + local-storage already
// round-trip small run inputs in production, but nothing exercised the large
// blob path through the actual storage task, encryption, and the IPC frame.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

// buildinLocalStorageDir anchors the vendored local-storage task via
// runtime.Caller (this file lives in pkg/trigger, so the repo root is ../..).
func buildinLocalStorageDir(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot anchor local-storage path")
	}
	dir := filepath.Join(filepath.Dir(self), "testdata", "local-storage")
	if _, err := os.Stat(filepath.Join(dir, "task.yaml")); err != nil {
		t.Fatalf("local-storage task.yaml not found at %s: %v", dir, err)
	}
	return dir
}

// TestE2E_Suspend_LargeStateRoundTrip drives the OTHER large-frame path that
// #570 depends on: a real Deno task that dicode.suspend()s with a ~500 KB
// state blob. The suspend state travels task→daemon through the same shim
// frame writer as a return value, so before the #591 partial-write fix this
// frame is truncated, the daemon blocks on it, and the run is SIGKILLed
// instead of reaching `suspended`.
func TestE2E_Suspend_LargeStateRoundTrip(t *testing.T) {
	e := newTestEnv(t) // real Deno runtime; skips if deno unavailable

	dir := t.TempDir()
	script := `export default async function main({ dicode }) {
  const big = "S".repeat(500 * 1024); // ~500 KB, over the ~128 KB single-write cliff
  await dicode.suspend({ to: "next", state: { blob: big }, schema: { type: "object" } });
}
export const steps = { next: async () => ({ done: true }) };
`
	if err := os.WriteFile(filepath.Join(dir, "task.ts"), []byte(script), 0o644); err != nil {
		t.Fatalf("write task.ts: %v", err)
	}
	spec := &task.Spec{
		ID: "large-suspend", Name: "large-suspend", Runtime: task.RuntimeDeno,
		TaskDir: dir, Trigger: task.TriggerConfig{Manual: true}, Enabled: true,
	}
	if err := e.reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	runID, err := e.engine.FireManual(context.Background(), "large-suspend", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	run := waitStatus(t, e.reg, runID, registry.StatusSuspended)

	if len(run.ResumeState) < 500*1024 {
		t.Fatalf("resume state = %d bytes; large suspend blob was truncated or lost", len(run.ResumeState))
	}
	if !bytes.Contains(run.ResumeState, []byte(strings.Repeat("S", 4096))) {
		t.Error("large suspend blob missing from persisted resume state")
	}
	t.Logf("suspended with %d bytes of resume state", len(run.ResumeState))
}

func TestE2E_InputStore_LargeResumeStateRoundTrip(t *testing.T) {
	e := newTestEnv(t) // real Deno runtime; skips if deno unavailable
	ctx := context.Background()

	// Resolve ${DATADIR} (root default + the fs permission grant) to a temp
	// dir so the storage task can write there under its restrictive --allow-write.
	dataDir := t.TempDir()
	spec, err := task.LoadDirWithVars(buildinLocalStorageDir(t), map[string]string{
		task.VarDataDir: dataDir,
	})
	if err != nil {
		t.Fatalf("load local-storage: %v", err)
	}
	// InputStore addresses the storage task by this exact id.
	spec.ID = "buildin/local-storage"
	spec.Name = "buildin/local-storage"
	if err := spec.Validate(); err != nil {
		t.Fatalf("re-validate: %v", err)
	}
	if err := e.reg.Register(spec); err != nil {
		t.Fatalf("register local-storage: %v", err)
	}

	// Deterministic 32-byte key, matching the other input-persistence tests.
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	runner := NewInputStoreTaskRunner(e.engine)
	store := registry.NewInputStore(registry.NewInputCrypto(key), runner, "buildin/local-storage")

	// A ~4 MB payload shaped like a multi-turn conversation. A unique marker
	// rides in the last message so we can prove (a) it survives the round-trip
	// and (b) it is NOT present in the on-disk blob (i.e. actually encrypted).
	const marker = "RESUME_STATE_PLAINTEXT_MARKER_7f3a"
	const filler = "x"
	chunk := strings.Repeat(filler, 4096)
	msgs := make([]map[string]string, 0, 1001)
	for i := 0; i < 1000; i++ {
		msgs = append(msgs, map[string]string{"role": "assistant", "content": chunk})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": marker})
	body, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	t.Logf("payload plaintext body: %d bytes", len(body))
	in := registry.PersistedInput{Source: "manual", BodyKind: "json", Body: body}

	// InputCrypto binds the AEAD's AAD to the runID parsed as a UUID (real
	// runs always are); resume_state offload would key by the root-run UUID.
	const runID = "11111111-1111-1111-1111-111111111111"

	// --- Persist ---
	storedKey, size, storedAt, err := store.Persist(ctx, runID, in)
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if storedKey != "run-inputs/"+runID {
		t.Errorf("key = %q, want run-inputs/%s", storedKey, runID)
	}
	if size == 0 {
		t.Error("ciphertext size = 0")
	}
	t.Logf("stored ciphertext: %d bytes, key %q", size, storedKey)

	// --- On-disk blob must exist and be ciphertext (no plaintext leak) ---
	blobPath := filepath.Join(dataDir, "run-inputs", runID+".bin")
	raw, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("read stored blob at %s: %v", blobPath, err)
	}
	if len(raw) == 0 {
		t.Fatal("stored blob is empty")
	}
	if bytes.Contains(raw, []byte(marker)) {
		t.Error("plaintext marker found in stored blob — payload was not encrypted")
	}
	if bytes.Contains(raw, []byte(strings.Repeat(filler, 64))) {
		t.Error("plaintext filler found in stored blob — payload was not encrypted")
	}

	// --- Fetch: byte-identical round-trip ---
	out, err := store.Fetch(ctx, runID, storedKey, storedAt)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(out.Body, in.Body) {
		t.Errorf("round-trip body mismatch: got %d bytes, want %d", len(out.Body), len(in.Body))
	}
	if !bytes.Contains(out.Body, []byte(marker)) {
		t.Error("marker missing after round-trip")
	}
	if out.Source != in.Source || out.BodyKind != in.BodyKind {
		t.Errorf("metadata mismatch: got source=%q kind=%q", out.Source, out.BodyKind)
	}

	// --- Delete: blob gone, Fetch reports unavailable ---
	if err := store.Delete(ctx, storedKey); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(blobPath); !os.IsNotExist(err) {
		t.Errorf("blob still on disk after Delete: %v", err)
	}
	if _, err := store.Fetch(ctx, runID, storedKey, storedAt); err != registry.ErrInputUnavailable {
		t.Errorf("Fetch after Delete: err = %v, want ErrInputUnavailable", err)
	}
}
