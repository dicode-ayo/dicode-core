package python

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	uvpkg "github.com/dicode/dicode/pkg/uv"
)

type recordingSecrets struct {
	mu   sync.Mutex
	vals map[string]string
	ops  []string
}

func (r *recordingSecrets) List(context.Context) ([]string, error) { return nil, nil }

func (r *recordingSecrets) Has(_ context.Context, key string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.vals[key]
	return ok, nil
}

func (r *recordingSecrets) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vals[key] = value
	r.ops = append(r.ops, "set:"+key+"="+value)
	return nil
}

func (r *recordingSecrets) Delete(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.vals, key)
	r.ops = append(r.ops, "delete:"+key)
	return nil
}

type fixedDeriver struct{}

func (fixedDeriver) DeriveSubKey(string) ([]byte, error) { return make([]byte, 32), nil }

func (fixedDeriver) DeriveSubKeyHKDF(ctx string) ([]byte, error) {
	k := make([]byte, 32)
	copy(k, ctx)
	return k, nil
}

// memStorage is an in-memory storage task for InputStore and
// ResumeStateStore, recording every delete.
type memStorage struct {
	mu      sync.Mutex
	blobs   map[string]string
	deleted []string
}

func (m *memStorage) RunTaskSync(_ context.Context, _ string, p map[string]string) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch p["op"] {
	case "put":
		m.blobs[p["key"]] = p["value"]
	case "get":
		return map[string]any{"ok": true, "value": m.blobs[p["key"]]}, nil
	case "delete":
		delete(m.blobs, p["key"])
		m.deleted = append(m.deleted, p["key"])
	}
	return map[string]any{"ok": true}, nil
}

func (m *memStorage) deletedKeys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.deleted...)
}

func runIDs(v any) []string {
	rows, _ := v.([]any)
	var out []string
	for _, r := range rows {
		if m, ok := r.(map[string]any); ok {
			id, _ := m["runID"].(string)
			out = append(out, id)
		}
	}
	return out
}

// TestExecute_SDKVerbs runs a real Python task through the crypto, secrets
// write and runs.* retention verbs over IPC, and checks the stores behind
// them.
func TestExecute_SDKVerbs(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	uv, err := uvpkg.EnsureUv("")
	if err != nil {
		t.Skipf("uv provisioning failed (offline?): %v", err)
	}
	rt, reg := newTestRuntime(t)
	sec := &recordingSecrets{vals: map[string]string{}}
	rt.SetSecretsManager(sec)
	rt.SetCryptoDeriver(fixedDeriver{})
	storage := &memStorage{blobs: map[string]string{}}
	storeKey := make([]byte, 32)
	inputs := registry.NewInputStore(registry.NewInputCrypto(storeKey), storage, "mem-storage")
	rt.SetInputStore(inputs)
	rt.SetResumeStateStore(registry.NewResumeStateStore(registry.NewInputCrypto(storeKey), storage, "mem-storage", "/unused"))
	ex := rt.NewExecutor(uv)

	spec := writePythonTask(t, "examples/sdk-verbs", `
async def main():
    pt = b"\x00hello\xffworld"
    ct = dicode.crypto.encrypt("verbs", pt)
    assert isinstance(ct, bytes) and ct != pt
    rt = dicode.crypto.decrypt("verbs", ct)
    rt_async = await dicode.crypto.decrypt_async("verbs", await dicode.crypto.encrypt_async("verbs", pt))
    denied = ""
    try:
        dicode.crypto.encrypt("other", pt)
    except RuntimeError as e:
        denied = str(e)

    dicode.secrets_set("SDK_VERBS_KEY", "v1")
    await dicode.secrets_set_async("SDK_VERBS_KEY2", "v2")
    dicode.secrets_delete("SDK_VERBS_KEY")
    await dicode.secrets_delete_async("SDK_VERBS_KEY2")

    target = params.get("target")
    other = params.get("other")
    pinned = dicode.runs.pin_input(target)
    dicode.runs.pin_input(other)
    unpinned = await dicode.runs.unpin_input_async(other)
    cleared = dicode.runs.delete_inputs([target])

    old = params.get("old")
    expired_after = dicode.runs.list_expired(before_ts=1500.7)
    expired_before = await dicode.runs.list_expired_async(before_ts=999.9)

    stored = params.get("stored")
    fetched = dicode.runs.get_input(stored)
    fetched_async = await dicode.runs.get_input_async(stored)
    deleted_input = dicode.runs.delete_input(stored)

    suspended = params.get("suspended")
    rs_after = dicode.runs.list_expired_resume_state(before_ts=1500.2)
    rs_before = await dicode.runs.list_expired_resume_state_async(before_ts=999.5)
    deleted_rs = dicode.runs.delete_resume_state(suspended)
    rs_left = dicode.runs.list_expired_resume_state(before_ts=1500.2)
    return {
        "roundtrip": rt == pt,
        "roundtrip_async": rt_async == pt,
        "denied": denied,
        "pinned": pinned,
        "unpinned": unpinned,
        "cleared": cleared,
        "expired_after": expired_after,
        "expired_before": expired_before,
        "fetched": fetched,
        "fetched_async": fetched_async,
        "deleted_input": deleted_input,
        "rs_after": rs_after,
        "rs_before": rs_before,
        "deleted_rs": deleted_rs,
        "rs_left": rs_left,
    }
`)
	spec.Params = []task.Param{{Name: "target"}, {Name: "other"}, {Name: "old"}, {Name: "stored"}, {Name: "suspended"}}
	spec.Permissions = task.Permissions{Dicode: &task.DicodePermissions{
		Crypto:                     []string{"verbs"},
		SecretsWrite:               true,
		RunsPinInput:               true,
		RunsUnpinInput:             true,
		RunsListExpired:            true,
		RunsDeleteInput:            true,
		RunsGetInput:               true,
		RunsListExpiredResumeState: true,
		RunsDeleteResumeState:      true,
	}}
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}
	target, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	old, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.SetRunInput(ctx, old, "run-inputs/"+old, 10, 1000, nil); err != nil {
		t.Fatal(err)
	}
	stored, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	key, size, storedAt, err := inputs.Persist(ctx, stored, registry.PersistedInput{Source: "manual", Params: map[string]any{"who": "sdk-verbs"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.SetRunInput(ctx, stored, key, size, storedAt, nil); err != nil {
		t.Fatal(err)
	}
	suspended, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	rsKey := "resume-state/" + suspended
	storage.blobs[rsKey] = "blob"
	if ok, err := reg.SuspendRun(ctx, suspended, nil, nil, "tok-"+suspended, 1000, 0, nil, nil,
		&registry.ResumeStateBlobRef{StorageKey: rsKey, Size: 4, StoredAt: 1000}); err != nil || !ok {
		t.Fatalf("SuspendRun = %v, %v", ok, err)
	}
	if err := reg.FinishRun(ctx, suspended, registry.StatusSuccess); err != nil {
		t.Fatal(err)
	}
	runID, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	res, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{
		RunID: runID,
		Params: map[string]string{
			"target": target, "other": other, "old": old, "stored": stored, "suspended": suspended,
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Error != nil {
		dumpRunLogs(t, reg, runID)
		t.Fatalf("run failed: %v", res.Error)
	}
	m, ok := res.ReturnValue.(map[string]any)
	if !ok {
		dumpRunLogs(t, reg, runID)
		t.Fatalf("ReturnValue is %T, want map: %+v", res.ReturnValue, res.ReturnValue)
	}

	if m["roundtrip"] != true || m["roundtrip_async"] != true {
		t.Errorf("crypto round trip failed: %+v", m)
	}
	if d, _ := m["denied"].(string); d == "" {
		t.Errorf("encrypt under an undeclared context was not rejected")
	}
	for _, k := range []string{"pinned", "unpinned"} {
		if r, _ := m[k].(map[string]any); r["ok"] != true {
			t.Errorf("%s = %+v, want {ok: true}", k, m[k])
		}
	}
	if c, _ := m["cleared"].(map[string]any); c["ok"] != true || c["count"] != float64(1) {
		t.Errorf("cleared = %+v, want {ok: true, count: 1}", m["cleared"])
	}
	if got := runIDs(m["expired_after"]); !slices.Contains(got, old) || slices.Contains(got, stored) {
		t.Errorf("list_expired(before_ts=1500.7) = %v, want %s (stored_at 1000) and not %s (stored now)", got, old, stored)
	}
	if got := runIDs(m["expired_before"]); slices.Contains(got, old) {
		t.Errorf("list_expired(before_ts=999.9) = %v, want %s excluded", got, old)
	}
	for _, k := range []string{"fetched", "fetched_async"} {
		f, _ := m[k].(map[string]any)
		p, _ := f["params"].(map[string]any)
		if f["source"] != "manual" || p["who"] != "sdk-verbs" {
			t.Errorf("%s = %+v, want the persisted input", k, m[k])
		}
	}
	if d, _ := m["deleted_input"].(map[string]any); d["ok"] != true {
		t.Errorf("deleted_input = %+v, want {ok: true}", m["deleted_input"])
	}
	if got := runIDs(m["rs_after"]); !slices.Contains(got, suspended) {
		t.Errorf("list_expired_resume_state(before_ts=1500.2) = %v, want %s", got, suspended)
	}
	if got := runIDs(m["rs_before"]); slices.Contains(got, suspended) {
		t.Errorf("list_expired_resume_state(before_ts=999.5) = %v, want %s excluded", got, suspended)
	}
	if d, _ := m["deleted_rs"].(map[string]any); d["ok"] != true {
		t.Errorf("deleted_rs = %+v, want {ok: true}", m["deleted_rs"])
	}
	if got := runIDs(m["rs_left"]); slices.Contains(got, suspended) {
		t.Errorf("list_expired_resume_state after delete = %v, want %s gone", got, suspended)
	}
	if got := storage.deletedKeys(); !slices.Contains(got, key) || !slices.Contains(got, rsKey) {
		t.Errorf("storage deletes = %v, want %s and %s", got, key, rsKey)
	}
	if r, err := reg.GetRun(ctx, stored); err != nil || r.InputStorageKey != "" {
		t.Errorf("run %s InputStorageKey = %q (err %v), want cleared", stored, r.InputStorageKey, err)
	}
	if r, err := reg.GetRun(ctx, suspended); err != nil || r.ResumeStateStorageKey != "" {
		t.Errorf("run %s ResumeStateStorageKey = %q (err %v), want cleared", suspended, r.ResumeStateStorageKey, err)
	}

	wantOps := []string{"set:SDK_VERBS_KEY=v1", "set:SDK_VERBS_KEY2=v2", "delete:SDK_VERBS_KEY", "delete:SDK_VERBS_KEY2"}
	sec.mu.Lock()
	gotOps := append([]string(nil), sec.ops...)
	sec.mu.Unlock()
	if len(gotOps) != len(wantOps) {
		t.Fatalf("secrets ops = %v, want %v", gotOps, wantOps)
	}
	for i := range wantOps {
		if gotOps[i] != wantOps[i] {
			t.Errorf("secrets op %d = %q, want %q", i, gotOps[i], wantOps[i])
		}
	}

	for id, want := range map[string]int{target: 1, other: 0} {
		run, err := reg.GetRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if run.InputPinned != want {
			t.Errorf("run %s InputPinned = %d, want %d", id, run.InputPinned, want)
		}
	}
}

// TestExecute_SDKArgTypeErrors checks that the Python SDK rejects wrongly
// typed secrets and crypto arguments with a TypeError before anything reaches
// the wire, leaving the IPC connection usable.
func TestExecute_SDKArgTypeErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	uv, err := uvpkg.EnsureUv("")
	if err != nil {
		t.Skipf("uv provisioning failed (offline?): %v", err)
	}
	rt, reg := newTestRuntime(t)
	sec := &recordingSecrets{vals: map[string]string{}}
	rt.SetSecretsManager(sec)
	rt.SetCryptoDeriver(fixedDeriver{})
	ex := rt.NewExecutor(uv)

	spec := writePythonTask(t, "examples/sdk-arg-types", `
async def main():
    errs = {}

    def record(name, fn):
        try:
            fn()
        except TypeError as e:
            errs[name] = str(e)

    async def record_async(name, coro):
        try:
            await coro
        except TypeError as e:
            errs[name] = str(e)

    record("secret_value", lambda: dicode.secrets_set("K", 123))
    record("secret_key", lambda: dicode.secrets_set(7, "v"))
    await record_async("secret_value_async", dicode.secrets_set_async("K", b"v"))
    record("encrypt_str", lambda: dicode.crypto.encrypt("verbs", "text"))
    record("encrypt_int", lambda: dicode.crypto.encrypt("verbs", 5))
    record("decrypt_int", lambda: dicode.crypto.decrypt("verbs", 5))
    await record_async("encrypt_async_int", dicode.crypto.encrypt_async("verbs", 5))
    await record_async("decrypt_async_str", dicode.crypto.decrypt_async("verbs", "x"))

    ct = dicode.crypto.encrypt("verbs", bytearray(b"still-ok"))
    after = dicode.crypto.decrypt("verbs", memoryview(ct))
    dicode.secrets_set("AFTER", "ok")
    return {"errs": errs, "after": after == b"still-ok"}
`)
	spec.Permissions = task.Permissions{Dicode: &task.DicodePermissions{
		Crypto:       []string{"verbs"},
		SecretsWrite: true,
	}}
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}
	runID, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{RunID: runID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Error != nil {
		dumpRunLogs(t, reg, runID)
		t.Fatalf("run failed: %v", res.Error)
	}
	m, ok := res.ReturnValue.(map[string]any)
	if !ok {
		dumpRunLogs(t, reg, runID)
		t.Fatalf("ReturnValue is %T, want map: %+v", res.ReturnValue, res.ReturnValue)
	}
	errs, _ := m["errs"].(map[string]any)
	for name, want := range map[string]string{
		"secret_value":       "secret value must be a str, not int",
		"secret_key":         "secret key must be a str, not int",
		"secret_value_async": "secret value must be a str, not bytes",
		"encrypt_str":        ".encode()",
		"encrypt_int":        "plaintext must be bytes, bytearray or memoryview, not int",
		"decrypt_int":        "ciphertext must be bytes, bytearray or memoryview, not int",
		"encrypt_async_int":  "plaintext must be bytes",
		"decrypt_async_str":  ".encode()",
	} {
		if got, _ := errs[name].(string); !strings.Contains(got, want) {
			t.Errorf("%s TypeError = %q, want it to contain %q", name, got, want)
		}
	}
	if m["after"] != true {
		t.Errorf("crypto after the TypeErrors failed: %+v", m)
	}
	sec.mu.Lock()
	defer sec.mu.Unlock()
	if len(sec.ops) != 1 || sec.ops[0] != "set:AFTER=ok" {
		t.Errorf("secrets ops = %v, want only set:AFTER=ok", sec.ops)
	}
}
