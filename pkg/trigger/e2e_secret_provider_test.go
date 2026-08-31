package trigger

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	denoruntime "github.com/dicode/dicode/pkg/runtime/deno"
	"github.com/dicode/dicode/pkg/runtime/envresolve"
	pythonruntime "github.com/dicode/dicode/pkg/runtime/python"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	uvpkg "github.com/dicode/dicode/pkg/uv"
	"go.uber.org/zap/zaptest"
)

// TestE2E_SecretProvider_FullChain wires the real trigger engine, the real
// Deno runtime, the real envresolve resolver, and a Deno-side provider task
// against an httptest.Server-backed Doppler-shaped upstream. It exercises the
// full chain consumer launch → preflight → batched provider spawn → real
// HTTP call to mock → output{secret:true} → IPC routing → PreResolvedEnv
// threading → consumer subprocess sees the resolved env values.
//
// Skips when Deno is unavailable so hosts without the runtime exit cleanly.
//
// What this pins down (the bugs that lived in this chain):
//   - manager.go field-stripping (NewExecutor) — the executor in dispatch
//     must observe the same providerRunner the engine wired into the
//     runtime, and the per-run secretOutputCh the engine threads through
//     RunOptions (issue #719) for the manager-as-default-executor path this
//     test exercises. See TestE2E_SecretProvider_PinnedDenoExecutor and
//     TestE2E_SecretProvider_PythonRuntime (same file) for the two paths
//     that stayed broken even after the earlier #718 fix — a pinned Deno
//     version and any Python provider — because the channel used to live on
//     shared BridgeDeps state that NewExecutor snapshotted once (as nil) at
//     daemon boot instead of being wired per-run.
//   - Issue #235 / #240 double-resolve — the provider task fires exactly
//     once per consumer launch, not once for preflight + once for dispatch.
//   - Run-log redaction — plaintext provider values never appear in logs
//     even though the SDK shim logs the secret-output call.
func TestE2E_SecretProvider_FullChain(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}

	// ── Mock upstream: Doppler-shaped JSON response.
	var upstreamCalls atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"secrets": map[string]any{
				"PG_URL":    map[string]any{"computed": "postgres://example.com/db"},
				"REDIS_URL": map[string]any{"computed": "redis://example.com:6379"},
			},
		})
	}))
	t.Cleanup(ts.Close)

	tsURL, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse ts url: %v", err)
	}

	// ── Real wiring: in-memory SQLite + registry + Deno runtime.
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	reg := registry.New(d)
	log := zaptest.NewLogger(t)

	denoRT, err := denoruntime.New(reg, secrets.Chain{}, d, log)
	if err != nil {
		t.Skipf("deno not available: %v", err)
	}

	eng := New(reg, denoRT, log)
	eng.SetSecrets(secrets.Chain{})
	eng.SetDenoRuntime(denoRT)
	denoRT.SetProviderRunner(eng)
	denoRT.SetEngine(eng)

	// ── Provider task — Doppler-shaped body, but reads UPSTREAM_URL from
	// host env so the test can point it at the httptest server. The host
	// allowlisted in permissions.net is the only test-time-dynamic value
	// (httptest assigns the port at server startup); inject via {{MOCK_HOST}}.
	const providerID = "test-secret-provider"
	providerSpec := loadFixtureTpl(t,
		"secret-provider/test-secret-provider",
		map[string]string{"MOCK_HOST": tsURL.Host},
		providerID)
	if err := reg.Register(providerSpec); err != nil {
		t.Fatalf("register provider: %v", err)
	}

	// ── Consumer task — pulls PG_URL and REDIS_URL from the provider task,
	// then echoes them via output.text so the run record carries the
	// resolved values back to the test for end-to-end assertion.
	consumerSpec := loadFixture(t, "secret-provider/test-consumer", "")
	if err := reg.Register(consumerSpec); err != nil {
		t.Fatalf("register consumer: %v", err)
	}

	// UPSTREAM_URL is host-env-allowlisted in the provider's permissions.env;
	// bare allowlist entries are forwarded from the host env into the
	// subprocess env, so t.Setenv flows through to the Deno subprocess.
	t.Setenv("UPSTREAM_URL", ts.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// ── First launch.
	runID, result, err := eng.fireSync(ctx, consumerSpec, pkgruntime.RunOptions{}, "manual")
	if err != nil {
		t.Fatalf("fireSync: %v", err)
	}
	if result == nil || result.Error != nil {
		t.Fatalf("consumer run errored: %+v", result)
	}
	if runID == "" {
		t.Fatal("empty run ID")
	}

	// (1) Consumer process saw the injected env values, end-to-end through
	// SDK shim → IPC → resolver → runtime → subprocess --allow-env.
	gotOut := result.OutputContent
	if !strings.Contains(gotOut, "PG_URL=postgres://example.com/db") ||
		!strings.Contains(gotOut, "REDIS_URL=redis://example.com:6379") {
		t.Errorf("consumer output missing resolved env values: %q", gotOut)
	}

	// (2) Run-log redaction: provider's `output(secret:true)` emits a
	// "[redacted]" log line; plaintext values must NOT appear in either
	// the provider's or the consumer's logs.
	allLogs := collectAllLogs(t, ctx, reg, "test-consumer", providerID)
	if !strings.Contains(allLogs, "[redacted]") {
		t.Errorf("expected [redacted] marker in logs, got: %s", allLogs)
	}
	for _, plaintext := range []string{"postgres://example.com/db", "redis://example.com:6379"} {
		// The consumer task itself echoes plaintext via output.text; that's
		// a structured-output value (not a log line) and lives in
		// run.output_content, not run_logs. Logs must stay clean.
		for _, line := range strings.Split(allLogs, "\n") {
			if strings.Contains(line, plaintext) {
				t.Errorf("plaintext leaked in run log line: %q", line)
			}
		}
	}

	// (3) Provider spawned exactly once for this launch (batching across
	// PG_URL+REDIS_URL + the #240 double-resolve fix).
	if got := upstreamCalls.Load(); got != 1 {
		t.Errorf("upstream should have been hit exactly once, got %d", got)
	}
}

// collectAllLogs joins logs from every run of the named task IDs into a
// single string for substring-based redaction assertions.
func collectAllLogs(t *testing.T, ctx context.Context, reg *registry.Registry, taskIDs ...string) string {
	t.Helper()
	var b strings.Builder
	for _, id := range taskIDs {
		runs, err := reg.ListRuns(ctx, id, 100)
		if err != nil {
			t.Fatalf("ListRuns(%s): %v", id, err)
		}
		for _, run := range runs {
			logs, err := reg.GetRunLogs(ctx, run.ID)
			if err != nil {
				continue
			}
			for _, le := range logs {
				b.WriteString(le.Level)
				b.WriteString(" ")
				b.WriteString(le.Message)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// captureConsumerExecutor is a minimal Executor that never spawns a
// subprocess; it just records the RunOptions the trigger engine dispatched
// it with. Used as the *consumer's* executor in tests below whose actual
// focus is the provider dispatch path (a pinned Deno executor, or a Python
// executor) — a second real subprocess runtime for the consumer would only
// add noise, since all we need to observe is whether opts.PreResolvedEnv
// carries the value the provider produced.
type captureConsumerExecutor struct {
	lastOpts atomic.Pointer[pkgruntime.RunOptions]
}

func (c *captureConsumerExecutor) Execute(_ context.Context, _ *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	captured := opts
	c.lastOpts.Store(&captured)
	return &pkgruntime.RunResult{RunID: opts.RunID}, nil
}

// newSecretUpstream starts an httptest server that answers with a
// Doppler-shaped JSON body containing the given name→value secrets, and
// counts how many times it was hit.
func newSecretUpstream(t *testing.T, values map[string]string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	secrets := make(map[string]any, len(values))
	for k, v := range values {
		secrets[k] = map[string]any{"computed": v}
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"secrets": secrets})
	}))
	t.Cleanup(ts.Close)
	return ts, &calls
}

// TestE2E_SecretProvider_PinnedDenoExecutor is the regression test for the
// first broken path documented in issue #719: a Deno provider task dispatched
// through a PINNED per-version executor (built via Runtime.NewExecutor, the
// same construction daemon.go's buildRuntimes uses whenever
// runtimes.deno.version names an installed version) rather than the manager
// itself serving as the RuntimeDeno executor.
//
// Before the fix, the per-run secret-output channel lived on shared
// BridgeDeps state that NewExecutor snapshotted (always nil) at construction
// time — long before any provider invocation ever set it — so a pinned
// executor's IPC server never saw the channel Engine.Run wired onto the
// manager, and the provider run finished with "provider completed without
// secret output". After the fix the channel flows per-run through
// RunOptions.SecretOutputCh, so every executor — pinned or not — sees it.
func TestE2E_SecretProvider_PinnedDenoExecutor(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}

	const pgURLValue = "postgres://pinned-executor.example.com/db"
	ts, upstreamCalls := newSecretUpstream(t, map[string]string{"PG_URL": pgURLValue})
	tsURL, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse ts url: %v", err)
	}

	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	reg := registry.New(d)
	log := zaptest.NewLogger(t)

	denoRT, err := denoruntime.New(reg, secrets.Chain{}, d, log)
	if err != nil {
		t.Skipf("deno not available: %v", err)
	}

	// The engine is constructed exactly like daemon.go's buildRuntimes: the
	// manager (denoRT) is the default RuntimeDeno executor at New() time...
	eng := New(reg, denoRT, log)
	eng.SetSecrets(secrets.Chain{})
	eng.SetDenoRuntime(denoRT)
	denoRT.SetProviderRunner(eng)
	denoRT.SetEngine(eng)

	// ...and then, exactly like buildRuntimes does whenever
	// runtimes.deno.version names an installed version, a per-version
	// executor silently overrides that registration.
	binPath, err := denoRT.BinaryPath(denoRT.DefaultVersion())
	if err != nil {
		t.Fatalf("resolve deno binary path: %v", err)
	}
	eng.RegisterExecutor(task.RuntimeDeno, denoRT.NewExecutor(binPath))

	// The consumer's own runtime is irrelevant to this test — it never
	// touches the pinned executor above, which serves only the provider.
	const fakeConsumerRuntime = task.Runtime("fake-consumer-for-pinned-deno-test")
	captureExec := &captureConsumerExecutor{}
	eng.RegisterExecutor(fakeConsumerRuntime, captureExec)

	const providerID = "test-secret-provider-pinned"
	providerSpec := loadFixtureTpl(t,
		"secret-provider/test-secret-provider",
		map[string]string{"MOCK_HOST": tsURL.Host},
		providerID)
	if err := reg.Register(providerSpec); err != nil {
		t.Fatalf("register provider: %v", err)
	}

	consumerSpec := &task.Spec{
		ID:      "pinned-deno-consumer",
		Name:    "pinned-deno-consumer",
		Runtime: fakeConsumerRuntime,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 10 * time.Second,
		Permissions: task.Permissions{
			Env: []task.EnvEntry{{Name: "PG_URL", From: "task:" + providerID}},
		},
	}
	if err := reg.Register(consumerSpec); err != nil {
		t.Fatalf("register consumer: %v", err)
	}

	t.Setenv("UPSTREAM_URL", ts.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	runID, result, err := eng.fireSync(ctx, consumerSpec, pkgruntime.RunOptions{}, "manual")
	if err != nil {
		t.Fatalf("fireSync: %v", err)
	}
	if result == nil || result.Error != nil {
		t.Fatalf("consumer run errored: %+v", result)
	}
	if runID == "" {
		t.Fatal("empty run ID")
	}

	captured := captureExec.lastOpts.Load()
	if captured == nil {
		t.Fatal("consumer executor was never dispatched")
	}
	if captured.PreResolvedEnv == nil {
		t.Fatal("PreResolvedEnv is nil — the pinned executor's provider run never routed its secret output back (issue #719)")
	}
	if got := captured.PreResolvedEnv.Env["PG_URL"]; got != pgURLValue {
		t.Errorf("PG_URL = %q, want %q", got, pgURLValue)
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Errorf("upstream should have been hit exactly once, got %d", got)
	}
}

// pythonSecretProviderScript is a minimal Python provider task: it reads the
// batched "requests" param (JSON [{name, optional}, ...]), fetches a
// Doppler-shaped JSON body from UPSTREAM_URL, and calls
// output(map, secret=True) with the requested keys — the Python-SDK mirror
// of the Deno fixture at testdata/secret-provider/test-secret-provider.
const pythonSecretProviderScript = `
import json
import os
import urllib.request

async def main():
    reqs = json.loads(params.get("requests") or "[]")
    url = os.environ.get("UPSTREAM_URL")
    if not url:
        raise RuntimeError("UPSTREAM_URL not set")
    req = urllib.request.Request(url, headers={"Authorization": "Bearer test"})
    with urllib.request.urlopen(req) as resp:
        body = json.loads(resp.read().decode())
    out = {}
    for r in reqs:
        name = r["name"]
        v = body.get("secrets", {}).get(name, {}).get("computed")
        if v is not None:
            out[name] = v
        elif not r.get("optional"):
            raise RuntimeError("required secret " + name + " missing")
    output(out, secret=True)
`

// TestE2E_SecretProvider_PythonRuntime is the regression test for the second
// broken path documented in issue #719: a Python-runtime provider task.
// Unlike Deno, the Python runtime has no "manager doubles as the registered
// executor" fallback — daemon.go's buildRuntimes always registers Python via
// pythonMgr.NewExecutor(p) — so before the fix EVERY Python-runtime provider
// run read a permanently-nil secret-output channel and from: task: always
// failed with "provider ... completed without secret output". After the fix
// the channel flows per-run through RunOptions.SecretOutputCh, which the
// Python executor's Execute wires into its IPC server like the Deno runtime
// does.
//
// Skips cleanly when uv can't be provisioned (offline sandbox), mirroring
// the pattern pkg/runtime/python's own subprocess tests use.
func TestE2E_SecretProvider_PythonRuntime(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	uvBin, err := uvpkg.EnsureUv("")
	if err != nil {
		t.Skipf("uv provisioning failed (offline?): %v", err)
	}

	const pgURLValue = "postgres://python-provider.example.com/db"
	ts, upstreamCalls := newSecretUpstream(t, map[string]string{"PG_URL": pgURLValue})

	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	reg := registry.New(d)
	log := zaptest.NewLogger(t)

	pythonMgr, err := pythonruntime.New(reg, secrets.Chain{}, d, log)
	if err != nil {
		t.Fatalf("python.New: %v", err)
	}

	// The consumer's own runtime is irrelevant to this test — see
	// captureConsumerExecutor's doc.
	const fakeConsumerRuntime = task.Runtime("fake-consumer-for-python-provider-test")
	captureExec := &captureConsumerExecutor{}

	eng := New(reg, captureExec, log)
	eng.SetSecrets(secrets.Chain{})
	eng.SetPythonRuntime(pythonMgr)
	pythonMgr.SetProviderRunner(eng)
	pythonMgr.SetEngine(eng)
	eng.RegisterExecutor(fakeConsumerRuntime, captureExec)

	// A real, uv-backed Python executor built via NewExecutor — mirroring
	// exactly how daemon.go always registers the Python runtime (there is no
	// "manager also serves as executor" fallback for Python, unlike Deno's
	// default-manager path exercised by TestE2E_SecretProvider_FullChain).
	eng.RegisterExecutor(task.Runtime("python"), pythonMgr.NewExecutor(uvBin))

	const providerID = "test-secret-provider-python"
	providerDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(providerDir, "task.py"), []byte(pythonSecretProviderScript), 0o600); err != nil {
		t.Fatalf("write provider script: %v", err)
	}
	providerSpec := &task.Spec{
		ID:      providerID,
		Name:    providerID,
		Runtime: task.Runtime("python"),
		TaskDir: providerDir,
		Trigger: task.TriggerConfig{Manual: true},
		Permissions: task.Permissions{
			Env: []task.EnvEntry{{Name: "UPSTREAM_URL"}},
			Net: []string{"*"},
		},
		Timeout: 30 * time.Second,
	}
	if err := reg.Register(providerSpec); err != nil {
		t.Fatalf("register provider: %v", err)
	}

	consumerSpec := &task.Spec{
		ID:      "python-provider-consumer",
		Name:    "python-provider-consumer",
		Runtime: fakeConsumerRuntime,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 10 * time.Second,
		Permissions: task.Permissions{
			Env: []task.EnvEntry{{Name: "PG_URL", From: "task:" + providerID}},
		},
	}
	if err := reg.Register(consumerSpec); err != nil {
		t.Fatalf("register consumer: %v", err)
	}

	t.Setenv("UPSTREAM_URL", ts.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	runID, result, err := eng.fireSync(ctx, consumerSpec, pkgruntime.RunOptions{}, "manual")
	if err != nil {
		t.Fatalf("fireSync: %v", err)
	}
	if result == nil || result.Error != nil {
		t.Fatalf("consumer run errored: %+v", result)
	}
	if runID == "" {
		t.Fatal("empty run ID")
	}

	captured := captureExec.lastOpts.Load()
	if captured == nil {
		t.Fatal("consumer executor was never dispatched")
	}
	if captured.PreResolvedEnv == nil {
		t.Fatal("PreResolvedEnv is nil — the Python provider run never routed its secret output back (issue #719)")
	}
	if got := captured.PreResolvedEnv.Env["PG_URL"]; got != pgURLValue {
		t.Errorf("PG_URL = %q, want %q", got, pgURLValue)
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Errorf("upstream should have been hit exactly once, got %d", got)
	}
}

// TestE2E_SecretProvider_ConcurrentInvocationsDoNotClobber is the regression
// test for the third bug documented in issue #719: before the fix, the
// secret-output channel was shared mutable state on the runtime
// (BridgeDeps.SecretOutputCh), so two Engine.Run provider invocations
// in flight at once could see each other's IPC server wired to the same
// channel — one send could fill the buffer first and silently drop the
// other, and a key-name collision could resolve the wrong task's output.
// providerRunMu papered over this by serializing Engine.Run outright.
//
// After the fix the channel is a fresh value created per Engine.Run call and
// threaded through that one run's RunOptions, so two concurrent invocations
// — for the SAME provider task, fired with different requested keys, the
// case most likely to alias under the old shared-channel design — never
// share anything. This test fires both concurrently with providerRunMu
// gone (removed by the fix) and asserts each caller gets back exactly its
// own requested value, never the other's and never a timeout.
//
// Uses a real Deno subprocess (the production per-run IPC server path, not
// a mock) so the assertion exercises the actual channel plumbing rather than
// a hand-rolled substitute.
func TestE2E_SecretProvider_ConcurrentInvocationsDoNotClobber(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}

	ts, upstreamCalls := newSecretUpstream(t, map[string]string{
		"KEY_A": "value-A",
		"KEY_B": "value-B",
	})
	tsURL, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse ts url: %v", err)
	}

	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	reg := registry.New(d)
	log := zaptest.NewLogger(t)

	denoRT, err := denoruntime.New(reg, secrets.Chain{}, d, log)
	if err != nil {
		t.Skipf("deno not available: %v", err)
	}

	eng := New(reg, denoRT, log)
	eng.SetSecrets(secrets.Chain{})
	eng.SetDenoRuntime(denoRT)
	denoRT.SetProviderRunner(eng)
	denoRT.SetEngine(eng)

	const providerID = "test-secret-provider-concurrent"
	providerSpec := loadFixtureTpl(t,
		"secret-provider/test-secret-provider",
		map[string]string{"MOCK_HOST": tsURL.Host},
		providerID)
	if err := reg.Register(providerSpec); err != nil {
		t.Fatalf("register provider: %v", err)
	}

	t.Setenv("UPSTREAM_URL", ts.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Fire both invocations of the SAME provider task concurrently, each
	// requesting a different key, and race them against each other with a
	// WaitGroup so neither call is accidentally serialized by test code —
	// only Engine.Run's own (now channel-per-call) implementation may do
	// that, or nothing does.
	var wg sync.WaitGroup
	var resA, resB *envresolve.ProviderResult
	var errA, errB error
	wg.Add(2)
	go func() {
		defer wg.Done()
		resA, errA = eng.Run(ctx, providerID, []envresolve.ProviderRequest{{Name: "KEY_A"}})
	}()
	go func() {
		defer wg.Done()
		resB, errB = eng.Run(ctx, providerID, []envresolve.ProviderRequest{{Name: "KEY_B"}})
	}()
	wg.Wait()

	if errA != nil {
		t.Fatalf("Run(KEY_A): %v", errA)
	}
	if errB != nil {
		t.Fatalf("Run(KEY_B): %v", errB)
	}
	if resA == nil || resB == nil {
		t.Fatalf("nil result: resA=%+v resB=%+v", resA, resB)
	}

	if got := resA.Values["KEY_A"]; got != "value-A" {
		t.Errorf("resA[KEY_A] = %q, want %q (concurrent invocation clobbered it)", got, "value-A")
	}
	if v, present := resA.Values["KEY_B"]; present {
		t.Errorf("resA unexpectedly contains KEY_B=%q — cross-invocation leak", v)
	}
	if got := resB.Values["KEY_B"]; got != "value-B" {
		t.Errorf("resB[KEY_B] = %q, want %q (concurrent invocation clobbered it)", got, "value-B")
	}
	if v, present := resB.Values["KEY_A"]; present {
		t.Errorf("resB unexpectedly contains KEY_A=%q — cross-invocation leak", v)
	}

	// Both invocations dispatched — the provider ran twice (once per
	// Engine.Run call, no cross-call caching at this layer).
	if got := upstreamCalls.Load(); got != 2 {
		t.Errorf("upstream should have been hit exactly twice (once per concurrent invocation), got %d", got)
	}
}
