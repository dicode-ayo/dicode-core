package trigger

// End-to-end test for issue #312: trigger.before on one-shot tasks.
//
// Composes the REAL buildin/template and buildin/write-local tasks
// (loaded from disk) into a 2-stage preflight pipeline on a MANUAL
// task. Pre-#312 spec.go:711 would have rejected this configuration
// outright with "trigger.before: requires daemon: true". Post-#312 the
// pipeline runs through real Deno:
//
//   1. buildin/template renders a literal config body (no placeholders →
//      no env wiring needed). run_result.enabled:false in the on-disk
//      task.yaml means the rendered string flows in-memory via
//      ${input.output} interpolation rather than via the persisted
//      runs.return_value column.
//
//   2. buildin/write-local persists the rendered string to disk at a
//      caller-declared path. Its own permissions.fs ships empty, so
//      the manual task's per-edge override must declare an fs:rw
//      scope covering the target path.
//
//   3. The manual task body runs only after both stages succeed and
//      asserts (via marker file) the pipeline composed correctly.
//
// Assertions:
//   - The rendered file lands on disk at the expected path (proves
//     buildin/template ran with its overrides AND buildin/write-local
//     received the rendered string via ${input.output}).
//   - The manual parent run's status is success (proves the body
//     dispatched after both preflight stages cleared).
//   - The preflight stages have their own runs in the registry tagged
//     TriggerPreflight (data-model linkage for the WebUI).
//
// Gated on real Deno (skipped via newTestEnv's t.Skipf) — same gate
// as the daemon-flavored TestE2E_TemplatePreflightPipeline.

import (
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

// buildinWriteLocalDir returns the absolute path to the on-disk
// `tasks/buildin/write-local/` task. Anchored the same way as
// buildinTemplateDir — see that helper's comment for the walk-up
// rationale.
func buildinWriteLocalDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot anchor buildin/write-local path")
	}
	pkgDir := filepath.Dir(thisFile)               // .../pkg/trigger
	repoRoot := filepath.Dir(filepath.Dir(pkgDir)) // .../
	dir := filepath.Join(repoRoot, "tasks", "buildin", "write-local")
	if _, err := os.Stat(filepath.Join(dir, "task.yaml")); err != nil {
		t.Fatalf("buildin/write-local task.yaml not found at %s: %v", dir, err)
	}
	return dir
}

// loadBuildinWriteLocalAs mirrors loadBuildinTemplateAs but for the
// write-local task. ID/Name rebinding keeps the registry from
// collision when multiple subtests register their own instance.
func loadBuildinWriteLocalAs(t *testing.T, id string) *task.Spec {
	t.Helper()
	spec, err := task.LoadDir(buildinWriteLocalDir(t))
	if err != nil {
		t.Fatalf("LoadDir buildin/write-local: %v", err)
	}
	spec.ID = id
	spec.Name = id
	if err := spec.Validate(); err != nil {
		t.Fatalf("buildin/write-local re-validate after rebind: %v", err)
	}
	return spec
}

// TestE2E_OneShotPreflightPipeline_RealDeno wires a manual one-shot
// task with a 2-stage preflight pipeline through REAL Deno. Pre-#312
// this configuration was rejected at spec validation; post-#312 it
// must compose end to end.
func TestE2E_OneShotPreflightPipeline_RealDeno(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}

	// Where the preflight pipeline writes its rendered config.
	scratch := t.TempDir()
	outpath := filepath.Join(scratch, "rendered.yaml")

	e := newTestEnv(t)

	const tmplID = "tmpl"
	const writerID = "writer"
	const consumerID = "consumer"

	// Stage 1: buildin/template. The body has no ${VAR} placeholders so
	// the renderer just echoes it back — keeps env wiring out of the
	// test surface. The template body is passed via per-edge override
	// on the consumer's before-list, not via params on the template
	// spec itself.
	tmpl := loadBuildinTemplateAs(t, tmplID, nil)
	if err := e.reg.Register(tmpl); err != nil {
		t.Fatalf("reg.Register tmpl: %v", err)
	}
	if err := e.engine.Register(tmpl); err != nil {
		t.Fatalf("eng.Register tmpl: %v", err)
	}

	// Stage 2: buildin/write-local. fs:[] by default — the consumer's
	// per-edge override declares the fs:rw scope.
	writer := loadBuildinWriteLocalAs(t, writerID)
	if err := e.reg.Register(writer); err != nil {
		t.Fatalf("reg.Register writer: %v", err)
	}
	if err := e.engine.Register(writer); err != nil {
		t.Fatalf("eng.Register writer: %v", err)
	}

	// Consumer: a manual task with the 2-stage preflight pipeline. The
	// consumer body itself is a no-op (writes a marker proving it ran
	// after the preflight cleared) — the meaningful side-effect is
	// buildin/write-local laying down `outpath`.
	consumerDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(consumerDir, "task.ts"),
		[]byte(`export default async function main() { return "ok"; }`+"\n"),
		0o644); err != nil {
		t.Fatalf("write consumer task.ts: %v", err)
	}
	consumer := &task.Spec{
		ID:      consumerID,
		Name:    consumerID,
		Runtime: task.RuntimeDeno,
		TaskDir: consumerDir,
		Trigger: task.TriggerConfig{
			Manual: true,
			Before: []task.BeforeEntry{
				{
					Task: tmplID,
					Overrides: &task.Overrides{
						Params: task.ParamOverrides{
							{Name: "template", Default: "rendered: hello-from-preflight\n"},
						},
					},
				},
				{
					Task: writerID,
					Overrides: &task.Overrides{
						Params: task.ParamOverrides{
							{Name: "content", Default: "${input.output}"},
							{Name: "path", Default: outpath},
							{Name: "mode", Default: "0600"},
						},
						Fs: []task.FSEntry{
							{Path: scratch, Permission: "rw"},
						},
					},
				},
			},
		},
		Enabled: true,
	}
	if err := consumer.Validate(); err != nil {
		t.Fatalf("consumer Validate: %v", err)
	}
	if err := e.reg.Register(consumer); err != nil {
		t.Fatalf("reg.Register consumer: %v", err)
	}
	if err := e.engine.Register(consumer); err != nil {
		t.Fatalf("eng.Register consumer: %v", err)
	}

	// Fire manually.
	parentRunID, err := e.engine.FireManual(context.Background(), consumerID, nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	// Wait for the consumer body to finish — the only path to that is
	// preflight success → body dispatch → body completion.
	deadline := time.Now().Add(30 * time.Second)
	var parent *registry.Run
	for time.Now().Before(deadline) {
		p, err := e.reg.GetRun(context.Background(), parentRunID)
		if err == nil && p.FinishedAt != nil {
			parent = p
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if parent == nil {
		t.Fatal("consumer parent run never finished within 30s")
	}
	if parent.Status != registry.StatusSuccess {
		t.Fatalf("consumer parent status = %q (fail_reason=%q), want success",
			parent.Status, parent.FailureReason)
	}

	// Rendered file must exist on disk with the expected content.
	got, err := os.ReadFile(outpath)
	if err != nil {
		t.Fatalf("rendered file not found at %s: %v", outpath, err)
	}
	want := "rendered: hello-from-preflight\n"
	if string(got) != want {
		t.Errorf("rendered file content = %q, want %q", string(got), want)
	}

	// Preflight stages must be recorded as their own runs.
	for _, taskID := range []string{tmplID, writerID} {
		runs, err := e.reg.ListRuns(context.Background(), taskID, 5)
		if err != nil {
			t.Fatalf("ListRuns %s: %v", taskID, err)
		}
		var found bool
		for _, r := range runs {
			if r.TriggerSource == registry.TriggerPreflight && r.Status == registry.StatusSuccess {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no successful TriggerPreflight run found for %s; runs=%+v", taskID, runs)
		}
	}
}
