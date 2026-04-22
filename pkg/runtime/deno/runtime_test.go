package deno

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// testEnv wires up a full Deno runtime for tests.
// Tests are skipped if Deno is not available on the system / download fails.
type testEnv struct {
	rt  *Runtime
	reg *registry.Registry
	db  db.DB
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	rt, err := New(reg, secrets.Chain{}, d, zap.NewNop())
	if err != nil {
		t.Skipf("deno not available: %v", err)
	}
	return &testEnv{rt: rt, reg: reg, db: d}
}

// run executes a task body wrapped in export default async function main().
// body is the function body; use runRaw to provide a complete task.ts.
func (e *testEnv) run(t *testing.T, body string, opts ...RunOptions) *RunResult {
	t.Helper()
	return e.runSpec(t, "export default async function main({ params, kv, input, output }) {\n"+body+"\n}", &task.Spec{
		ID:      "test-task",
		Name:    "test-task",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 30 * time.Second,
	}, opts...)
}

func (e *testEnv) runSpec(t *testing.T, script string, spec *task.Spec, opts ...RunOptions) *RunResult {
	t.Helper()
	dir := t.TempDir()
	spec.TaskDir = dir
	_ = os.WriteFile(filepath.Join(dir, "task.ts"), []byte(script), 0644)
	_ = os.WriteFile(filepath.Join(dir, "task.yaml"),
		[]byte("name: test-task\nruntime: deno\ntrigger:\n  manual: true\n"), 0644)
	_ = e.reg.Register(spec)
	o := RunOptions{}
	if len(opts) > 0 {
		o = opts[0]
	}
	result, err := e.rt.Run(context.Background(), spec, o)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	return result
}

// --- tests ---

func TestRuntime_ReturnValue(t *testing.T) {
	e := newTestEnv(t)
	r := e.run(t, `return 42`)
	if r.Error != nil {
		t.Fatalf("unexpected error: %v", r.Error)
	}
	// JSON numbers deserialise to float64.
	if r.ReturnValue != float64(42) {
		t.Errorf("expected 42, got %v (%T)", r.ReturnValue, r.ReturnValue)
	}
}

func TestRuntime_ReturnString(t *testing.T) {
	e := newTestEnv(t)
	r := e.run(t, `return "hello"`)
	if r.ReturnValue != "hello" {
		t.Errorf("got %v", r.ReturnValue)
	}
}

func TestRuntime_AsyncAwait(t *testing.T) {
	e := newTestEnv(t)
	r := e.run(t, `
		const p = new Promise(resolve => setTimeout(resolve, 10, "async-result"))
		const v = await p
		return v
	`)
	if r.Error != nil {
		t.Fatalf("error: %v", r.Error)
	}
	if r.ReturnValue != "async-result" {
		t.Errorf("got %v", r.ReturnValue)
	}
}

func TestRuntime_Log(t *testing.T) {
	e := newTestEnv(t)
	r := e.run(t, `
		console.log("hello")
		console.warn("world")
		return "done"
	`)
	if r.Error != nil {
		t.Fatalf("error: %v", r.Error)
	}
	if len(r.Logs) < 2 {
		t.Fatalf("expected ≥2 logs, got %d", len(r.Logs))
	}
	found := map[string]bool{}
	for _, l := range r.Logs {
		if l.Message == "hello" || l.Message == "world" {
			found[l.Message] = true
		}
	}
	if !found["hello"] || !found["world"] {
		t.Errorf("unexpected logs: %+v", r.Logs)
	}
}

// ── permissions tests ─────────────────────────────────────────────────────────

// TestRuntime_Env_BarePassthrough: bare name allowlists the var so the script
// can read it from the host env — no injection, no secrets.
func TestRuntime_Env_BarePassthrough(t *testing.T) {
	t.Setenv("DICODE_TEST_TOKEN", "bare-value")
	e := newTestEnv(t)
	spec := &task.Spec{
		ID: "env-bare", Name: "env-bare", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		Permissions: task.Permissions{
			Env: []task.EnvEntry{{Name: "DICODE_TEST_TOKEN"}},
		},
	}
	r := e.runSpec(t, `export default async function main() { return Deno.env.get("DICODE_TEST_TOKEN") ?? null }`, spec)
	if r.Error != nil {
		t.Fatalf("error: %v", r.Error)
	}
	if r.ReturnValue != "bare-value" {
		t.Errorf("expected bare-value, got %v", r.ReturnValue)
	}
}

// TestRuntime_Env_From: from: reads a host env var and injects it under a different name.
func TestRuntime_Env_From(t *testing.T) {
	t.Setenv("DICODE_TEST_SOURCE", "from-value")
	e := newTestEnv(t)
	spec := &task.Spec{
		ID: "env-from", Name: "env-from", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		Permissions: task.Permissions{
			Env: []task.EnvEntry{{Name: "INJECTED", From: "DICODE_TEST_SOURCE"}},
		},
	}
	r := e.runSpec(t, `export default async function main() { return Deno.env.get("INJECTED") ?? null }`, spec)
	if r.Error != nil {
		t.Fatalf("error: %v", r.Error)
	}
	if r.ReturnValue != "from-value" {
		t.Errorf("expected from-value, got %v", r.ReturnValue)
	}
}

// TestRuntime_Env_Secret: secret: resolves from the secrets chain and injects.
func TestRuntime_Env_Secret(t *testing.T) {
	e := newTestEnv(t)
	e.rt.secrets = secrets.Chain{mockSecretProvider{"my_api_key": "secret-value"}}
	spec := &task.Spec{
		ID: "env-secret", Name: "env-secret", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		Permissions: task.Permissions{
			Env: []task.EnvEntry{{Name: "API_KEY", Secret: "my_api_key"}},
		},
	}
	r := e.runSpec(t, `export default async function main() { return Deno.env.get("API_KEY") ?? null }`, spec)
	if r.Error != nil {
		t.Fatalf("error: %v", r.Error)
	}
	if r.ReturnValue != "secret-value" {
		t.Errorf("expected secret-value, got %v", r.ReturnValue)
	}
}

// TestRuntime_Env_SecretMissing: secret: with a missing key fails the run immediately.
func TestRuntime_Env_SecretMissing(t *testing.T) {
	e := newTestEnv(t)
	e.rt.secrets = secrets.Chain{mockSecretProvider{}} // empty — key not present
	spec := &task.Spec{
		ID: "env-missing", Name: "env-missing", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		Permissions: task.Permissions{
			Env: []task.EnvEntry{{Name: "API_KEY", Secret: "nonexistent"}},
		},
	}
	r := e.runSpec(t, `export default async function main() { return "should not reach" }`, spec)
	if r.Error == nil {
		t.Fatal("expected error for missing secret, got nil")
	}
}

// TestRuntime_Env_NotDeclared: a var not in permissions.env must not be readable by the script.
func TestRuntime_Env_NotDeclared(t *testing.T) {
	t.Setenv("DICODE_TEST_SECRET", "should-be-hidden")
	e := newTestEnv(t)
	spec := &task.Spec{
		ID: "env-reject", Name: "env-reject", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		// Permissions.Env is empty — DICODE_TEST_SECRET is NOT declared.
	}
	// Deno throws NotCapable when a script tries to read an undeclared env var.
	r := e.runSpec(t, `export default async function main({ env }) {
		try {
			const v = Deno.env.get("DICODE_TEST_SECRET")
			return v ?? "null"
		} catch (e) {
			return "blocked"
		}
	}`, spec)
	if r.Error != nil {
		t.Fatalf("unexpected run error: %v", r.Error)
	}
	if r.ReturnValue != "blocked" {
		t.Errorf("expected env var to be blocked, got %v", r.ReturnValue)
	}
}

// TestRuntime_Net_Restricted: net: with specific hosts blocks requests to unlisted hosts.
func TestRuntime_Net_Restricted(t *testing.T) {
	e := newTestEnv(t)
	spec := &task.Spec{
		ID: "net-restrict", Name: "net-restrict", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		Permissions: task.Permissions{
			Net: []string{"localhost"}, // only localhost allowed
		},
	}
	// Trying to fetch 1.2.3.4 (not in the allowlist) should throw NotCapable.
	r := e.runSpec(t, `export default async function main() {
		try {
			await fetch("http://1.2.3.4/")
			return "allowed"
		} catch (e) {
			return "blocked"
		}
	}`, spec)
	if r.Error != nil {
		t.Fatalf("unexpected run error: %v", r.Error)
	}
	if r.ReturnValue != "blocked" {
		t.Errorf("expected fetch to non-allowlisted host to be blocked, got %v", r.ReturnValue)
	}
}

// TestRuntime_Net_Denied: net: [] (empty) blocks all network access.
func TestRuntime_Net_Denied(t *testing.T) {
	e := newTestEnv(t)
	spec := &task.Spec{
		ID: "net-deny", Name: "net-deny", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		Permissions: task.Permissions{
			Net: []string{}, // explicit empty = deny all
		},
	}
	r := e.runSpec(t, `export default async function main() {
		try {
			await fetch("http://example.com/")
			return "allowed"
		} catch (e) {
			return "blocked"
		}
	}`, spec)
	if r.Error != nil {
		t.Fatalf("unexpected run error: %v", r.Error)
	}
	if r.ReturnValue != "blocked" {
		t.Errorf("expected all network to be blocked, got %v", r.ReturnValue)
	}
}

func TestRuntime_Params(t *testing.T) {
	e := newTestEnv(t)
	spec := &task.Spec{
		ID:      "param-task",
		Name:    "param-task",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 30 * time.Second,
		Params:  []task.Param{{Name: "channel", Default: "#general"}},
	}
	r := e.runSpec(t, `export default async function main({ params }) { return await params.get("channel") }`, spec,
		RunOptions{Params: map[string]string{"channel": "#devops"}})
	if r.ReturnValue != "#devops" {
		t.Errorf("expected #devops, got %v", r.ReturnValue)
	}
}

func TestRuntime_HTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
	}))
	defer srv.Close()

	e := newTestEnv(t)
	r := e.run(t, `
		const res = await fetch("`+srv.URL+`")
		const body = await res.json()
		return body.ok
	`)
	if r.Error != nil {
		t.Fatalf("error: %v", r.Error)
	}
	if r.ReturnValue != true {
		t.Errorf("expected true, got %v", r.ReturnValue)
	}
}

func TestRuntime_HTTP_Unrestricted(t *testing.T) {
	// net: omitted → --allow-net (unrestricted); fetch should succeed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
	}))
	defer srv.Close()

	e := newTestEnv(t)
	spec := &task.Spec{
		ID: "net-open", Name: "net-open", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		// Permissions.Net is nil → unrestricted
	}
	r := e.runSpec(t, `export default async function main() {
		const res = await fetch("`+srv.URL+`")
		const body = await res.json()
		return body.ok
	}`, spec)
	if r.Error != nil {
		t.Fatalf("error: %v", r.Error)
	}
	if r.ReturnValue != true {
		t.Errorf("expected true, got %v", r.ReturnValue)
	}
}

func TestRuntime_KV(t *testing.T) {
	e := newTestEnv(t)
	r := e.run(t, `
		await kv.set("mykey", { count: 42 })
		const val = await kv.get("mykey")
		return val.count
	`, RunOptions{})
	if r.Error != nil {
		t.Fatalf("error: %v", r.Error)
	}
	if r.ReturnValue != float64(42) {
		t.Errorf("expected 42, got %v (%T)", r.ReturnValue, r.ReturnValue)
	}
}

func TestRuntime_Output_HTML(t *testing.T) {
	e := newTestEnv(t)
	r := e.run(t, `return output.html("<h1>Hello</h1>")`)
	if r.Error != nil {
		t.Fatalf("error: %v", r.Error)
	}
	if r.Output == nil {
		t.Fatal("expected output to be set")
	}
	if r.Output.ContentType != "text/html" {
		t.Errorf("expected text/html, got %s", r.Output.ContentType)
	}
	if r.Output.Content != "<h1>Hello</h1>" {
		t.Errorf("unexpected content: %s", r.Output.Content)
	}
}

func TestRuntime_Output_Text(t *testing.T) {
	e := newTestEnv(t)
	r := e.run(t, `return output.text("plain text result")`)
	if r.Output == nil || r.Output.ContentType != "text/plain" {
		t.Fatalf("expected text/plain output, got %+v", r.Output)
	}
}

func TestRuntime_RunRecord(t *testing.T) {
	e := newTestEnv(t)
	r := e.run(t, `return "ok"`)
	if r.RunID == "" {
		t.Fatal("no run ID")
	}
	run, err := e.reg.GetRun(context.Background(), r.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != registry.StatusSuccess {
		t.Errorf("expected success, got %s", run.Status)
	}
}

func TestRuntime_ScriptError(t *testing.T) {
	e := newTestEnv(t)
	r := e.run(t, `throw new Error("boom")`, RunOptions{})
	if r.Error == nil {
		t.Fatal("expected error")
	}
	run, err := e.reg.GetRun(context.Background(), r.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != registry.StatusFailure {
		t.Errorf("expected failure, got %s", run.Status)
	}
}

func TestRuntime_Timeout(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()
	reg := registry.New(d)
	rt, err := New(reg, secrets.Chain{}, d, zap.NewNop())
	if err != nil {
		t.Skipf("deno not available: %v", err)
	}

	spec := &task.Spec{
		ID:      "timeout-task",
		Name:    "timeout-task",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 500 * time.Millisecond,
	}
	dir := t.TempDir()
	spec.TaskDir = dir
	_ = os.WriteFile(filepath.Join(dir, "task.ts"),
		[]byte(`await new Promise(r => setTimeout(r, 30000))`), 0644)
	_ = os.WriteFile(filepath.Join(dir, "task.yaml"),
		[]byte("name: timeout-task\nruntime: deno\ntrigger:\n  manual: true\n"), 0644)
	_ = reg.Register(spec)

	r, _ := rt.Run(context.Background(), spec, RunOptions{})
	if r.Error == nil {
		t.Fatal("expected timeout error")
	}
}

// TestBuildEnv_InjectsBrokerURL covers issue #84: when the daemon has a
// resolved broker URL, buildEnv must export it to the Deno subprocess as
// DICODE_BROKER_URL. When empty (relay disabled or ServerURL unparsable),
// the variable must NOT be present — tasks then distinguish "broker
// disabled" from "broker at default" by checking Deno.env.get().
func TestBuildEnv_InjectsBrokerURL(t *testing.T) {
	t.Run("injected when non-empty", func(t *testing.T) {
		env := buildEnv(nil, "/tmp/sock", "tok", "https://broker.dicode.app")
		var found string
		for _, kv := range env {
			if strings.HasPrefix(kv, "DICODE_BROKER_URL=") {
				found = kv
				break
			}
		}
		if found != "DICODE_BROKER_URL=https://broker.dicode.app" {
			t.Errorf("expected DICODE_BROKER_URL injection, got %q among env", found)
		}
	})

	t.Run("omitted when empty", func(t *testing.T) {
		env := buildEnv(nil, "/tmp/sock", "tok", "")
		for _, kv := range env {
			if strings.HasPrefix(kv, "DICODE_BROKER_URL=") {
				t.Errorf("DICODE_BROKER_URL must not be injected when broker URL is empty, got %q", kv)
			}
		}
	})
}

// TestBuildDenoArgs_AllowsBrokerURL ensures DICODE_BROKER_URL is added to
// --allow-env (auto-granted, like DICODE_SOCKET) when configured, so auth
// tasks can Deno.env.get() it without declaring it in permissions.env.
func TestBuildDenoArgs_AllowsBrokerURL(t *testing.T) {
	spec := &task.Spec{
		ID:      "probe",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
	}
	findAllowEnv := func(args []string) string {
		for _, a := range args {
			if strings.HasPrefix(a, "--allow-env=") {
				return a
			}
		}
		return ""
	}

	t.Run("allowlist includes DICODE_BROKER_URL when set", func(t *testing.T) {
		args := buildDenoArgs(spec, "/tmp/sock", "/tmp/shim", "/tmp/runner", "https://b")
		allow := findAllowEnv(args)
		if !strings.Contains(allow, "DICODE_BROKER_URL") {
			t.Errorf("--allow-env must include DICODE_BROKER_URL when broker URL set, got %q", allow)
		}
	})

	t.Run("allowlist omits DICODE_BROKER_URL when empty", func(t *testing.T) {
		args := buildDenoArgs(spec, "/tmp/sock", "/tmp/shim", "/tmp/runner", "")
		allow := findAllowEnv(args)
		if strings.Contains(allow, "DICODE_BROKER_URL") {
			t.Errorf("--allow-env must not include DICODE_BROKER_URL when unset, got %q", allow)
		}
	})
}

// mockSecretProvider for env tests.
type mockSecretProvider map[string]string

func (m mockSecretProvider) Name() string { return "mock" }
func (m mockSecretProvider) Get(_ context.Context, key string) (string, error) {
	return m[key], nil
}
