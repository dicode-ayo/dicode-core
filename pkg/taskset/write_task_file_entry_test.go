package taskset_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"go.uber.org/zap"
)

// resolveBuildin resolves the shipped buildin taskset against a fixed data
// dir, so assertions can read the same expanded specs the daemon registers.
func resolveBuildin(t *testing.T, dataDir string) map[string]*task.Spec {
	t.Helper()
	r := taskset.NewResolver(dataDir, false, zap.NewNop())
	resolved, failures, err := r.Resolve(context.Background(), "buildin",
		&taskset.Ref{Path: "../../tasks/buildin/taskset.yaml"}, nil, nil, nil)
	if err != nil {
		t.Fatalf("resolve buildin taskset: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("resolve failures: %v", failures)
	}
	out := map[string]*task.Spec{}
	for _, rt := range resolved {
		if spec, ok := rt.Kinded.(*task.Spec); ok {
			out[rt.ID] = spec
		}
	}
	return out
}

// The authoring agent's only route to disk is a task it can call as a tool.
// A write task that isn't listed in spec.entries never reaches list_tasks, so
// it is neither offered to the model nor callable via run_task (#734).
func TestWriteTaskFileEntry_IsRegisteredWithScopedGrants(t *testing.T) {
	specs := resolveBuildin(t, "/data")

	spec, ok := specs["buildin/write-task-file"]
	if !ok {
		t.Fatalf("buildin/write-task-file not resolved; got %d tasks", len(specs))
	}

	// ${DATADIR}/ai-tasks is the ai-scratch source config.go synthesises, so
	// it is where `dicode task create` scaffolds. Every other root is a
	// widening: a directory the daemon resolves taskset files from hands the
	// caller the daemon's git credentials through a ref's auth.token_env.
	if len(spec.Permissions.FS) != 1 {
		t.Fatalf("write-task-file fs grants = %+v, want exactly ${DATADIR}/ai-tasks", spec.Permissions.FS)
	}
	if g := spec.Permissions.FS[0]; g.Path != "/data/ai-tasks" || g.Permission != "rw" {
		t.Errorf("write-task-file fs grant = %+v, want /data/ai-tasks rw", g)
	}

	// The grant is the outer boundary; the task's own path check is the inner
	// one, and it needs the roots to enforce anything.
	roots := ""
	for _, p := range spec.Params {
		if p.Name == "roots" {
			roots = p.Default
		}
	}
	if roots != "/data/ai-tasks" {
		t.Errorf("write-task-file roots default = %q, want /data/ai-tasks", roots)
	}

	if spec.Description == "" {
		t.Error("write-task-file needs a description — it is what the model sees as the tool's docs")
	}
}

// A tool the agent may call must also be in its dicode.tasks allowlist by the
// id run_task is invoked with, which is the namespaced one. The ai-agent base
// these entries override grants "*", so this pins the declared intent rather
// than the only thing permitting the call.
func TestAuthoringAgents_AllowWriteTaskFile(t *testing.T) {
	specs := resolveBuildin(t, "/data")

	for _, id := range []string{"buildin/task-create"} {
		spec, ok := specs[id]
		if !ok {
			t.Errorf("%s not resolved", id)
			continue
		}
		if spec.Permissions.Dicode == nil {
			t.Errorf("%s has no dicode permissions", id)
			continue
		}
		if !slices.Contains(spec.Permissions.Dicode.Tasks, "buildin/write-task-file") {
			t.Errorf("%s dicode.tasks missing buildin/write-task-file; got %v", id, spec.Permissions.Dicode.Tasks)
		}
	}
}

// The system prompt is the only place the model learns how to get files onto
// disk; a prompt describing capabilities it has no tool for costs a paid model
// call and produces nothing.
func TestTaskCreateEntry_PromptNamesTheWriteTool(t *testing.T) {
	spec, ok := resolveBuildin(t, "/data")["buildin/task-create"]
	if !ok {
		t.Fatal("buildin/task-create not resolved")
	}
	prompt := ""
	for _, p := range spec.Params {
		if p.Name == "system_prompt" {
			prompt = p.Default
		}
	}
	if !strings.Contains(prompt, "write-task-file") {
		t.Errorf("task-create system_prompt never names the write tool:\n%s", prompt)
	}
}
