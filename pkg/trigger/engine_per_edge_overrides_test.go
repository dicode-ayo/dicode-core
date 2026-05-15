package trigger

// Integration tests for per-edge overrides on trigger.before (preflight
// chains) and trigger.chain (success chains). Both edges reuse
// taskset.ApplyOverrides — these tests pin the contract that:
//
//  1. The override is applied to a deep copy of the prereq/downstream's
//     spec at dispatch time.
//  2. The registry's canonical spec is NOT mutated — manual / cron /
//     unrelated fires of the same task still see the on-disk values.
//  3. Overrides that produce an invalid Spec are rejected, not dispatched.
//
// The preflight test inspects the spec that the executor receives (timeout
// + env) because those are the cheapest end-to-end-observable side effects.
// The chain test goes through the real Deno runtime so it can read back
// the merged Params.Default via the task script.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// writeTaskFromYAML writes a fully-formed task.yaml + task.ts pair to dir/id
// and returns nil on success. Unlike writeTask (engine_test.go) which takes
// a strongly-typed TriggerConfig literal, this helper accepts the raw
// task.yaml body so tests can exercise YAML-level constructs that the
// strongly-typed builder doesn't reach (e.g. trigger.chain.overrides — a
// late-bound *task.Overrides nested under chain).
func writeTaskFromYAML(t *testing.T, dir, id, yaml, script string) error {
	t.Helper()
	td := filepath.Join(dir, id)
	if err := os.MkdirAll(td, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(td, "task.yaml"), []byte(yaml), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(td, "task.ts"), []byte(script), 0o644)
}

// captureExec records the spec the executor was handed for every Execute
// call, keyed by run ID. Lets per-edge override tests assert that the
// override-merged spec (not the registry's canonical spec) is what reaches
// the runtime layer. Adapted from preflightExec — kept separate so the
// captureExec ↔ test contract is obvious from the call sites below.
type captureExec struct {
	reg *registry.Registry

	mu       sync.Mutex
	captured map[string]*task.Spec
	daemonCh chan struct{}
}

func newCaptureExec(reg *registry.Registry) *captureExec {
	return &captureExec{
		reg:      reg,
		captured: make(map[string]*task.Spec),
		daemonCh: make(chan struct{}),
	}
}

func (c *captureExec) Execute(ctx context.Context, spec *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	c.mu.Lock()
	// Take a shallow copy: tests assert on Timeout, Env, Params, Runtime —
	// not on slice identity.
	specCopy := *spec
	c.captured[opts.RunID] = &specCopy
	c.mu.Unlock()

	// Daemons block until ctx cancelled — mirrors preflightExec's daemon path.
	if spec.Trigger.Daemon {
		<-ctx.Done()
		_ = c.reg.FinishRun(context.Background(), opts.RunID, registry.StatusSuccess)
		return &pkgruntime.RunResult{}, nil
	}
	_ = c.reg.FinishRun(context.Background(), opts.RunID, registry.StatusSuccess)
	return &pkgruntime.RunResult{}, nil
}

func (c *captureExec) specFor(runID string) *task.Spec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.captured[runID]
}

func (c *captureExec) specForTask(reg *registry.Registry, taskID string, triggerSrc registry.TriggerSource) *task.Spec {
	runs, _ := reg.ListRuns(context.Background(), taskID, 20)
	for _, r := range runs {
		if r.TriggerSource == triggerSrc {
			if s := c.specFor(r.ID); s != nil {
				return s
			}
		}
	}
	return nil
}

func newCaptureEnv(t *testing.T) (*Engine, *registry.Registry, *captureExec) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	exec := newCaptureExec(reg)
	eng := New(reg, exec, zap.NewNop())
	eng.RegisterExecutor(task.RuntimeDocker, exec)
	return eng, reg, exec
}

// TestPerEdgeOverrides_BeforeAppliesAtPreflightOnly verifies that a
// `before: [{task: render, overrides: {timeout: 5m}}]` edge dispatches
// the prereq with the override-merged timeout, while the prereq's own
// registry spec (and standalone manual fires) keep the 60s value from
// task.yaml.
func TestPerEdgeOverrides_BeforeAppliesAtPreflightOnly(t *testing.T) {
	eng, reg, exec := newCaptureEnv(t)

	// Prereq: on-disk timeout 60s.
	prereqOriginalTimeout := 60 * time.Second
	prereq := &task.Spec{
		ID:      "render",
		Name:    "render",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: prereqOriginalTimeout,
		Enabled: true,
	}
	if err := reg.Register(prereq); err != nil {
		t.Fatalf("reg.Register prereq: %v", err)
	}

	// Daemon: declares a per-edge override on the render prereq with
	// timeout 5m. The override should apply only to the preflight fire.
	overrideTimeout := 5 * time.Minute
	daemon := &task.Spec{
		ID:      "d",
		Name:    "d",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{
			Daemon:  true,
			Restart: "never",
			Before: []task.BeforeEntry{{
				Task: "render",
				Overrides: &task.Overrides{
					Timeout: overrideTimeout,
				},
			}},
		},
		Enabled: true,
	}
	if err := reg.Register(daemon); err != nil {
		t.Fatalf("reg.Register daemon: %v", err)
	}
	if err := eng.Register(daemon); err != nil {
		t.Fatalf("eng.Register daemon: %v", err)
	}

	// Wait for daemon to come up (preflight succeeded).
	waitUntil(t, 5*time.Second, func() bool {
		return eng.DaemonState("d") == DaemonRunning
	}, "daemon never reached Running")

	// The preflight fire of render should have used the override timeout.
	preflightSpec := exec.specForTask(reg, "render", registry.TriggerPreflight)
	if preflightSpec == nil {
		t.Fatal("preflight render run never reached executor")
	}
	if preflightSpec.Timeout != overrideTimeout {
		t.Errorf("preflight render Timeout = %v, want %v (override should apply)",
			preflightSpec.Timeout, overrideTimeout)
	}

	// The registry's canonical spec must NOT have been mutated.
	regSpec, _ := reg.Get("render")
	if regSpec.Timeout != prereqOriginalTimeout {
		t.Errorf("registry render spec Timeout = %v, want %v (registry must not be mutated)",
			regSpec.Timeout, prereqOriginalTimeout)
	}

	// Standalone manual fire must see the original timeout.
	manualRunID, err := eng.FireManual(context.Background(), "render", nil)
	if err != nil {
		t.Fatalf("FireManual render: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		s := exec.specFor(manualRunID)
		return s != nil
	}, "manual render never reached executor")
	manualSpec := exec.specFor(manualRunID)
	if manualSpec.Timeout != prereqOriginalTimeout {
		t.Errorf("manual render Timeout = %v, want %v (override must not leak to manual fires)",
			manualSpec.Timeout, prereqOriginalTimeout)
	}
}

// TestPerEdgeOverrides_BeforeInvalidOverrideRejectedAtRegister verifies
// that overrides which would produce an invalid Spec are caught at
// Engine.Register time. Operators see a clean error path rather than a
// silently-failing daemon that stays in PrereqFailed forever.
//
// We use runtime: "" because Spec.validate accepts any non-empty runtime
// but the empty case routes through the deno default path — instead
// trigger an invariant we can actually break: a chain trigger with no
// `from:`. The override switches the prereq to a chain trigger that
// fails per-spec validation.
func TestPerEdgeOverrides_BeforeInvalidOverrideRejectedAtRegister(t *testing.T) {
	eng, reg, _ := newCaptureEnv(t)

	prereq := &task.Spec{
		ID:      "render",
		Name:    "render",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 60 * time.Second,
		Enabled: true,
	}
	if err := reg.Register(prereq); err != nil {
		t.Fatal(err)
	}

	emptyFrom := ""
	daemon := &task.Spec{
		ID:      "d",
		Name:    "d",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{
			Daemon: true,
			Before: []task.BeforeEntry{{
				Task: "render",
				Overrides: &task.Overrides{
					Trigger: &task.TriggerPatch{
						Chain: &task.ChainTrigger{From: emptyFrom},
					},
				},
			}},
		},
		Enabled: true,
	}
	if err := reg.Register(daemon); err != nil {
		t.Fatal(err)
	}

	err := eng.Register(daemon)
	if err == nil {
		t.Fatal("expected Register to fail with invalid override, got nil")
	}
	if !strings.Contains(err.Error(), "invalid spec") {
		t.Errorf("expected error mentioning invalid spec, got: %v", err)
	}
}

// TestPerEdgeOverrides_ChainAppliesAtChainOnly is the end-to-end chain
// dispatch test. It uses the real Deno runtime path (writeTask + newTestEnv
// from engine_test.go's helpers) so we can read back the merged spec
// values via the task script's return value.
//
// Upstream completes; downstream's trigger.chain.overrides patches
// params.mode to "prod". The downstream task reads params.mode at runtime
// and returns it; the test asserts the value is the override-merged
// "prod", not the on-disk default "review".
func TestPerEdgeOverrides_ChainAppliesAtChainOnly(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	// Downstream task: declares trigger.chain on "upstream-edge" with an
	// override that changes the `mode` param default from "review" to
	// "prod". The script echoes the param so the test can compare.
	defaultMode := "review"
	overrideMode := "prod"

	downstreamYAML := `
name: downstream-edge
runtime: deno
trigger:
  chain:
    from: upstream-edge
    on: success
    overrides:
      params:
        mode: ` + overrideMode + `
params:
  mode:
    default: ` + defaultMode + `
`
	// writeTask doesn't support trigger.chain.overrides directly because
	// the helper takes a strongly-typed TriggerConfig literal. Build the
	// spec via WriteFile + LoadDir instead so we exercise the real YAML
	// path the engine sees in production.
	if err := writeTaskFromYAML(t, dir, "downstream-edge", downstreamYAML,
		`export default async function main({ params }) { return await params.get("mode") }`); err != nil {
		t.Fatalf("write downstream: %v", err)
	}
	downstream, err := task.LoadDir(dir + "/downstream-edge")
	if err != nil {
		t.Fatalf("load downstream: %v", err)
	}
	if err := e.reg.Register(downstream); err != nil {
		t.Fatalf("reg.Register downstream: %v", err)
	}
	if err := e.engine.Register(downstream); err != nil {
		t.Fatalf("eng.Register downstream: %v", err)
	}

	// Upstream: just returns; the engine fires downstream on success.
	upstream := writeTask(t, dir, "upstream-edge",
		`export default async function main() { return "go" }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(upstream)
	if err := e.engine.Register(upstream); err != nil {
		t.Fatalf("eng.Register upstream: %v", err)
	}

	// Fire upstream.
	upstreamRunID, err := e.engine.FireManual(context.Background(), "upstream-edge", nil)
	if err != nil {
		t.Fatalf("FireManual upstream: %v", err)
	}
	primary := waitForTerminal(t, e.engine, upstreamRunID, 30*time.Second)
	if primary.Status != registry.StatusSuccess {
		t.Fatalf("upstream status = %q, want success", primary.Status)
	}

	// Wait for the chain-fired downstream run and inspect its return.
	got := waitForRunOfTask(t, e.engine, "downstream-edge", 30*time.Second)
	if got == nil {
		t.Fatal("downstream-edge was never fired via chain")
	}
	if got.Status != registry.StatusSuccess {
		t.Errorf("downstream status = %q, want success", got.Status)
	}
	returnValue := pollReturnValue(t, e.engine, got.ID, 5*time.Second)
	var mode string
	if err := json.Unmarshal([]byte(returnValue), &mode); err != nil {
		t.Fatalf("unmarshal return %q: %v", returnValue, err)
	}
	if mode != overrideMode {
		t.Errorf("chain-fired downstream params.mode = %q, want %q (override should apply)",
			mode, overrideMode)
	}

	// Now fire the downstream manually — wait, downstream's trigger is
	// chain-only, so manual fire would be rejected by the engine. The
	// "doesn't leak to manual" half of this assertion is covered by the
	// before-edge test above (where a manual fire is legal because the
	// prereq's trigger is Manual). Instead assert the registry's
	// canonical downstream spec was NOT mutated.
	regDownstream, _ := e.reg.Get("downstream-edge")
	if len(regDownstream.Params) != 1 || regDownstream.Params[0].Name != "mode" {
		t.Fatalf("registry downstream params = %+v, want [mode]", regDownstream.Params)
	}
	if regDownstream.Params[0].Default != defaultMode {
		t.Errorf("registry downstream params.mode.default = %q, want %q (registry must not be mutated)",
			regDownstream.Params[0].Default, defaultMode)
	}
}
