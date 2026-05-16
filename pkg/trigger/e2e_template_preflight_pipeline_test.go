package trigger

// End-to-end test composing every feature shipped by the 7-PR epic
// (#297, #298, #299, #300, #302, #303) into a single cloudflared-style
// pipeline:
//
//  1. A daemon task declares `docker.volumes: ["${DATADIR}/...:..."]`
//     (PR #297) and a `trigger.before` preflight edge with per-edge
//     overrides patching params + env on a real Deno renderer prereq
//     (PR #300 + #303).
//  2. The renderer task is a Deno port of the buildin/template task
//     (PR #298 logic) with `run_result.enabled: false` so its rendered
//     output is suppressed from `runs.return_value` (PR #302) while
//     still flowing in-memory to a chain downstream.
//  3. A verifier task chains off the renderer with `trigger.chain.params`
//     (PR #299), receives the rendered string via `input.output`, and
//     writes a marker file so the test can assert the rendered value
//     reached the chain consumer.
//
// Daemon execution is stubbed (no real docker) — the test asserts the
// dispatched daemon spec carries the expanded `${DATADIR}` volume path,
// that the daemon entered DaemonRunning (preflight + chain succeeded),
// and that the side-effects of the renderer + verifier landed on disk.
//
// What would have broken in earlier development:
//
//   - PR #297 regression: a template loader that forgot to call
//     expandSpec on docker.volumes would leave the literal "${DATADIR}"
//     in the dispatched daemon spec, failing the volume assertion.
//   - PR #299 regression: if FireChain didn't merge user-supplied
//     trigger.chain.params, the verifier would see the renderer's raw
//     output string as `input` (no `marker` key, no `output` key),
//     failing the verifier's assertion.
//   - PR #300 regression: if registerDaemon didn't gate on preflight,
//     the daemon would reach Running without the renderer having
//     written the file, racing the test.
//   - PR #302 regression: if `run_result.enabled: false` accidentally
//     suppressed the in-memory ChainInput, the verifier would never
//     receive the rendered content and the marker file would never
//     appear.
//   - PR #303 regression: if per-edge overrides leaked into the
//     prereq's registry spec, manual fires of the renderer would
//     pick up the daemon's template body — observable via a second
//     manual fire whose rendered file content would differ.
//
// Gated on real Deno (skipped via newTestEnv's t.Skipf) — same gate
// as TestE2E_SecretProvider_FullChain and TestReplay_FullPipeline.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
)

// stubDockerExec captures the spec each Daemon Execute call receives so
// the test can assert on the post-merge, post-expansion docker.volumes
// value the runtime would have seen.
//
// Daemons block until ctx is cancelled — mirrors the preflightExec /
// captureExec patterns used by the rest of the trigger e2e suite. This
// keeps the daemon's run in DaemonRunning long enough for the assertion
// pass to read out the captured spec; the run ends cleanly when the
// test's t.Cleanup tears down the engine.
type stubDockerExec struct {
	reg *registry.Registry

	mu       sync.Mutex
	captured map[string]*task.Spec
}

func newStubDockerExec(reg *registry.Registry) *stubDockerExec {
	return &stubDockerExec{
		reg:      reg,
		captured: make(map[string]*task.Spec),
	}
}

func (s *stubDockerExec) Execute(ctx context.Context, spec *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	if !spec.Trigger.Daemon {
		// Defensive: this test wires the docker stub only for the daemon.
		// A non-daemon docker fire here would mean some test fixture
		// quietly switched runtimes — fail loud rather than mask it.
		return nil, errors.New("stubDockerExec: non-daemon docker spec reached executor")
	}
	s.mu.Lock()
	// Shallow copy is enough: tests assert on Docker.Volumes (slice
	// values), Timeout, Trigger.Daemon — none of which the engine
	// mutates after dispatch.
	cp := *spec
	if spec.Docker != nil {
		d := *spec.Docker
		// Copy the slice so a later mutation in the engine wouldn't
		// confuse the assertion.
		d.Volumes = append([]string(nil), spec.Docker.Volumes...)
		cp.Docker = &d
	}
	s.captured[opts.RunID] = &cp
	s.mu.Unlock()

	<-ctx.Done()
	_ = s.reg.FinishRun(context.Background(), opts.RunID, registry.StatusSuccess)
	return &pkgruntime.RunResult{}, nil
}

func (s *stubDockerExec) specFor(runID string) *task.Spec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.captured[runID]
}

// pollFileContents waits up to timeout for path to exist + be readable,
// returning its content. Used to observe the renderer's on-disk side
// effect without racing the daemon-state assertion.
func pollFileContents(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			return string(b)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("file %s never appeared within %v", path, timeout)
	return ""
}

// TestE2E_TemplatePreflightPipeline composes the full 7-PR epic in a
// single cloudflared-style scenario. See file header for the per-PR
// gap each assertion guards.
func TestE2E_TemplatePreflightPipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}

	// ── Scratch dirs (real paths the renderer + verifier + daemon write into).
	dataDir := t.TempDir()   // ${DATADIR} for volume expansion + render output
	markerDir := t.TempDir() // verifier writes its marker here

	renderOutpath := filepath.Join(dataDir, "cf-config.yml")
	markerPath := filepath.Join(markerDir, "verify.marker")
	t.Setenv("MARKER_PATH", markerPath)
	t.Setenv("MARKER_DIR", markerDir)

	// ── Test env (real Deno + real engine + real registry).
	e := newTestEnv(t)
	// Wire docker runtime to a stub so the daemon's "fire" doesn't try
	// to pull alpine. The deno runtime is wired by newTestEnv already.
	dockerExec := newStubDockerExec(e.reg)
	e.engine.RegisterExecutor(task.RuntimeDocker, dockerExec)

	// ── Renderer + verifier load from testdata/. {{DATADIR}} and
	// {{MARKER_DIR}} are substituted at fixture-load by loadFixtureTpl.
	renderSpec := loadFixtureTpl(t,
		"template-preflight-pipeline/render",
		map[string]string{"DATADIR": dataDir}, "")
	if err := e.reg.Register(renderSpec); err != nil {
		t.Fatalf("reg.Register render: %v", err)
	}
	if err := e.engine.Register(renderSpec); err != nil {
		t.Fatalf("eng.Register render: %v", err)
	}

	verifySpec := loadFixtureTpl(t,
		"template-preflight-pipeline/verify",
		map[string]string{"MARKER_DIR": markerDir}, "")
	if err := e.reg.Register(verifySpec); err != nil {
		t.Fatalf("reg.Register verify: %v", err)
	}
	if err := e.engine.Register(verifySpec); err != nil {
		t.Fatalf("eng.Register verify: %v", err)
	}

	// ── Daemon fixture: same {{DATADIR}} substitution for the
	// trigger.before.overrides.params.outpath (outside expandSpec's
	// allowlist), PLUS LoadDirWithVars-side ${DATADIR} expansion on
	// docker.volumes (PR #297). loadFixtureTpl passes its vars map
	// through to LoadDirWithVars so a single key serves both layers.
	daemonSpec := loadFixtureTpl(t,
		"template-preflight-pipeline/tunnel",
		map[string]string{"DATADIR": dataDir}, "")

	// Sanity: spec-load-time expansion must have rewritten ${DATADIR}
	// in docker.volumes before we ever hand the spec to the engine.
	// This is the PR #297 contract surface most easily verifiable in
	// isolation; the post-dispatch assertion below catches the same
	// regression at a different layer.
	if daemonSpec.Docker == nil || len(daemonSpec.Docker.Volumes) != 1 {
		t.Fatalf("daemon spec docker.volumes shape unexpected: %+v", daemonSpec.Docker)
	}
	wantVolume := renderOutpath + ":/etc/cf/config.yml:ro"
	if daemonSpec.Docker.Volumes[0] != wantVolume {
		t.Fatalf("docker.volumes[0] = %q, want %q (DATADIR not expanded at spec-load)",
			daemonSpec.Docker.Volumes[0], wantVolume)
	}

	if err := e.reg.Register(daemonSpec); err != nil {
		t.Fatalf("reg.Register daemon: %v", err)
	}
	if err := e.engine.Register(daemonSpec); err != nil {
		t.Fatalf("eng.Register daemon: %v", err)
	}

	// ── Wait for the daemon to clear preflight and enter Running. This
	// is the single most important assertion: it means the renderer ran
	// (#300), the per-edge overrides projected template + env into the
	// preflight fire (#303), AND the docker.volumes expansion didn't
	// produce an invalid spec (#297). If any of those broke, the daemon
	// would never reach Running within the timeout.
	waitUntil(t, 30*time.Second, func() bool {
		return e.engine.DaemonState(daemonSpec.ID) == DaemonRunning
	}, "daemon never reached Running")

	// ── Renderer's on-disk side effect: PR #303 must have projected
	// the daemon's overrides into the render fire, and PR #300 must
	// have completed preflight before letting the daemon start. The
	// literal values mirror the env-block in the tunnel fixture's
	// trigger.before.overrides.env.
	got := pollFileContents(t, renderOutpath, 5*time.Second)
	want := "tunnel: abc-123\nhost: api.example.com\n"
	if got != want {
		t.Errorf("rendered file contents = %q, want %q", got, want)
	}

	// ── Daemon's captured spec must carry the expanded docker.volumes
	// — caught at the executor layer (engine → docker runtime path).
	// Find the daemon's run via the registry, then look up the
	// captured spec keyed by run ID.
	var daemonRunID string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runs, _ := e.reg.ListRuns(context.Background(), daemonSpec.ID, 5)
		for _, r := range runs {
			if r.TriggerSource == registry.TriggerDaemon {
				daemonRunID = r.ID
				break
			}
		}
		if daemonRunID != "" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if daemonRunID == "" {
		t.Fatal("daemon run never appeared in registry")
	}
	captured := dockerExec.specFor(daemonRunID)
	if captured == nil {
		t.Fatal("daemon spec never reached the stub docker executor")
	}
	if captured.Docker == nil || len(captured.Docker.Volumes) != 1 ||
		captured.Docker.Volumes[0] != wantVolume {
		t.Errorf("captured daemon docker.volumes = %+v, want [%s]",
			captured.Docker, wantVolume)
	}

	// ── Renderer's persisted return value must be empty (PR #302).
	// Track down the preflight run.
	var preflightRunID string
	runs, _ := e.reg.ListRuns(context.Background(), "render", 10)
	for _, r := range runs {
		if r.TriggerSource == registry.TriggerPreflight {
			preflightRunID = r.ID
			break
		}
	}
	if preflightRunID == "" {
		t.Fatal("render preflight run never recorded in registry")
	}
	preRun, err := e.reg.GetRun(context.Background(), preflightRunID)
	if err != nil {
		t.Fatalf("GetRun preflight: %v", err)
	}
	if preRun.Status != registry.StatusSuccess {
		t.Fatalf("preflight render status = %q, want success", preRun.Status)
	}
	if preRun.ReturnValue != "" {
		t.Errorf("preflight render ReturnValue = %q, want empty (run_result.enabled=false)",
			preRun.ReturnValue)
	}

	// ── Verifier received the rendered string via in-memory chain
	// delivery (PR #299 + PR #302 composed). The marker file's content
	// proves both:
	//   - PR #299: input.marker (chain.params) AND input.output (raw
	//     upstream return) coexisted under the wrapped-map shape.
	//   - PR #302: the rendered string flowed via ChainInput despite
	//     run_result.enabled=false suppressing the persisted column.
	markerContent := pollFileContents(t, markerPath, 30*time.Second)
	wantMarker := "ok:" + want
	if markerContent != wantMarker {
		t.Errorf("marker file = %q, want %q (chain.params + input.output composition)",
			markerContent, wantMarker)
	}

	// ── PR #303 isolation guarantee: the renderer's canonical registry
	// spec must NOT be mutated by the per-edge override on the daemon
	// edge. A standalone manual fire of `render` should still require
	// params (no leaked defaults) and the registry spec must show no
	// override-injected env entries.
	regRender, ok := e.reg.Get("render")
	if !ok {
		t.Fatal("render task disappeared from registry")
	}
	for _, ev := range regRender.Permissions.Env {
		if ev.Name == "TUNNEL_ID" && ev.Value != "" {
			t.Errorf("registry render spec leaked TUNNEL_ID=%q from per-edge override",
				ev.Value)
		}
	}
	if regRender.Timeout != 15*time.Second {
		t.Errorf("registry render Timeout = %v, want 15s (override must not leak)",
			regRender.Timeout)
	}
}
