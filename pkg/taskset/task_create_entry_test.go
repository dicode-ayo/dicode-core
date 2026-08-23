package taskset_test

import (
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
)

// TestTaskCreateEntry_HasExpectedDicodePerms pins task-create-turn's granted
// capabilities against the real taskset.yaml rather than its doc comment.
// Every capability listed here becomes a tool the model can call, so the
// absent ones are as much a part of the contract as the present ones:
// scaffolding a new task has no prior run to read, pin, unpin or replay.
func TestTaskCreateEntry_HasExpectedDicodePerms(t *testing.T) {
	ts, err := taskset.LoadTaskSet("../../tasks/buildin/taskset.yaml")
	if err != nil {
		t.Fatalf("load taskset: %v", err)
	}

	entry, ok := ts.Spec.Entries["task-create-turn"]
	if !ok {
		t.Fatal("task-create-turn entry not found in buildin taskset")
	}
	if entry.Overrides == nil {
		t.Fatal("task-create-turn entry has no overrides")
	}
	d := entry.Overrides.Dicode
	if d == nil {
		t.Fatal("task-create-turn overrides.dicode is nil")
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
	entry, ok := ts.Spec.Entries["task-create-turn"]
	if !ok {
		t.Fatal("task-create-turn entry not found in buildin taskset")
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
	entry, ok := ts.Spec.Entries["task-create-turn"]
	if !ok {
		t.Fatal("task-create-turn entry not found in buildin taskset")
	}
	if entry.Overrides == nil {
		t.Fatal("task-create-turn entry has no overrides")
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

// TestTaskCreatePipeline_VerifiesWhatTheTurnWrote pins the structure the #755
// post-condition depends on. The check is not in the control plane — the
// control plane fires whatever ai.create_task names — so it exists only for as
// long as buildin/task-create stays a pipeline whose LAST stage reads the disk.
// Collapsing it back to a bare agent entry silently restores the bug where a
// turn that wrote nothing settles green, so that has to be a deliberate act
// that fails this test, not an edit nobody notices.
func TestTaskCreatePipeline_VerifiesWhatTheTurnWrote(t *testing.T) {
	p, err := task.LoadPipelineDir("../../tasks/buildin/task-create", nil)
	if err != nil {
		t.Fatalf("buildin/task-create must be a kind: PipelineTask: %v", err)
	}
	if len(p.Stages) < 2 {
		t.Fatalf("stages = %d, want the agent turn plus a verification stage", len(p.Stages))
	}
	last := p.Stages[len(p.Stages)-1]
	if last.Task != "buildin/verify-task-written" {
		t.Errorf("terminal stage = %q, want buildin/verify-task-written — a failing terminal stage is what fails the run", last.Task)
	}
	if p.Stages[0].Task != "buildin/task-create-turn" {
		t.Errorf("first stage = %q, want buildin/task-create-turn", p.Stages[0].Task)
	}

	// Stages receive no params of their own and ${input.params.…} resolves
	// only on the first stage, so the turn's inputs have to be threaded there
	// explicitly or the agent runs with bare defaults and no prompt.
	first := p.Stages[0]
	if first.Overrides == nil {
		t.Fatal("first stage must thread the fire params through overrides")
	}
	threaded := map[string]string{}
	for _, po := range first.Overrides.Params {
		threaded[po.Name] = po.Default
	}
	for _, name := range []string{"prompt", "session_id", "task_dir"} {
		want := "${input.params." + name + "}"
		if threaded[name] != want {
			t.Errorf("first stage param %q = %q, want %q", name, threaded[name], want)
		}
	}

	// The verification stage learns the directory from the turn's return
	// value; without this it would check nothing and pass everything.
	var verifyDir string
	if last.Overrides != nil {
		for _, po := range last.Overrides.Params {
			if po.Name == "task_dir" {
				verifyDir = po.Default
			}
		}
	}
	if verifyDir != "${input.output.task_dir}" {
		t.Errorf("verify stage task_dir = %q, want ${input.output.task_dir}", verifyDir)
	}
}
