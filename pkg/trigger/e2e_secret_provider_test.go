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
	"sync/atomic"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	denoruntime "github.com/dicode/dicode/pkg/runtime/deno"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
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
//     must observe the same secretOutputCh / providerRunner the engine
//     wired into the runtime.
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
	// host env so the test can point it at the httptest server.
	const providerID = "test-secret-provider"
	providerDir := t.TempDir()
	providerYAML := `apiVersion: dicode/v1
kind: Task
name: test-secret-provider
runtime: deno
trigger:
  manual: true
provider:
  cache_ttl: 5m
permissions:
  env:
    - UPSTREAM_URL
  net:
    - ` + tsURL.Host + `
params:
  requests:
    type: string
    required: true
timeout: 10s
`
	providerTS := `
interface Req { name: string; optional: boolean }
interface Resp { secrets: Record<string, { computed: string }> }
export default async function main({ params, output }: any) {
  const reqs: Req[] = JSON.parse((await params.get("requests")) ?? "[]");
  const url = Deno.env.get("UPSTREAM_URL");
  if (!url) throw new Error("UPSTREAM_URL not set");
  const resp = await fetch(url, { headers: { "Authorization": "Bearer test" } });
  if (!resp.ok) throw new Error("upstream " + resp.status);
  const body = (await resp.json()) as Resp;
  const out: Record<string, string> = {};
  for (const r of reqs) {
    const v = body.secrets[r.name]?.computed;
    if (typeof v === "string") out[r.name] = v;
    else if (!r.optional) throw new Error("required secret " + r.name + " missing");
  }
  await output(out, { secret: true });
}
`
	if err := os.WriteFile(filepath.Join(providerDir, "task.yaml"), []byte(providerYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(providerDir, "task.ts"), []byte(providerTS), 0o644); err != nil {
		t.Fatal(err)
	}
	providerSpec, err := task.LoadDir(providerDir)
	if err != nil {
		t.Fatalf("load provider: %v", err)
	}
	providerSpec.ID = providerID
	if err := reg.Register(providerSpec); err != nil {
		t.Fatalf("register provider: %v", err)
	}

	// ── Consumer task — pulls PG_URL and REDIS_URL from the provider task,
	// then echoes them via output.text so the run record carries the
	// resolved values back to the test for end-to-end assertion.
	consumerDir := t.TempDir()
	consumerYAML := `apiVersion: dicode/v1
kind: Task
name: test-consumer
runtime: deno
trigger:
  manual: true
permissions:
  env:
    - name: PG_URL
      from: task:test-secret-provider
    - name: REDIS_URL
      from: task:test-secret-provider
      optional: true
timeout: 10s
`
	consumerTS := `
export default async function main({ output }: any) {
  const pg = Deno.env.get("PG_URL") ?? "";
  const r  = Deno.env.get("REDIS_URL") ?? "";
  await output.text("PG_URL=" + pg + " REDIS_URL=" + r);
}
`
	if err := os.WriteFile(filepath.Join(consumerDir, "task.yaml"), []byte(consumerYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consumerDir, "task.ts"), []byte(consumerTS), 0o644); err != nil {
		t.Fatal(err)
	}
	consumerSpec, err := task.LoadDir(consumerDir)
	if err != nil {
		t.Fatalf("load consumer: %v", err)
	}
	consumerSpec.ID = "test-consumer"
	if err := reg.Register(consumerSpec); err != nil {
		t.Fatalf("register consumer: %v", err)
	}

	// UPSTREAM_URL is host-env-allowlisted in the provider's permissions.env;
	// the runtime inherits os.Environ() so t.Setenv flows through to the
	// Deno subprocess.
	t.Setenv("UPSTREAM_URL", ts.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// ── First launch.
	runID, result, err := eng.fireSync(consumerSpec, pkgruntime.RunOptions{}, "manual")
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
