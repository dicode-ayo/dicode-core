package taskset_test

import (
	"testing"

	"github.com/dicode/dicode/pkg/taskset"
)

func TestAutoFixEntry_HasExpectedDicodePerms(t *testing.T) {
	// Load the buildin taskset and inspect the "auto-fix" entry.
	// The API is LoadTaskSet which returns *TaskSetSpec; we then inspect
	// entry.Overrides.Dicode directly (the resolved merge would require
	// loading the ai-agent task.yaml, which needs a real data dir).
	ts, err := taskset.LoadTaskSet("../../tasks/buildin/taskset.yaml")
	if err != nil {
		t.Fatalf("load taskset: %v", err)
	}

	entry, ok := ts.Spec.Entries["auto-fix"]
	if !ok {
		t.Fatal("auto-fix entry not found in buildin taskset")
	}
	if entry.Overrides == nil {
		t.Fatal("auto-fix entry has no overrides")
	}
	d := entry.Overrides.Dicode
	if d == nil {
		t.Fatal("auto-fix overrides.dicode is nil")
	}

	want := map[string]bool{
		"RunsReplay":        d.RunsReplay,
		"RunsGetInput":      d.RunsGetInput,
		"RunsPinInput":      d.RunsPinInput,
		"RunsUnpinInput":    d.RunsUnpinInput,
		"SourcesSetDevMode": d.SourcesSetDevMode,
		"TasksTest":         d.TasksTest,
		"GitCommitPush":     d.GitCommitPush,
		"ListTasks":         d.ListTasks,
		"GetRuns":           d.GetRuns,
	}
	for name, v := range want {
		if !v {
			t.Errorf("%s expected true in auto-fix overrides.dicode", name)
		}
	}

	// Tasks union must include git-pr, named the way taskAllowed actually
	// compares it: the namespaced id ("buildin/git-pr"), not the bare entry
	// name. A bare "git-pr" can never match the id dicode.run_task is called
	// with, so it silently relies on the base ai-agent's "*" wildcard instead
	// of the restriction this list is meant to express (#742).
	hasGitPR := false
	for _, taskID := range d.Tasks {
		if taskID == "git-pr" {
			t.Errorf(`auto-fix tasks slice has bare "git-pr", which taskAllowed can never match against the namespaced call id; want "buildin/git-pr"`)
		}
		if taskID == "buildin/git-pr" {
			hasGitPR = true
		}
	}
	if !hasGitPR {
		t.Errorf("auto-fix tasks slice missing buildin/git-pr; got %v", d.Tasks)
	}
}
