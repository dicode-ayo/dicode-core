package trigger

// End-to-end test for PR3's 2-stage preflight pipeline shape — the
// production wiring used by buildin/relay-server post-refactor:
//
//   stage 1: render task (buildin/template-equivalent) substitutes
//            ${VAR} placeholders from process env and returns the
//            rendered string. run_result.enabled: false so the value
//            flows in-memory but never lands in runs.return_value.
//
//   stage 2: write task (buildin/write-local-equivalent) receives the
//            rendered string via ${input.output} interpolation on its
//            `content` param, writes it to disk at the configured path
//            and mode, and returns the resolved path.
//
//   daemon: a stubbed docker task whose only job is to verify the
//           pipeline ran and the rendered file is on disk before the
//           daemon's "process" was started. The daemon spec's
//           trigger.before pins both stages in order.
//
// Why not use the real buildin/template + buildin/write-local? Because
// buildin/write-local lives in PR1 of this 3-PR epic and may not yet
// have landed when this test runs in CI. Re-implementing the two
// stages inline (Deno scripts written to the per-test tasks dir) keeps
// the test self-contained — it exercises the PR3 trigger-engine
// machinery (sequential before:, ${input.output} piping,
// dispatchPipelineStage helper, daemon gating on preflight success)
// without depending on the on-disk buildin/* tree's snapshot at any
// particular epic-PR boundary.
//
// Manual mid-pipeline re-fire propagation is already covered by
// TestBefore_MidPipelineReFirePropagates (controllable executor, no
// real Deno needed). This e2e test focuses on the initial-boot path:
// "the pipeline composes and the rendered file is on disk at the
// expected path / mode / content before the daemon fires."

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
)

// relayRenderScript mirrors buildin/template's behaviour: substitute
// ${VAR} placeholders from process env, throw on unresolved, return
// the rendered string. Inlined here (not imported) so the e2e test
// doesn't depend on the production buildin/template task body
// continuing to expose any particular shape — that task has its own
// task.test.ts coverage.
const relayRenderScript = `
const PLACEHOLDER_RE = /\$\{([A-Za-z_][A-Za-z0-9_]*)\}/g;
export default async function main({ params }) {
  const tpl = await params.get("template");
  if (tpl === null) throw new Error("missing template param");
  return tpl.replace(PLACEHOLDER_RE, (_m, name) => {
    const v = Deno.env.get(name);
    if (v === undefined) throw new Error("unresolved placeholder: ${" + name + "}");
    return v;
  });
}
`

// relayWriteScript mirrors buildin/write-local (PR1): write content
// to the given path at the given mode, return the resolved path.
// Inlined for the same self-containment reason as relayRenderScript.
const relayWriteScript = `
export default async function main({ params }) {
  const content = await params.get("content");
  const path = await params.get("path");
  const modeStr = (await params.get("mode")) ?? "0600";
  if (content === null) throw new Error("missing content param");
  if (path === null) throw new Error("missing path param");
  const mode = parseInt(modeStr, 8);
  if (!Number.isFinite(mode)) throw new Error("mode is not a valid octal string: " + modeStr);
  await Deno.writeTextFile(path, content);
  await Deno.chmod(path, mode);
  return path;
}
`

// relayDaemonScript stands in for buildin/relay-server's startServer
// call. It asserts the pre-rendered relay.yaml exists at the path the
// supervisor would have looked at, then parks until the run is
// cancelled (which is how the real daemon body waits on http.Server's
// `close` event). The captured spec lets the test assert on the
// daemon's docker.volumes after dispatch.
//
// We use a Deno stub rather than a docker stub so the daemon's body
// can directly probe the rendered config file the pipeline produced
// — that's the contract the relay-server refactor depends on.
const relayDaemonScript = `
export default async function main() {
  const dataDir = Deno.env.get("DICODE_DATADIR");
  if (!dataDir) throw new Error("DICODE_DATADIR not set");
  const configPath = dataDir + "/relay/relay.yaml";
  const body = await Deno.readTextFile(configPath); // throws if missing
  if (!body.includes("base_url:")) {
    throw new Error("rendered relay.yaml missing base_url key; got:\n" + body);
  }
  // Park forever — the engine cancels via KillRun on shutdown.
  await new Promise(() => {});
}
`

// writeInlineTask writes a fully-formed task.yaml + task.ts pair to
// dir/id, returning the directory path so the test can LoadDir it.
// Differs from writeTask (engine_test.go) only in that the YAML body
// is supplied verbatim — the e2e fixtures need to express
// permissions.env / permissions.fs / run_result blocks that the
// strongly-typed builder doesn't reach.
func writeInlineTask(t *testing.T, dir, id, yaml, script string) string {
	t.Helper()
	td := filepath.Join(dir, id)
	if err := os.MkdirAll(td, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "task.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "task.ts"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	return td
}

// captureRelayDaemonExec wraps the real Deno runtime: it dispatches
// Execute through the runtime (so the daemon body actually probes the
// rendered file) AND captures the spec the executor was handed so the
// test can assert on the post-pipeline state. The Deno runtime is
// registered separately for non-daemon (render/write) stages.
//
// We can't reuse stubDockerExec from e2e_template_preflight_pipeline_test.go
// because PR3's relay-server is runtime:deno, not docker — the
// daemon body itself wants the Deno runtime so it can fs.read the
// rendered config.
type captureRelayDaemonExec struct {
	inner pkgruntime.Executor

	mu       sync.Mutex
	captured map[string]*task.Spec
}

func newCaptureRelayDaemonExec(inner pkgruntime.Executor) *captureRelayDaemonExec {
	return &captureRelayDaemonExec{
		inner:    inner,
		captured: make(map[string]*task.Spec),
	}
}

func (c *captureRelayDaemonExec) Execute(ctx context.Context, spec *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	c.mu.Lock()
	cp := *spec
	c.captured[opts.RunID] = &cp
	c.mu.Unlock()
	return c.inner.Execute(ctx, spec, opts)
}

func (c *captureRelayDaemonExec) specFor(runID string) *task.Spec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.captured[runID]
}

// TestE2E_RelayTemplatePipeline composes PR3's preflight pipeline
// (sequential before: + ${input.output} piping + descendant pipeline
// stage helper) against a real Deno runtime, mirroring the
// buildin/relay-server post-refactor shape:
//
//	render (template) → write (write-local) → daemon
//
// Assertions:
//   - Both preflight stages run; daemon reaches Running.
//   - ${DATADIR}/relay/relay.yaml exists with mode 0600 and contains
//     the rendered substitutions (BASE_URL, STATUS_PASSWORD).
//   - The renderer's return value is empty in runs.return_value
//     (run_result.enabled=false) but DID flow through the pipeline.
//   - The render task's registry spec is NOT mutated by the per-edge
//     override on the daemon edge (carries PR2's pointer-shared
//     mutation hazard fix into the real-deno path).
func TestE2E_RelayTemplatePipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}

	tasksDir := t.TempDir()
	dataDir := t.TempDir()
	relayDir := filepath.Join(dataDir, "relay")
	if err := os.MkdirAll(relayDir, 0o755); err != nil {
		t.Fatalf("mkdir relayDir: %v", err)
	}
	relayConfigPath := filepath.Join(relayDir, "relay.yaml")

	// Doppler-equivalent secrets, sourced from process env for the
	// renderer. Real production would source these via task:doppler
	// in the renderer's per-edge env override; here we project them
	// directly through t.Setenv so the test stays hermetic.
	const wantBaseURL = "https://relay.test.example.com"
	const wantStatusPW = "test-status-pw"
	t.Setenv("BASE_URL", wantBaseURL)
	t.Setenv("STATUS_PASSWORD", wantStatusPW)

	e := newTestEnv(t)

	// Wrap the Deno runtime in the spec-capturing decorator for the
	// daemon path only. The render + write stages flow through the
	// underlying Deno runtime directly.
	captureExec := newCaptureRelayDaemonExec(e.denoRT)
	// Override the deno executor used by the engine for daemon runs.
	// (The engine resolves by runtime; deno is the runtime for all
	// three stages here. We swap the engine's deno executor wholesale
	// so dispatches go through the capture layer — captureExec.inner
	// is the real Deno runtime, so render/write still work.)
	e.engine.RegisterExecutor(task.RuntimeDeno, captureExec)

	// ── Stage 1: renderer.
	const renderYAML = `apiVersion: dicode/v1
kind: Task
name: render
runtime: deno
trigger:
  manual: true
permissions:
  env:
    - BASE_URL
    - STATUS_PASSWORD
params:
  template:
    type: string
    required: true
run_result:
  enabled: false
timeout: 15s
`
	renderDir := writeInlineTask(t, tasksDir, "render", renderYAML, relayRenderScript)
	renderSpec, err := task.LoadDir(renderDir)
	if err != nil {
		t.Fatalf("LoadDir render: %v", err)
	}
	if err := e.reg.Register(renderSpec); err != nil {
		t.Fatalf("reg.Register render: %v", err)
	}
	if err := e.engine.Register(renderSpec); err != nil {
		t.Fatalf("eng.Register render: %v", err)
	}

	// ── Stage 2: writer.
	writeYAML := `apiVersion: dicode/v1
kind: Task
name: write
runtime: deno
trigger:
  manual: true
permissions:
  fs:
    - path: ` + relayDir + `
      permission: rw
params:
  content:
    type: string
    required: true
  path:
    type: string
    required: true
  mode:
    type: string
    default: "0600"
timeout: 15s
`
	writeDir := writeInlineTask(t, tasksDir, "write", writeYAML, relayWriteScript)
	writeSpec, err := task.LoadDir(writeDir)
	if err != nil {
		t.Fatalf("LoadDir write: %v", err)
	}
	if err := e.reg.Register(writeSpec); err != nil {
		t.Fatalf("reg.Register write: %v", err)
	}
	if err := e.engine.Register(writeSpec); err != nil {
		t.Fatalf("eng.Register write: %v", err)
	}

	// ── Daemon: declares the 2-stage before: + reads the rendered
	// relay.yaml at startup.
	daemonYAML := `apiVersion: dicode/v1
kind: Task
name: relay-daemon
runtime: deno
trigger:
  daemon: true
  restart: never
  before:
    - task: render
      overrides:
        params:
          template: "server:\n  base_url: ${BASE_URL}\n  port: 5553\nstatus:\n  password: ${STATUS_PASSWORD}\n"
        env:
          - BASE_URL
          - STATUS_PASSWORD
    - task: write
      overrides:
        params:
          content: "${input.output}"
          path: "` + relayConfigPath + `"
          mode: "0600"
        fs:
          - path: ` + relayDir + `
            permission: rw
permissions:
  env:
    - DICODE_DATADIR
  fs:
    - path: ` + relayDir + `
      permission: rw
timeout: 30s
`
	daemonDir := writeInlineTask(t, tasksDir, "relay-daemon", daemonYAML, relayDaemonScript)
	daemonSpec, err := task.LoadDir(daemonDir)
	if err != nil {
		t.Fatalf("LoadDir daemon: %v", err)
	}
	// The daemon task body reads DICODE_DATADIR; project it so the
	// runtime allows the env entry through.
	t.Setenv("DICODE_DATADIR", dataDir)

	if err := e.reg.Register(daemonSpec); err != nil {
		t.Fatalf("reg.Register daemon: %v", err)
	}
	if err := e.engine.Register(daemonSpec); err != nil {
		t.Fatalf("eng.Register daemon: %v", err)
	}

	// ── Wait for daemon to clear preflight + reach Running. This is
	// the all-in-one assertion: if either stage's dispatch broke
	// (overrides not applied, ${input.output} not piped, runtime not
	// wired), the daemon would never get past PrereqRunning.
	waitUntil(t, 60*time.Second, func() bool {
		return e.engine.DaemonState(daemonSpec.ID) == DaemonRunning
	}, "daemon never reached Running")

	// ── Rendered file is on disk, mode 0600, content matches.
	body, err := os.ReadFile(relayConfigPath)
	if err != nil {
		t.Fatalf("read relay.yaml: %v", err)
	}
	wantBody := "server:\n  base_url: " + wantBaseURL + "\n  port: 5553\nstatus:\n  password: " + wantStatusPW + "\n"
	if string(body) != wantBody {
		t.Errorf("rendered relay.yaml:\ngot:\n%s\nwant:\n%s", string(body), wantBody)
	}
	info, err := os.Stat(relayConfigPath)
	if err != nil {
		t.Fatalf("stat relay.yaml: %v", err)
	}
	if got := info.Mode() & fs.ModePerm; got != 0o600 {
		t.Errorf("relay.yaml mode = %o, want 0600 (write stage failed to apply mode)", got)
	}

	// ── Renderer's persisted return value must be empty
	// (run_result.enabled=false) but the pipeline still composed,
	// which proves the in-memory delivery path works for the new
	// trigger.before semantics.
	renderRuns, _ := e.reg.ListRuns(context.Background(), "render", 10)
	var renderRunID string
	for _, r := range renderRuns {
		if r.TriggerSource == registry.TriggerPreflight {
			renderRunID = r.ID
			break
		}
	}
	if renderRunID == "" {
		t.Fatal("render preflight run never recorded in registry")
	}
	renderRun, err := e.reg.GetRun(context.Background(), renderRunID)
	if err != nil {
		t.Fatalf("GetRun render: %v", err)
	}
	if renderRun.Status != registry.StatusSuccess {
		t.Errorf("render preflight status = %q, want success", renderRun.Status)
	}
	if renderRun.ReturnValue != "" {
		t.Errorf("render ReturnValue persisted = %q, want empty (run_result.enabled=false)", renderRun.ReturnValue)
	}

	// ── Writer run must have observed the substituted content via
	// ${input.output}. The writer returned its target path; the
	// existence of the file at the expected path is already asserted
	// above. Additionally verify the writer's persisted record is
	// success so a write-failure-but-pipeline-passes regression can't
	// slip past.
	writeRuns, _ := e.reg.ListRuns(context.Background(), "write", 10)
	var sawWriteSuccess bool
	for _, r := range writeRuns {
		if r.TriggerSource == registry.TriggerPreflight && r.Status == registry.StatusSuccess {
			sawWriteSuccess = true
			break
		}
	}
	if !sawWriteSuccess {
		t.Error("no write preflight run with status=success recorded; pipeline did not complete cleanly")
	}

	// ── PR2 hazard regression: the per-edge override on the daemon
	// edge must NOT have mutated the renderer's canonical registry
	// spec. A standalone manual fire of render should still observe
	// the original (empty) template default — the override-projected
	// template body must not leak.
	regRender, ok := e.reg.Get("render")
	if !ok {
		t.Fatal("render task disappeared from registry")
	}
	for _, p := range regRender.Params {
		if p.Name == "template" && p.Default != "" {
			t.Errorf("registry render spec leaked template default %q from per-edge override (PR2 mutation hazard regressed)", p.Default)
		}
	}

	// Sanity check on the captured daemon spec — should have reached
	// the executor (the wait-for-Running above confirms it, but the
	// presence of a captured spec rules out the daemon executor being
	// silently bypassed).
	deadline := time.Now().Add(5 * time.Second)
	var captured *task.Spec
	for time.Now().Before(deadline) {
		runs, _ := e.reg.ListRuns(context.Background(), daemonSpec.ID, 5)
		for _, r := range runs {
			if r.TriggerSource == registry.TriggerDaemon {
				if s := captureExec.specFor(r.ID); s != nil {
					captured = s
					break
				}
			}
		}
		if captured != nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if captured == nil {
		t.Fatal("daemon spec never reached the deno capture executor")
	}

	// Avoid leaking the daemon goroutine into the rest of the test
	// run — newTestEnv's t.Cleanup will close the DB, but the
	// engine's daemon goroutines are killed via KillRun in the
	// engine's Stop path which the test harness doesn't wire. Best
	// effort cleanup: cancel the run if we still know the ID. (The
	// daemon body's `await new Promise(() => {})` parks until the
	// context cancels.)
	if runID := func() string {
		e.engine.daemonMu.Lock()
		defer e.engine.daemonMu.Unlock()
		return e.engine.daemonRuns[daemonSpec.ID]
	}(); runID != "" {
		e.engine.KillRun(runID)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if _, werr := e.engine.WaitRun(ctx, runID); werr != nil && !errors.Is(werr, context.DeadlineExceeded) {
			t.Logf("WaitRun on shutdown daemon returned: %v (best effort)", werr)
		}
		cancel()
	}
}
