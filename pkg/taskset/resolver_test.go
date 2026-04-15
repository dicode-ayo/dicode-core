package taskset

import (
	"context"
	"os"
	"path/filepath"
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
	results, err := r.Resolve(context.Background(), "infra", rootRef, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].ID != "infra/deploy" {
		t.Errorf("ID: got %q", results[0].ID)
	}
	if results[0].Spec.Name != "deploy" {
		t.Errorf("spec.name: %q", results[0].Spec.Name)
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
	results, err := r.Resolve(context.Background(), "team", &Ref{Path: tsPath}, nil, nil, nil)
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
	results, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1")
	}
	spec := results[0].Spec
	if spec.Trigger.Cron != "0 2 * * *" {
		t.Errorf("cron: %q", spec.Trigger.Cron)
	}
	em := envMap(spec.Permissions.Env)
	if em["DEPLOY_TARGET"] != "prod" {
		t.Errorf("env not merged: %v", spec.Permissions.Env)
	}
}

func TestResolver_DisabledEntrySkipped(t *testing.T) {
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
	results, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("disabled task should not appear: %v", results)
	}
}

func TestResolver_ParentEntryPatchDisables(t *testing.T) {
	// Task is enabled in taskset.yaml but parent patches it to disabled.
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
	results, err := r.Resolve(context.Background(), "infra/backend", &Ref{Path: tsPath}, nil, parentOverrides, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("parent disabled task should not appear: got %d results", len(results))
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
	results, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1")
	}
	spec := results[0].Spec
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
	results, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, configDefaults, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1")
	}
	spec := results[0].Spec
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
	results, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if results[0].Spec.Timeout != 30*time.Second {
		t.Errorf("entry override should beat set defaults: got %v", results[0].Spec.Timeout)
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
	results, err := r.Resolve(context.Background(), "infra", &Ref{Path: rootPath}, nil, nil, nil)
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
	results, err := r.Resolve(context.Background(), "infra", &Ref{Path: rootPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1, got %d", len(results))
	}
	// Nested entry's own override (0 4 * * *) beats parent entry patch (0 3 * * *) — leaf wins.
	if results[0].Spec.Trigger.Cron != "0 4 * * *" {
		t.Errorf("leaf should win: got %q", results[0].Spec.Trigger.Cron)
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
	results, err := r.Resolve(context.Background(), "ns", &Ref{Path: tsPath}, nil, nil, nil)
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
	results, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if results[0].Spec.Trigger.Cron != "0 8 * * *" {
		t.Errorf("dev mode off: got %q", results[0].Spec.Trigger.Cron)
	}

	// dev mode ON — should use dev ref (0 1)
	rDev := NewResolver(t.TempDir(), true, zap.NewNop())
	resultsDev, err := rDev.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if resultsDev[0].Spec.Trigger.Cron != "0 1 * * *" {
		t.Errorf("dev mode on: got %q", resultsDev[0].Spec.Trigger.Cron)
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
	results, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1, got %d", len(results))
	}
	if results[0].ID != "infra/health-check" {
		t.Errorf("ID: %q", results[0].ID)
	}
	if !results[0].Spec.Trigger.Manual {
		t.Error("trigger.manual should be true")
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
	results, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}

	spec := results[0].Spec
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
	results, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, caller)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if got, want := results[0].Spec.Permissions.FS[0].Path, "/caller/wins/pool"; got != want {
		t.Errorf("fs.path: got %q, want %q — caller extraVars should override resolver derivation", got, want)
	}
}
