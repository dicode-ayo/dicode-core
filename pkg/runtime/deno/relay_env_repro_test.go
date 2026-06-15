package deno

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/task"
)

// TestEnforcement_Env_RelayImportNeedsReadExposed reproduces dicode-core#412:
// the buildin/relay-server-body task does `import "npm:dicode-relay/start"`
// (Deno node-compat), whose transitive deps read process.env at module init
// beyond any small declared set. Under the restrictive --allow-env built from
// declared names the import throws NotCapable; with env_read_exposed (bare
// --allow-env) it imports cleanly while named entries still forward their host
// values.
//
// Skipped in -short / when Deno or the npm package can't be fetched.
func TestEnforcement_Env_RelayImportNeedsReadExposed(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno + npm registry access")
	}
	e := newTestEnv(t)
	t.Setenv("DICODE_DATADIR", "/tmp/relay-repro-datadir")

	const script = `import { startServer } from "npm:dicode-relay@^0.1.6/start";
export default async function main() {
  return typeof startServer + ":" + (Deno.env.get("DICODE_DATADIR") ?? "MISSING");
}`

	logsFor := func(id string) string {
		runs, _ := e.reg.ListRuns(context.Background(), id, 5)
		var b strings.Builder
		for _, run := range runs {
			entries, _ := e.reg.GetRunLogs(context.Background(), run.ID)
			for _, l := range entries {
				b.WriteString(l.Message)
				b.WriteByte('\n')
			}
		}
		return b.String()
	}

	// Baseline: declared names only → import is denied (NotCapable).
	denied := &task.Spec{
		ID: "relay-denied", Name: "relay-denied", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 120 * time.Second,
		Permissions: task.Permissions{
			Net: []string{"*"},
			Env: []task.EnvEntry{{Name: "DICODE_DATADIR"}, {Name: "DICODE_VERSION"}},
		},
	}
	r := e.runSpec(t, script, denied)
	if r.Error == nil {
		t.Fatalf("expected NotCapable under declared-only env, but import succeeded: %v", r.ReturnValue)
	}
	if logs := logsFor("relay-denied"); !strings.Contains(logs, "NotCapable") {
		t.Fatalf("expected NotCapable in run logs, got:\n%s", logs)
	}

	// Fix: env_read_exposed + named entries → import succeeds AND DICODE_DATADIR
	// is forwarded (mirrors tasks/buildin/relay-server-body/task.yaml).
	exposed := &task.Spec{
		ID: "relay-exposed", Name: "relay-exposed", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 120 * time.Second,
		Permissions: task.Permissions{
			Net:            []string{"*"},
			EnvReadExposed: true,
			Env:            []task.EnvEntry{{Name: "DICODE_DATADIR"}, {Name: "DICODE_VERSION"}},
		},
	}
	r = e.runSpec(t, script, exposed)
	if r.Error != nil {
		t.Fatalf("relay import still failed under env_read_exposed: %v\nlogs:\n%s", r.Error, logsFor("relay-exposed"))
	}
	if r.ReturnValue != "function:/tmp/relay-repro-datadir" {
		t.Errorf("expected startServer imported and DICODE_DATADIR forwarded, got %v", r.ReturnValue)
	}
}
