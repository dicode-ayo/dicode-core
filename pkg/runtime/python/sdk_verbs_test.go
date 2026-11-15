package python

import (
	"context"
	"sync"
	"testing"
	"time"

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

// TestExecute_SDKVerbs runs a real Python task through the crypto, secrets
// write and runs.* retention verbs over IPC.
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
    expired = dicode.runs.list_expired(before_ts=1)
    cleared = dicode.runs.delete_inputs([target])
    return {
        "roundtrip": rt == pt,
        "roundtrip_async": rt_async == pt,
        "denied": denied,
        "pinned": pinned,
        "unpinned": unpinned,
        "expired": expired,
        "cleared": cleared,
    }
`)
	spec.Params = []task.Param{{Name: "target"}, {Name: "other"}}
	spec.Permissions = task.Permissions{Dicode: &task.DicodePermissions{
		Crypto:          []string{"verbs"},
		SecretsWrite:    true,
		RunsPinInput:    true,
		RunsUnpinInput:  true,
		RunsListExpired: true,
		RunsDeleteInput: true,
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
	runID, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	res, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{
		RunID:  runID,
		Params: map[string]string{"target": target, "other": other},
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
	if _, ok := m["expired"]; !ok {
		t.Errorf("list_expired result missing")
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
