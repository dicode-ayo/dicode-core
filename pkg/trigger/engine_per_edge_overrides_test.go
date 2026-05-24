package trigger

// Integration test for per-edge overrides on trigger.chain (success chains).
// Pins the contract that:
//
//  1. The override is applied to a deep copy of the downstream's spec at
//     dispatch time.
//  2. The registry's canonical spec is NOT mutated — manual / cron /
//     unrelated fires of the same task still see the on-disk values.
//
// (The trigger.before per-edge override tests were removed in PR6 along with
// trigger.before itself; the equivalent stage-override behaviour for kind:
// PipelineTask is covered by the pipeline runner tests.)
//
// The chain test goes through the real Deno runtime so it can read back the
// merged Params.Default via the task script.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
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

	// downstream's trigger is chain-only, so a manual fire would be rejected
	// by the engine; instead assert the registry's canonical downstream spec
	// was NOT mutated by the chain-edge override.
	regDownstream, _ := e.reg.Get("downstream-edge")
	if len(regDownstream.Params) != 1 || regDownstream.Params[0].Name != "mode" {
		t.Fatalf("registry downstream params = %+v, want [mode]", regDownstream.Params)
	}
	if regDownstream.Params[0].Default != defaultMode {
		t.Errorf("registry downstream params.mode.default = %q, want %q (registry must not be mutated)",
			regDownstream.Params[0].Default, defaultMode)
	}
}
