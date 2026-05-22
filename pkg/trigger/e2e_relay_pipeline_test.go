package trigger

// End-to-end test for the production buildin/relay-server shape after the
// kind: PipelineTask migration (PR6). Composes the REAL buildin/template and
// buildin/write-local tasks (loaded from disk) into a 3-stage sequential
// PipelineTask whose terminal stage is a trigger.daemon: true Task — exactly
// the shape of tasks/buildin/relay-server/task.yaml:
//
//	stage 1: buildin/template      renders relay.yaml from ${VAR} env
//	stage 2: buildin/write-local   persists it to ${DATADIR}/relay/relay.yaml
//	stage 3: relay daemon body     reads the rendered config + parks (daemon)
//
// This replaces the pre-PR6 e2e_relay_template_pipeline_test.go, which
// exercised the same render→write→daemon flow via the now-removed
// trigger.before machinery. The assertions carried over verbatim:
//
//   - both render stages run and the daemon terminal stage reaches Running
//     (proves overrides applied + ${input.output} piped + stages ordered);
//   - ${DATADIR}/relay/relay.yaml is on disk at mode 0600 with the rendered
//     substitutions (BASE_URL, STATUS_PASSWORD);
//   - the renderer's persisted return value stays empty (run_result.enabled:
//     false) while still flowing through the pipeline.
//
// Gated on real Deno (newTestEnv skips when absent).

import (
	"context"
	"io/fs"
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
// buildinTemplateDir — walk up from this source file to the repo root.
// (Moved here from the deleted e2e_oneshot_preflight_test.go; still used by
// e2e_pipeline_task_test.go's loadBuildinWriteLocalAs.)
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

// loadBuildinWriteLocalAs loads the real buildin/write-local task and rebinds
// its ID + Name so multiple subtests can register their own instance without
// colliding in the registry.
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

// relayPipelineDaemonScript stands in for buildin/relay-server-body: it probes
// the pre-rendered relay.yaml the pipeline produced, then parks until the run
// is cancelled (mirroring the real body's wait on http.Server `close`).
const relayPipelineDaemonScript = `
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

// TestE2E_RelayServerPipeline_RealDeno composes the production relay-server
// PipelineTask shape (render → write → daemon body) against real Deno and
// asserts the daemon terminal stage comes up after the two render stages.
func TestE2E_RelayServerPipeline_RealDeno(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}

	dataDir := t.TempDir()
	relayDir := filepath.Join(dataDir, "relay")
	if err := os.MkdirAll(relayDir, 0o755); err != nil {
		t.Fatalf("mkdir relayDir: %v", err)
	}
	relayConfigPath := filepath.Join(relayDir, "relay.yaml")

	const wantBaseURL = "https://relay.test.example.com"
	const wantStatusPW = "test-status-pw"
	t.Setenv("BASE_URL", wantBaseURL)
	t.Setenv("STATUS_PASSWORD", wantStatusPW)
	t.Setenv("DICODE_DATADIR", dataDir)

	e := newTestEnv(t)

	// Stage 1: real buildin/template, with BASE_URL/STATUS_PASSWORD allowlisted.
	tmpl := loadBuildinTemplateAs(t, "tmpl", []task.EnvEntry{
		{Name: "BASE_URL"},
		{Name: "STATUS_PASSWORD"},
	})
	// Stage 2: real buildin/write-local.
	writer := loadBuildinWriteLocalAs(t, "writer")
	// Stage 3: daemon body stub that probes the rendered config + parks.
	dir := t.TempDir()
	daemon := writeTask(t, dir, "relay-body", relayPipelineDaemonScript,
		task.TriggerConfig{Daemon: true, Restart: "never"})
	daemon.Permissions.Env = append(daemon.Permissions.Env, task.EnvEntry{Name: "DICODE_DATADIR"})
	daemon.Permissions.FS = append(daemon.Permissions.FS, task.FSEntry{Path: relayDir, Permission: "rw"})

	for _, s := range []*task.Spec{tmpl, writer, daemon} {
		if err := e.reg.Register(s); err != nil {
			t.Fatalf("reg.Register %s: %v", s.ID, err)
		}
		if err := e.engine.Register(s); err != nil {
			t.Fatalf("eng.Register %s: %v", s.ID, err)
		}
	}

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "relay-server", Name: "Relay Server", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Manual: true},
		Stages: []task.Stage{
			{
				Task: "tmpl",
				Overrides: &task.Overrides{
					Params: task.ParamOverrides{
						{Name: "template", Default: "server:\n  base_url: ${BASE_URL}\n  port: 5553\nstatus:\n  password: ${STATUS_PASSWORD}\n"},
					},
					Env: []task.EnvEntry{{Name: "BASE_URL"}, {Name: "STATUS_PASSWORD"}},
				},
			},
			{
				Task: "writer",
				Overrides: &task.Overrides{
					Params: task.ParamOverrides{
						{Name: "content", Default: "${input.output}"},
						{Name: "path", Default: relayConfigPath},
						{Name: "mode", Default: "0600"},
					},
					Fs: []task.FSEntry{{Path: relayDir, Permission: "rw"}},
				},
			},
			{Task: "relay-body"},
		},
	}
	if err := e.reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register pipe: %v", err)
	}
	if err := e.engine.Register(pipe); err != nil {
		t.Fatalf("eng.Register pipe: %v", err)
	}

	parentRunID, err := e.engine.FireManual(context.Background(), "relay-server", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	// The daemon terminal stage child run must reach 'running' — proving both
	// render stages composed (overrides applied + ${input.output} piped) and
	// the daemon body launched.
	daemonChild := findStageChild(t, e, parentRunID, "relay-body", registry.StatusRunning, 60*time.Second)

	// While the daemon stage is up, the pipeline parent stays 'running'.
	parent, err := e.engine.registry.GetRun(context.Background(), parentRunID)
	if err != nil {
		t.Fatalf("GetRun parent: %v", err)
	}
	if parent.Status != registry.StatusRunning {
		t.Fatalf("pipeline finished %q while daemon terminal stage still running; want 'running'", parent.Status)
	}

	// The rendered file is on disk at mode 0600 with the substitutions applied.
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

	// The renderer's persisted return value stays empty (run_result.enabled:
	// false) but the rendered string still flowed through the pipeline (asserted
	// by the on-disk file above).
	tmplRuns, _ := e.reg.ListRuns(context.Background(), "tmpl", 10)
	var sawTmplStageSuccess bool
	for _, r := range tmplRuns {
		if r.TriggerSource == registry.TriggerPipelineStage && r.Status == registry.StatusSuccess {
			sawTmplStageSuccess = true
			if r.ReturnValue != "" {
				t.Errorf("template stage ReturnValue persisted = %q, want empty (run_result.enabled=false)", r.ReturnValue)
			}
		}
	}
	if !sawTmplStageSuccess {
		t.Error("no template pipeline-stage run with status=success recorded")
	}

	// Clean up: kill the daemon stage run so its goroutine doesn't leak into the
	// rest of the suite. The pipeline parent then finishes with the daemon's
	// terminal status.
	if !e.engine.KillRun(daemonChild.ID) {
		t.Logf("KillRun(daemonChild) returned false (best effort cleanup)")
	}
	waitForTerminal(t, e.engine, parentRunID, 20*time.Second)
}
