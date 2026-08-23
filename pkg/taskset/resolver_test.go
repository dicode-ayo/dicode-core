package taskset

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func newObservedLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.WarnLevel)
	return zap.New(core), logs
}

func newResolver(t *testing.T) *Resolver {
	t.Helper()
	return NewResolver(t.TempDir(), false, zap.NewNop())
}

// writeTaskDir writes a minimal task.yaml + task.js into dir/name/ and returns
// the absolute path to the task directory.
func writeTaskDir(t *testing.T, parent, name string, extra ...string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	cron := "0 8 * * *"
	if len(extra) > 0 {
		cron = extra[0]
	}
	yaml := "kind: Task\napiVersion: dicode/v1\nname: " + name + "\nruntime: deno\ntrigger:\n  cron: \"" + cron + "\"\n"
	writeFile(t, dir, "task.yaml", yaml)
	writeFile(t, dir, "task.js", "// task")
	return dir
}

// writePipelineDir writes a minimal kind: PipelineTask task.yaml into dir/name/
// and returns the absolute path to the task directory.
func writePipelineDir(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	yaml := "apiVersion: dicode/v1\nkind: PipelineTask\nname: " + name +
		"\nsubtype: sequential\ntrigger:\n  manual: true\nstages:\n  - task: buildin/template\n"
	writeFile(t, dir, "task.yaml", yaml)
	return dir
}

// rtSpec extracts the *task.Spec carried by a resolved kind: Task. Tests use it
// after ResolvedTask migrated from a typed *task.Spec field to task.Kinded.
func rtSpec(rt *ResolvedTask) *task.Spec { return rt.Kinded.(*task.Spec) }

// writeTaskSet writes a taskset.yaml into dir/name.yaml and returns the path.
func writeTaskSetFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	writeFile(t, dir, name, content)
	return p
}

// ── joinNamespace ─────────────────────────────────────────────────────────────

func TestJoinNamespace(t *testing.T) {
	tests := []struct{ ns, key, want string }{
		{"infra", "deploy", "infra/deploy"},
		{"", "deploy", "deploy"},
		{"a/b", "c", "a/b/c"},
	}
	for _, tc := range tests {
		got := joinNamespace(tc.ns, tc.key)
		if got != tc.want {
			t.Errorf("joinNamespace(%q,%q) = %q, want %q", tc.ns, tc.key, got, tc.want)
		}
	}
}

// ── buildOverrideLayers ───────────────────────────────────────────────────────

func TestBuildOverrideLayers_Order(t *testing.T) {
	// Three-level stack (lowest to highest): setDefaults → parentEntryOverride → entryOverrides.
	set := &Defaults{Timeout: 20 * time.Second}
	parentEntry := &Overrides{Timeout: 40 * time.Second}
	entry := &Overrides{Timeout: 50 * time.Second}

	layers := buildOverrideLayers(set, parentEntry, entry)

	base := &task.Spec{Name: "x", Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}}
	got := applyOverrides(base, layers...)
	// Entry (50s) is highest.
	if got.Timeout != 50*time.Second {
		t.Errorf("leaf should win: got %v", got.Timeout)
	}
}

func TestBuildOverrideLayers_EntryBeatsSetDefaults(t *testing.T) {
	// Entry overrides (level 3) beat set defaults (level 1).
	set := &Defaults{Timeout: 20 * time.Second}
	entry := &Overrides{Timeout: 50 * time.Second}

	layers := buildOverrideLayers(set, nil, entry)
	base := &task.Spec{Name: "x", Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}}
	got := applyOverrides(base, layers...)
	if got.Timeout != 50*time.Second {
		t.Errorf("entry should beat set defaults: got %v", got.Timeout)
	}
}

func TestBuildOverrideLayers_ParentEntryBeatsSetDefaults(t *testing.T) {
	// Parent entry patch (level 2) beats set defaults (level 1).
	set := &Defaults{Timeout: 20 * time.Second}
	parentEntry := &Overrides{Timeout: 40 * time.Second}

	layers := buildOverrideLayers(set, parentEntry, nil)
	base := &task.Spec{Name: "x", Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}}
	got := applyOverrides(base, layers...)
	if got.Timeout != 40*time.Second {
		t.Errorf("parent entry patch should beat set defaults: got %v", got.Timeout)
	}
}

// ── Resolver local resolution ─────────────────────────────────────────────────

func TestResolver_SingleTask(t *testing.T) {
	repoDir := t.TempDir()
	taskDir := writeTaskDir(t, repoDir, "deploy")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    deploy:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)

	r := newResolver(t)
	rootRef := &Ref{Path: tsPath}
	results, _, err := r.Resolve(context.Background(), "infra", rootRef, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].ID != "infra/deploy" {
		t.Errorf("ID: got %q", results[0].ID)
	}
	if rtSpec(results[0]).Name != "deploy" {
		t.Errorf("spec.name: %q", rtSpec(results[0]).Name)
	}
}

// TestResolvePipelineKind asserts a taskset ref to a kind: PipelineTask dir
// resolves to a ResolvedTask carrying a *task.PipelineTask under the namespaced
// ID (no override layers — pipelines carry stage overrides in their own spec).
func TestResolvePipelineKind(t *testing.T) {
	repoDir := t.TempDir()
	pipeDir := writePipelineDir(t, repoDir, "release")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    release:
      ref:
        path: ` + filepath.Join(pipeDir, "task.yaml") + `
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)

	r := newResolver(t)
	results, _, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].ID != "infra/release" {
		t.Errorf("ID: got %q", results[0].ID)
	}
	if results[0].Kinded == nil || results[0].Kinded.KindOf() != task.KindPipelineTask {
		t.Fatalf("want PipelineTask, got %v", results[0].Kinded)
	}
	if results[0].Kinded.TaskID() != "infra/release" {
		t.Errorf("pipeline ID: got %q, want infra/release", results[0].Kinded.TaskID())
	}
}

func TestResolver_NamespaceBuildsCorrectly(t *testing.T) {
	repoDir := t.TempDir()
	taskA := writeTaskDir(t, repoDir, "task-a")
	taskB := writeTaskDir(t, repoDir, "task-b")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: root
spec:
  entries:
    task-a:
      ref:
        path: ` + filepath.Join(taskA, "task.yaml") + `
    task-b:
      ref:
        path: ` + filepath.Join(taskB, "task.yaml") + `
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)
	r := newResolver(t)
	results, _, err := r.Resolve(context.Background(), "team", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	ids := make(map[string]bool)
	for _, rt := range results {
		ids[rt.ID] = true
	}
	if !ids["team/task-a"] || !ids["team/task-b"] {
		t.Errorf("IDs: %v", ids)
	}
}

func TestResolver_OverrideApplied(t *testing.T) {
	repoDir := t.TempDir()
	taskDir := writeTaskDir(t, repoDir, "deploy")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    deploy:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
      overrides:
        trigger:
          cron: "0 2 * * *"
        env:
          - DEPLOY_TARGET=prod
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)
	r := newResolver(t)
	results, _, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1")
	}
	spec := rtSpec(results[0])
	if spec.Trigger.Cron != "0 2 * * *" {
		t.Errorf("cron: %q", spec.Trigger.Cron)
	}
	em := envMap(spec.Permissions.Env)
	if em["DEPLOY_TARGET"] != "prod" {
		t.Errorf("env not merged: %v", spec.Permissions.Env)
	}
}

func TestResolver_DisabledEntrySkipped(t *testing.T) {
	// Disabled tasks now remain in results (Enabled=false) for API visibility.
	// The trigger engine skips scheduling them; the registry keeps them visible.
	repoDir := t.TempDir()
	taskDir := writeTaskDir(t, repoDir, "deploy")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    deploy:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
      overrides:
        enabled: false
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)
	r := newResolver(t)
	results, _, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("disabled task should appear with Enabled=false: got %d results", len(results))
	}
	if rtSpec(results[0]).Enabled {
		t.Errorf("disabled task should have Enabled=false, got Enabled=true")
	}
}

func TestResolver_ParentEntryPatchDisables(t *testing.T) {
	// Task is enabled in taskset.yaml but parent patches it to disabled.
	// The task remains in results with Enabled=false for API visibility.
	repoDir := t.TempDir()
	taskDir := writeTaskDir(t, repoDir, "deploy")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: backend
spec:
  entries:
    deploy:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
      overrides:
        enabled: true
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)
	r := newResolver(t)
	parentOverrides := &Overrides{
		Entries: map[string]*Overrides{
			"deploy": {Enabled: boolPtr(false)},
		},
	}
	results, _, err := r.Resolve(context.Background(), "infra/backend", &Ref{Path: tsPath}, nil, parentOverrides, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("parent-disabled task should appear with Enabled=false: got %d results", len(results))
	}
	if rtSpec(results[0]).Enabled {
		t.Errorf("parent-disabled task should have Enabled=false, got Enabled=true")
	}
}

func TestResolver_SetDefaultsApplied(t *testing.T) {
	repoDir := t.TempDir()
	taskDir := writeTaskDir(t, repoDir, "deploy")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  defaults:
    timeout: 90s
    env:
      - LOG=info
  entries:
    deploy:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)
	r := newResolver(t)
	results, _, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1")
	}
	spec := rtSpec(results[0])
	if spec.Timeout != 90*time.Second {
		t.Errorf("timeout from defaults: got %v", spec.Timeout)
	}
	em := envMap(spec.Permissions.Env)
	if em["LOG"] != "info" {
		t.Errorf("env from defaults not applied: %v", spec.Permissions.Env)
	}
}

func TestResolver_ConfigDefaultsDeprecated(t *testing.T) {
	// configDefaults passed to Resolve are now deprecated and NOT applied to the override stack.
	// A deprecation warning is emitted; the resolved spec retains task.yaml values.
	repoDir := t.TempDir()
	taskDir := writeTaskDir(t, repoDir, "deploy")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    deploy:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)
	// Use an observed logger so we can verify the deprecation warning is emitted.
	logger, logs := newObservedLogger()
	r := NewResolver(t.TempDir(), false, logger)
	configDefaults := &Defaults{
		Timeout: 120 * time.Second,
		Env:     []task.EnvEntry{{Name: "RUNTIME_ENV", Value: "backend"}},
	}
	results, _, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, configDefaults, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1")
	}
	spec := rtSpec(results[0])
	// Timeout should NOT be overridden by configDefaults.
	if spec.Timeout == 120*time.Second {
		t.Errorf("deprecated configDefaults should not be applied: timeout was set to 120s")
	}
	em := envMap(spec.Permissions.Env)
	if em["RUNTIME_ENV"] == "backend" {
		t.Errorf("deprecated configDefaults env should not be applied: found RUNTIME_ENV=backend")
	}
	// Deprecation warning must have been logged.
	if logs.FilterMessageSnippet("kind:Config spec.defaults is deprecated").Len() == 0 {
		t.Error("expected deprecation warning for configDefaults")
	}
}

func TestResolver_EntryOverrideBeatsSetDefaults(t *testing.T) {
	// Entry overrides (level 3) must beat set defaults (level 1).
	repoDir := t.TempDir()
	taskDir := writeTaskDir(t, repoDir, "deploy")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  defaults:
    timeout: 120s
  entries:
    deploy:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
      overrides:
        timeout: 30s
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)
	r := newResolver(t)
	results, _, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if rtSpec(results[0]).Timeout != 30*time.Second {
		t.Errorf("entry override should beat set defaults: got %v", rtSpec(results[0]).Timeout)
	}
}

func TestResolver_NestedTaskSet(t *testing.T) {
	rootDir := t.TempDir()
	nestedDir := t.TempDir()
	taskDir := writeTaskDir(t, nestedDir, "api-deploy")

	nestedTS := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: backend
spec:
  entries:
    api-deploy:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
`
	nestedPath := writeTaskSetFile(t, nestedDir, "taskset.yaml", nestedTS)

	rootTS := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    backend:
      ref:
        path: ` + nestedPath + `
`
	rootPath := writeTaskSetFile(t, rootDir, "taskset.yaml", rootTS)

	r := newResolver(t)
	results, _, err := r.Resolve(context.Background(), "infra", &Ref{Path: rootPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1, got %d", len(results))
	}
	if results[0].ID != "infra/backend/api-deploy" {
		t.Errorf("nested ID: got %q", results[0].ID)
	}
}

func TestResolver_NestedOverrideFromParent(t *testing.T) {
	// Parent patches a task inside a nested set via overrides.entries.
	rootDir := t.TempDir()
	nestedDir := t.TempDir()
	taskDir := writeTaskDir(t, nestedDir, "deploy")

	nestedTS := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: backend
spec:
  entries:
    deploy:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
      overrides:
        trigger:
          cron: "0 4 * * *"
`
	nestedPath := writeTaskSetFile(t, nestedDir, "taskset.yaml", nestedTS)

	rootTS := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    backend:
      ref:
        path: ` + nestedPath + `
      overrides:
        entries:
          deploy:
            trigger:
              cron: "0 3 * * *"
`
	rootPath := writeTaskSetFile(t, rootDir, "taskset.yaml", rootTS)

	r := newResolver(t)
	results, _, err := r.Resolve(context.Background(), "infra", &Ref{Path: rootPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1, got %d", len(results))
	}
	// Nested entry's own override (0 4 * * *) beats parent entry patch (0 3 * * *) — leaf wins.
	if rtSpec(results[0]).Trigger.Cron != "0 4 * * *" {
		t.Errorf("leaf should win: got %q", rtSpec(results[0]).Trigger.Cron)
	}
}

func TestResolver_RepoDedupLocalRefs(t *testing.T) {
	// Two entries pointing to the same local path are both resolved correctly.
	repoDir := t.TempDir()
	taskA := writeTaskDir(t, repoDir, "task-a")
	taskB := writeTaskDir(t, repoDir, "task-b")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: root
spec:
  entries:
    task-a:
      ref:
        path: ` + filepath.Join(taskA, "task.yaml") + `
    task-b:
      ref:
        path: ` + filepath.Join(taskB, "task.yaml") + `
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)
	r := newResolver(t)
	results, _, err := r.Resolve(context.Background(), "ns", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2, got %d", len(results))
	}
}

func TestResolver_DevModeSubstitution(t *testing.T) {
	// When devMode is true and a ref has a DevRef, the DevRef is used.
	repoDir := t.TempDir()
	devDir := t.TempDir()

	// "remote" task has cron 0 8 * * *, dev task has cron 0 1 * * *
	writeTaskDir(t, repoDir, "deploy", "0 8 * * *")
	writeTaskDir(t, devDir, "deploy", "0 1 * * *") // dev version

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    deploy:
      ref:
        path: ` + filepath.Join(repoDir, "deploy", "task.yaml") + `
        dev_ref:
          path: ` + filepath.Join(devDir, "deploy", "task.yaml") + `
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)

	// dev mode OFF — should use remote (0 8)
	r := NewResolver(t.TempDir(), false, zap.NewNop())
	results, _, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if rtSpec(results[0]).Trigger.Cron != "0 8 * * *" {
		t.Errorf("dev mode off: got %q", rtSpec(results[0]).Trigger.Cron)
	}

	// dev mode ON — should use dev ref (0 1)
	rDev := NewResolver(t.TempDir(), true, zap.NewNop())
	resultsDev, _, err := rDev.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if rtSpec(resultsDev[0]).Trigger.Cron != "0 1 * * *" {
		t.Errorf("dev mode on: got %q", rtSpec(resultsDev[0]).Trigger.Cron)
	}
}

func TestResolver_InlineTask(t *testing.T) {
	repoDir := t.TempDir()

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    health-check:
      inline:
        name: Health Check
        runtime: deno
        trigger:
          manual: true
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)
	r := newResolver(t)
	results, _, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1, got %d", len(results))
	}
	if results[0].ID != "infra/health-check" {
		t.Errorf("ID: %q", results[0].ID)
	}
	if !rtSpec(results[0]).Trigger.Manual {
		t.Error("trigger.manual should be true")
	}
}

// An inline taskset entry's own base spec must get the same ${VAR}
// expansion a ref-loaded task.yaml gets from LoadDirWithVars. Regression
// guard for #726: only the override layers stacked on top of entry.Inline
// were expanded, leaving literal ${TASK_SET_DIR}/${VAR} tokens in the
// inline base spec's own permissions.fs[].path and params[].default.
func TestResolver_InlineEntry_ExpandsVar(t *testing.T) {
	repoDir := t.TempDir()

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    health-check:
      inline:
        name: Health Check
        runtime: deno
        trigger:
          manual: true
        params:
          shared_dir:
            type: string
            default: "${TASK_SET_DIR}/shared"
            description: ""
        permissions:
          fs:
            - path: "${TASK_SET_DIR}/pool"
              permission: rw
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)
	wantDir := filepath.Dir(tsPath)

	r := newResolver(t)
	results, failures, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}

	spec := rtSpec(results[0])
	if len(spec.Permissions.FS) != 1 {
		t.Fatalf("want 1 fs entry, got %d", len(spec.Permissions.FS))
	}
	if got, want := spec.Permissions.FS[0].Path, wantDir+"/pool"; got != want {
		t.Errorf("fs.path: got %q, want %q (literal ${TASK_SET_DIR} survived expansion)", got, want)
	}

	var sharedDefault string
	for _, p := range spec.Params {
		if p.Name == "shared_dir" {
			sharedDefault = p.Default
			break
		}
	}
	if got, want := sharedDefault, wantDir+"/shared"; got != want {
		t.Errorf("params[shared_dir].default: got %q, want %q (literal ${TASK_SET_DIR} survived expansion)", got, want)
	}
}

// TestResolver_InlineEntry_WebhookAuthDowngrade proves an inline taskset
// entry gets the same auth: any → session downgrade LoadDirWithVars applies
// to a ref-loaded task.yaml, when its webhook_secret names a template var
// that never resolves (env unset, not in the resolve-time vars map). Without
// ExpandSpec running normalizeWebhookAuth, the literal "${WEBHOOK_SECRET_NOT_SET}"
// string would be served as the HMAC key for a relay-reachable, unauthenticated
// webhook — see pkg/task/webhook_auth_normalize_test.go for the ref-loaded
// mirror of this test.
func TestResolver_InlineEntry_WebhookAuthDowngrade(t *testing.T) {
	os.Unsetenv("WEBHOOK_SECRET_NOT_SET")

	repoDir := t.TempDir()
	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    ai-hook:
      inline:
        name: ai-hook
        runtime: deno
        trigger:
          webhook: /hooks/x
          auth: any
          webhook_secret: "${WEBHOOK_SECRET_NOT_SET}"
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)

	r := newResolver(t)
	results, failures, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}

	spec := rtSpec(results[0])
	if spec.Trigger.WebhookAuth != task.WebhookAuthSession {
		t.Errorf("Trigger.WebhookAuth = %q, want %q (downgraded from unresolved auth: any)",
			spec.Trigger.WebhookAuth, task.WebhookAuthSession)
	}
	if spec.Trigger.WebhookSecret != "" {
		t.Errorf("Trigger.WebhookSecret = %q, want \"\" (placeholder cleared)", spec.Trigger.WebhookSecret)
	}
}

// ── mergeOverrides ────────────────────────────────────────────────────────────

func TestMergeOverrides_BNil(t *testing.T) {
	a := &Overrides{Timeout: 10 * time.Second}
	got := mergeOverrides(a, nil)
	if got.Timeout != 10*time.Second {
		t.Errorf("got %v", got.Timeout)
	}
}

func TestMergeOverrides_ANil(t *testing.T) {
	b := &Overrides{Timeout: 20 * time.Second}
	got := mergeOverrides(nil, b)
	if got.Timeout != 20*time.Second {
		t.Errorf("got %v", got.Timeout)
	}
}

func TestMergeOverrides_BWins(t *testing.T) {
	a := &Overrides{Timeout: 10 * time.Second}
	b := &Overrides{Timeout: 20 * time.Second}
	got := mergeOverrides(a, b)
	if got.Timeout != 20*time.Second {
		t.Errorf("b should win: got %v", got.Timeout)
	}
}

func TestMergeOverrides_EntriesMerged(t *testing.T) {
	a := &Overrides{Entries: map[string]*Overrides{"x": {Timeout: 5 * time.Second}}}
	b := &Overrides{Entries: map[string]*Overrides{"y": {Timeout: 10 * time.Second}}}
	got := mergeOverrides(a, b)
	if got.Entries["x"] == nil {
		t.Error("x from a missing")
	}
	if got.Entries["y"] == nil {
		t.Error("y from b missing")
	}
}

// Resolver.Resolve injects TASK_SET_DIR from the resolved root taskset
// path, regardless of whether the source loader supplied extraVars.
// Regression guard for the git-source bug where TASK_SET_DIR was only
// injected for local sources, leaving literal ${TASK_SET_DIR} in every
// task.yaml resolved from a git clone.
func TestResolver_InjectsTaskSetDirFromRoot(t *testing.T) {
	repoDir := t.TempDir()
	taskDir := filepath.Join(repoDir, "fstask")
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		t.Fatal(err)
	}
	// task.yaml references ${TASK_SET_DIR} in both an fs permission and a
	// param default — the two fields that expandSpec actually touches.
	taskYAML := `kind: Task
apiVersion: dicode/v1
name: fstask
runtime: deno
trigger:
  manual: true
params:
  shared_dir:
    type: string
    default: "${TASK_SET_DIR}/shared"
    description: ""
permissions:
  fs:
    - path: "${TASK_SET_DIR}/pool"
      permission: r
`
	writeFile(t, taskDir, "task.yaml", taskYAML)
	writeFile(t, taskDir, "task.js", "// task")

	tsContent := `kind: TaskSet
apiVersion: dicode/v1
metadata:
  name: infra
spec:
  entries:
    fstask:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)
	wantDir := filepath.Dir(tsPath)

	r := newResolver(t)
	// Pass nil extraVars — the resolver itself must derive TASK_SET_DIR.
	results, _, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}

	spec := rtSpec(results[0])
	if len(spec.Permissions.FS) != 1 {
		t.Fatalf("want 1 fs entry, got %d", len(spec.Permissions.FS))
	}
	if got, want := spec.Permissions.FS[0].Path, wantDir+"/pool"; got != want {
		t.Errorf("fs.path: got %q, want %q (literal ${TASK_SET_DIR} survived expansion)", got, want)
	}

	// Find the shared_dir param and assert its default was expanded.
	var sharedDefault string
	for _, p := range spec.Params {
		if p.Name == "shared_dir" {
			sharedDefault = p.Default
			break
		}
	}
	if got, want := sharedDefault, wantDir+"/shared"; got != want {
		t.Errorf("params[shared_dir].default: got %q, want %q", got, want)
	}
}

// Caller-supplied extraVars override the resolver's TASK_SET_DIR
// derivation. Useful for tests or for future source types that want to
// override the "root taskset dir" convention.
func TestResolver_CallerExtraVarsOverrideTaskSetDir(t *testing.T) {
	repoDir := t.TempDir()
	taskDir := filepath.Join(repoDir, "fstask")
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		t.Fatal(err)
	}
	taskYAML := `kind: Task
apiVersion: dicode/v1
name: fstask
runtime: deno
trigger:
  manual: true
permissions:
  fs:
    - path: "${TASK_SET_DIR}/pool"
      permission: r
`
	writeFile(t, taskDir, "task.yaml", taskYAML)
	writeFile(t, taskDir, "task.js", "// task")

	tsContent := `kind: TaskSet
apiVersion: dicode/v1
metadata:
  name: infra
spec:
  entries:
    fstask:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)

	r := newResolver(t)
	caller := map[string]string{task.VarTaskSetDir: "/caller/wins"}
	results, _, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, caller)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if got, want := rtSpec(results[0]).Permissions.FS[0].Path, "/caller/wins/pool"; got != want {
		t.Errorf("fs.path: got %q, want %q — caller extraVars should override resolver derivation", got, want)
	}
}

// ── Item 6: root-level spec.entries override cascade ─────────────────────────

// TestResolver_RootSpecEntryOverrideCascades verifies that an override applied
// at the dicode.yaml spec.entries level (simulated via parentOverrides) propagates
// into the inner TaskSet at the highest precedence. This mirrors the real
// dicode.yaml spec.entries.<name>.overrides.entries.<inner> mechanism.
//
// Setup: inner TaskSet has entry "deploy" with timeout 30s. A parent override
// sets timeout 5m. The resolved task should have timeout 5m.
func TestResolver_RootSpecEntryOverrideCascades(t *testing.T) {
	repoDir := t.TempDir()
	taskDir := writeTaskDir(t, repoDir, "deploy")

	// Inner taskset with a short default timeout for the entry.
	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: buildin
spec:
  entries:
    deploy:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)

	r := newResolver(t)
	// Parent override: spec.entries["buildin"].overrides.entries["deploy"].timeout = 5m
	// This is the highest-precedence layer — it should win over anything in the taskset.
	parentOverrides := &Overrides{
		Entries: map[string]*Overrides{
			"deploy": {
				Timeout: 5 * time.Minute,
			},
		},
	}
	results, _, err := r.Resolve(context.Background(), "buildin", &Ref{Path: tsPath}, nil, parentOverrides, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if rtSpec(results[0]).Timeout != 5*time.Minute {
		t.Errorf("timeout = %v, want 5m (root spec.entries override should cascade)", rtSpec(results[0]).Timeout)
	}
}

// TestResolver_RootSpecEntryDisablesInnerTask verifies that
// spec.entries.<name>.overrides.entries.<inner>.enabled=false applied at the
// dicode.yaml level (via parentOverrides) marks the inner task Enabled=false.
func TestResolver_RootSpecEntryDisablesInnerTask(t *testing.T) {
	repoDir := t.TempDir()
	taskDir := writeTaskDir(t, repoDir, "relay-client")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: buildin
spec:
  entries:
    relay-client:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)

	r := newResolver(t)
	// Operator disables relay-client at the dicode.yaml level:
	//   spec.entries.buildin.overrides.entries.relay-client.enabled: false
	parentOverrides := &Overrides{
		Entries: map[string]*Overrides{
			"relay-client": {Enabled: boolPtr(false)},
		},
	}
	results, _, err := r.Resolve(context.Background(), "buildin", &Ref{Path: tsPath}, nil, parentOverrides, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result (Enabled=false), got %d", len(results))
	}
	if rtSpec(results[0]).Enabled {
		t.Errorf("relay-client should be Enabled=false via root spec.entries override")
	}
}

// TestResolver_EnabledDefaultsTrue confirms that tasks without any enabled
// override have Spec.Enabled == true after resolution.
func TestResolver_EnabledDefaultsTrue(t *testing.T) {
	repoDir := t.TempDir()
	taskDir := writeTaskDir(t, repoDir, "hello")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: examples
spec:
  entries:
    hello:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)
	r := newResolver(t)
	results, _, err := r.Resolve(context.Background(), "examples", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if !rtSpec(results[0]).Enabled {
		t.Errorf("task without enabled override should default to Enabled=true")
	}
}

// ── expandRefPath ────────────────────────────────────────────────────────────

func TestExpandRefPath_NoVars(t *testing.T) {
	// Path without variables is returned unchanged.
	got := expandRefPath("./deploy/task.yaml", nil)
	if got != "./deploy/task.yaml" {
		t.Errorf("got %q, want unchanged", got)
	}
}

func TestExpandRefPath_RepoDir(t *testing.T) {
	vars := map[string]string{VarRepoDir: "/home/repo", VarTaskSetRefDir: "/home/repo/sets"}
	got := expandRefPath("${REPO_DIR}/tasks/deploy", vars)
	if got != "/home/repo/tasks/deploy" {
		t.Errorf("got %q", got)
	}
}

func TestExpandRefPath_TaskSetDir(t *testing.T) {
	vars := map[string]string{VarRepoDir: "/home/repo", VarTaskSetRefDir: "/home/repo/sets"}
	got := expandRefPath("${TASKSET_DIR}/../shared", vars)
	if got != "/home/repo/sets/../shared" {
		t.Errorf("got %q", got)
	}
}

func TestExpandRefPath_BothVars(t *testing.T) {
	vars := map[string]string{VarRepoDir: "/root", VarTaskSetRefDir: "/root/sub"}
	got := expandRefPath("${REPO_DIR}/a/${TASKSET_DIR}/b", vars)
	if got != "/root/a//root/sub/b" {
		t.Errorf("got %q", got)
	}
}

func TestExpandRefPath_UnknownVar(t *testing.T) {
	vars := map[string]string{VarRepoDir: "/root"}
	got := expandRefPath("${UNKNOWN}/foo", vars)
	if got != "${UNKNOWN}/foo" {
		t.Errorf("unknown var should be left as-is: got %q", got)
	}
}

// ── Resolver ref path templating integration ─────────────────────────────────

func TestResolver_RefPathRepoDirExpansion(t *testing.T) {
	// ${REPO_DIR} in a ref path expands to the root taskset.yaml directory.
	repoDir := t.TempDir()
	taskDir := writeTaskDir(t, repoDir, "deploy")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    deploy:
      ref:
        path: ${REPO_DIR}/deploy
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)

	r := newResolver(t)
	results, _, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].ID != "infra/deploy" {
		t.Errorf("ID: got %q", results[0].ID)
	}
	// TaskDir should point at the actual task directory.
	if results[0].TaskDir != taskDir {
		t.Errorf("TaskDir: got %q, want %q", results[0].TaskDir, taskDir)
	}
}

func TestResolver_RefPathTaskSetDirExpansion(t *testing.T) {
	// ${TASKSET_DIR} in a ref path expands to the directory of the current
	// taskset.yaml, which for the root taskset is the same as REPO_DIR.
	repoDir := t.TempDir()
	taskDir := writeTaskDir(t, repoDir, "deploy")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    deploy:
      ref:
        path: ${TASKSET_DIR}/deploy
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)

	r := newResolver(t)
	results, _, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].TaskDir != taskDir {
		t.Errorf("TaskDir: got %q, want %q", results[0].TaskDir, taskDir)
	}
}

func TestResolver_RefPathWithoutVarsUnchanged(t *testing.T) {
	// Regression: ref paths without template variables must still work.
	repoDir := t.TempDir()
	taskDir := writeTaskDir(t, repoDir, "deploy")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    deploy:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)
	r := newResolver(t)
	results, _, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].ID != "infra/deploy" {
		t.Errorf("ID: got %q", results[0].ID)
	}
}

// TestResolveRef_LocalEscapeRejectedInsideClone verifies that a local ref
// that would traverse above cloneRoot is rejected when cloneRoot is set.
func TestResolveRef_LocalEscapeRejectedInsideClone(t *testing.T) {
	r := newResolver(t)
	parent := filepath.Join(t.TempDir(), "clone", "taskset.yaml")
	cloneRoot := filepath.Dir(parent)
	if err := os.MkdirAll(cloneRoot, 0755); err != nil {
		t.Fatal(err)
	}

	ref := &Ref{Path: "../../etc/passwd"}
	_, _, err := r.resolveRef(context.Background(), ref, parent, nil, cloneRoot, true)
	if err == nil {
		t.Fatal("expected error for path escaping clone root, got nil")
	}
}

// TestResolveRef_LocalContainedPathAllowed verifies a path that stays inside cloneRoot.
func TestResolveRef_LocalContainedPathAllowed(t *testing.T) {
	r := newResolver(t)
	cloneDir := t.TempDir()
	tsFile := filepath.Join(cloneDir, "sub", "taskset.yaml")
	if err := os.MkdirAll(filepath.Dir(tsFile), 0755); err != nil {
		t.Fatal(err)
	}

	// A relative path that stays inside cloneDir is allowed.
	ref := &Ref{Path: "sibling/taskset.yaml"}
	got, _, err := r.resolveRef(context.Background(), ref, tsFile, nil, cloneDir, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// resolveYAMLPath returns the path unchanged if it doesn't exist; just check it starts with cloneDir.
	if !strings.HasPrefix(got, cloneDir) {
		t.Errorf("resolved path %q not under cloneDir %q", got, cloneDir)
	}
}

// TestResolveRef_SymlinkEscapeRejected verifies that a ref whose path traverses
// a directory symlink committed inside the clone is rejected even though the
// lexical path stays under cloneRoot — go-git materializes such symlinks as real
// on-disk links, so following them would escape the clone.
func TestResolveRef_SymlinkEscapeRejected(t *testing.T) {
	r := newResolver(t)
	cloneDir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "taskset.yaml"), []byte("kind: TaskSet\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// A directory symlink inside the clone pointing outside it.
	if err := os.Symlink(outside, filepath.Join(cloneDir, "evil")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	tsFile := filepath.Join(cloneDir, "taskset.yaml")

	ref := &Ref{Path: "evil/taskset.yaml"}
	if _, _, err := r.resolveRef(context.Background(), ref, tsFile, nil, cloneDir, true); err == nil {
		t.Fatal("expected error for ref traversing a symlink out of the clone, got nil")
	}
}

// writeInvalidTaskDir writes a task.yaml whose `hash_include` field (a
// []string) is a YAML bool instead — the exact class of typo #649 quotes
// from daemon.log ("cannot unmarshal !!bool true into []string") — and
// returns the task dir.
func writeInvalidTaskDir(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	yaml := "kind: Task\napiVersion: dicode/v1\nname: " + name + "\nruntime: deno\ntrigger:\n  manual: true\nhash_include: true\n"
	writeFile(t, dir, "task.yaml", yaml)
	writeFile(t, dir, "task.js", "// task")
	return dir
}

// TestResolver_FailedEntryReportedNotDropped is the pkg/taskset-level
// regression lock for #649: a task.yaml that fails to parse must come back
// as a ResolveFailure (so callers can surface it) rather than just vanishing
// from the results with nothing but a log line to show for it. A sibling
// entry that resolves fine must be unaffected.
func TestResolver_FailedEntryReportedNotDropped(t *testing.T) {
	repoDir := t.TempDir()
	goodDir := writeTaskDir(t, repoDir, "good")
	badDir := writeInvalidTaskDir(t, repoDir, "bad")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    good:
      ref:
        path: ` + filepath.Join(goodDir, "task.yaml") + `
    bad:
      ref:
        path: ` + filepath.Join(badDir, "task.yaml") + `
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)
	r := newResolver(t)
	results, failures, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}

	if len(results) != 1 || results[0].ID != "infra/good" {
		t.Fatalf("want exactly the good entry in results, got %+v", results)
	}

	if len(failures) != 1 {
		t.Fatalf("want exactly 1 failure, got %d: %+v", len(failures), failures)
	}
	f := failures[0]
	if f.ID != "infra/bad" {
		t.Errorf("failure ID = %q, want %q", f.ID, "infra/bad")
	}
	if f.Error == nil || !strings.Contains(f.Error.Error(), "unmarshal") {
		t.Errorf("failure Error = %v, want an unmarshal error", f.Error)
	}
}
