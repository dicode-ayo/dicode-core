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

	// Tasks union must include git-pr.
	hasGitPR := false
	for _, taskID := range d.Tasks {
		if taskID == "git-pr" {
			hasGitPR = true
		}
	}
	if !hasGitPR {
		t.Errorf("auto-fix tasks slice missing git-pr; got %v", d.Tasks)
	}
}
