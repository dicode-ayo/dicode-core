package taskset_test

import (
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/taskset"
)

// TestTaskCreateEntry_HasExpectedDicodePerms pins task-create's granted
// capabilities against the real taskset.yaml rather than its doc comment.
// Every capability listed here becomes a tool the model can call, so the
// absent ones are as much a part of the contract as the present ones:
// scaffolding a new task has no prior run to read, pin, unpin or replay.
func TestTaskCreateEntry_HasExpectedDicodePerms(t *testing.T) {
	ts, err := taskset.LoadTaskSet("../../tasks/buildin/taskset.yaml")
	if err != nil {
		t.Fatalf("load taskset: %v", err)
	}

	entry, ok := ts.Spec.Entries["task-create"]
	if !ok {
		t.Fatal("task-create entry not found in buildin taskset")
	}
	if entry.Overrides == nil {
		t.Fatal("task-create entry has no overrides")
	}
	d := entry.Overrides.Dicode
	if d == nil {
		t.Fatal("task-create overrides.dicode is nil")
	}

	want := map[string]bool{
		"SourcesList":       d.SourcesList,
		"SourcesSetDevMode": d.SourcesSetDevMode,
		"TasksTest":         d.TasksTest,
		"GitCommitPush":     d.GitCommitPush,
		"ListTasks":         d.ListTasks,
		"GetRuns":           d.GetRuns,
	}
	for name, v := range want {
		if !v {
			t.Errorf("%s expected true in task-create overrides.dicode", name)
		}
	}

	withheld := map[string]bool{
		"RunsReplay":     d.RunsReplay,
		"RunsGetInput":   d.RunsGetInput,
		"RunsPinInput":   d.RunsPinInput,
		"RunsUnpinInput": d.RunsUnpinInput,
	}
	for name, v := range withheld {
		if v {
			t.Errorf("%s must stay false: task-create has no prior run to act on", name)
		}
	}

	// Tasks union must include git-pr: the authoring loop lands through it.
	hasGitPR := false
	for _, taskID := range d.Tasks {
		if taskID == "git-pr" {
			hasGitPR = true
		}
	}
	if !hasGitPR {
		t.Errorf("task-create tasks slice missing git-pr; got %v", d.Tasks)
	}

	// dev-clones fs grant: the scratch clone the write -> test loop works in.
	hasDevClonesRW := false
	for _, fs := range entry.Overrides.Fs {
		if strings.Contains(fs.Path, "dev-clones") && fs.Permission == "rw" {
			hasDevClonesRW = true
		}
	}
	if !hasDevClonesRW {
		t.Errorf("task-create fs grants missing rw dev-clones entry; got %+v", entry.Overrides.Fs)
	}
}

// TestTaskCreateEntry_TriggerIsManual pins the design decision that
// task-create is fired via FireManual from the control socket — never a
// webhook — since the authoring session (not an HTTP caller) owns dispatch.
func TestTaskCreateEntry_TriggerIsManual(t *testing.T) {
	ts, err := taskset.LoadTaskSet("../../tasks/buildin/taskset.yaml")
	if err != nil {
		t.Fatalf("load taskset: %v", err)
	}
	entry, ok := ts.Spec.Entries["task-create"]
	if !ok {
		t.Fatal("task-create entry not found in buildin taskset")
	}
	if entry.Overrides == nil || entry.Overrides.Trigger == nil {
		t.Fatal("task-create entry has no trigger override")
	}
	if entry.Overrides.Trigger.Manual == nil || !*entry.Overrides.Trigger.Manual {
		t.Errorf("task-create trigger.manual = %v, want explicit true", entry.Overrides.Trigger.Manual)
	}
	if entry.Overrides.Trigger.Webhook != nil && *entry.Overrides.Trigger.Webhook != "" {
		t.Errorf("task-create trigger.webhook = %q, want empty (manual-only trigger)", *entry.Overrides.Trigger.Webhook)
	}
}

// TestTaskCreateEntry_ProviderDefaultsLikeDicodai pins the design decision
// that task-create, unlike auto-fix (no provider defaults), defaults like
// dicodai to OpenAI's gpt-4o so `dicode task create --ai` works zero-config
// with just OPENAI_API_KEY set.
func TestTaskCreateEntry_ProviderDefaultsLikeDicodai(t *testing.T) {
	ts, err := taskset.LoadTaskSet("../../tasks/buildin/taskset.yaml")
	if err != nil {
		t.Fatalf("load taskset: %v", err)
	}
	entry, ok := ts.Spec.Entries["task-create"]
	if !ok {
		t.Fatal("task-create entry not found in buildin taskset")
	}
	if entry.Overrides == nil {
		t.Fatal("task-create entry has no overrides")
	}

	params := map[string]string{}
	for _, p := range entry.Overrides.Params {
		params[p.Name] = p.Default
	}
	wantParams := map[string]string{
		"model":       "gpt-4o",
		"base_url":    "https://api.openai.com/v1",
		"api_key_env": "OPENAI_API_KEY",
		"skills":      "dicode-task-dev,dicode-basics",
	}
	for name, want := range wantParams {
		if got := params[name]; got != want {
			t.Errorf("task-create param %q default = %q, want %q", name, got, want)
		}
	}

	hasOpenAIEnv := false
	for _, e := range entry.Overrides.Env {
		if e.Name == "OPENAI_API_KEY" {
			hasOpenAIEnv = true
		}
	}
	if !hasOpenAIEnv {
		t.Errorf("task-create env grants missing OPENAI_API_KEY; got %+v", entry.Overrides.Env)
	}

	hasOpenAINet := false
	for _, n := range entry.Overrides.Net {
		if n == "api.openai.com" {
			hasOpenAINet = true
		}
	}
	if !hasOpenAINet {
		t.Errorf("task-create net grants missing api.openai.com; got %v", entry.Overrides.Net)
	}
}
