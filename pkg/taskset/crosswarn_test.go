package taskset

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

func mustContain(t *testing.T, got, sub string) {
	t.Helper()
	if !strings.Contains(got, sub) {
		t.Errorf("%q does not contain %q", got, sub)
	}
}

func mustLen(t *testing.T, got []CrossTaskWarning, n int) {
	t.Helper()
	if len(got) != n {
		t.Fatalf("got %d warnings, want %d: %+v", len(got), n, got)
	}
}

func specRT(id string, params task.Params, ofc *task.OnFailureChainSpec) *ResolvedTask {
	return &ResolvedTask{ID: id, Kinded: &task.Spec{Name: id, Params: params, OnFailureChain: ofc}}
}

func pipelineRT(id, cron string, stages ...task.Stage) *ResolvedTask {
	return &ResolvedTask{ID: id, Kinded: &task.PipelineTask{
		Name: id, Trigger: task.PipelineTrigger{Cron: cron, Manual: cron == ""}, Stages: stages,
	}}
}

var requiredTitle = task.Params{{Name: "title", Required: true}}

func TestCrossTaskParamWarnings_CronPipelineUnsatisfiedStage(t *testing.T) {
	got := CrossTaskParamWarnings([]*ResolvedTask{
		specRT("ns/notify", requiredTitle, nil),
		pipelineRT("ns/pipe", "0 * * * *", task.Stage{Task: "ns/notify"}),
	}, task.OnFailureChainSpec{})
	mustLen(t, got, 1)
	if got[0].TaskID != "ns/pipe" {
		t.Errorf("TaskID = %q", got[0].TaskID)
	}
	mustContain(t, got[0].Message, "params.title")
	mustContain(t, got[0].Message, "ns/notify")
}

func TestCrossTaskParamWarnings_StageOverridesSatisfy(t *testing.T) {
	got := CrossTaskParamWarnings([]*ResolvedTask{
		specRT("ns/notify", requiredTitle, nil),
		pipelineRT("ns/pipe", "0 * * * *", task.Stage{
			Task:      "ns/notify",
			Overrides: &task.Overrides{Params: task.ParamOverrides{{Name: "title", Default: "hi"}}},
		}),
	}, task.OnFailureChainSpec{})
	mustLen(t, got, 0)
}

func TestCrossTaskParamWarnings_NonCronPipelineNoWarning(t *testing.T) {
	got := CrossTaskParamWarnings([]*ResolvedTask{
		specRT("ns/notify", requiredTitle, nil),
		pipelineRT("ns/pipe", "", task.Stage{Task: "ns/notify"}),
	}, task.OnFailureChainSpec{})
	mustLen(t, got, 0)
}

func TestCrossTaskParamWarnings_PerTaskFailureChainTarget(t *testing.T) {
	got := CrossTaskParamWarnings([]*ResolvedTask{
		specRT("ns/handler", requiredTitle, nil),
		specRT("ns/job", nil, &task.OnFailureChainSpec{Task: "ns/handler"}),
	}, task.OnFailureChainSpec{})
	mustLen(t, got, 1)
	if got[0].TaskID != "ns/job" {
		t.Errorf("TaskID = %q", got[0].TaskID)
	}
	mustContain(t, got[0].Message, "on_failure_chain target")
	mustContain(t, got[0].Message, "params.title")
}

func TestCrossTaskParamWarnings_FailureChainTargetWithDefault(t *testing.T) {
	got := CrossTaskParamWarnings([]*ResolvedTask{
		specRT("ns/handler", task.Params{{Name: "title", Required: true, Default: "x"}}, nil),
		specRT("ns/job", nil, &task.OnFailureChainSpec{Task: "ns/handler"}),
	}, task.OnFailureChainSpec{Task: "ns/handler"})
	mustLen(t, got, 0)
}

func TestCrossTaskParamWarnings_DefaultsChainWarnsOnce(t *testing.T) {
	got := CrossTaskParamWarnings([]*ResolvedTask{
		specRT("ns/handler", requiredTitle, nil),
		specRT("ns/a", nil, nil),
		specRT("ns/b", nil, nil),
		specRT("ns/own", nil, &task.OnFailureChainSpec{Task: "ns/a"}),
	}, task.OnFailureChainSpec{Task: "ns/handler"})
	mustLen(t, got, 1)
	mustContain(t, got[0].Message, "defaults.on_failure_chain")
	mustContain(t, got[0].Message, "ns/handler")
}

func TestCrossTaskParamWarnings_UnknownTargetsSkipped(t *testing.T) {
	got := CrossTaskParamWarnings([]*ResolvedTask{
		specRT("ns/job", nil, &task.OnFailureChainSpec{Task: "other/missing"}),
		specRT("ns/job2", nil, &task.OnFailureChainSpec{}),
		pipelineRT("ns/pipe", "0 * * * *", task.Stage{Task: "other/missing"}),
	}, task.OnFailureChainSpec{Task: "other/gone"})
	mustLen(t, got, 0)
}

func TestCrossTaskParamWarnings_SelfTargetSkipped(t *testing.T) {
	got := CrossTaskParamWarnings([]*ResolvedTask{
		specRT("ns/handler", requiredTitle, &task.OnFailureChainSpec{Task: "ns/handler"}),
	}, task.OnFailureChainSpec{Task: "ns/handler"})
	mustLen(t, got, 0)
}

func TestSource_CrossTaskWarningLoggedOncePerAppearance(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"handler", "job"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, dir, "handler/task.yaml", "kind: Task\napiVersion: dicode/v1\nname: handler\nruntime: deno\n"+
		"trigger:\n  manual: true\nparams:\n  title:\n    required: true\n")
	writeFile(t, dir, "handler/task.js", "// task")
	writeFile(t, dir, "job/task.yaml", "kind: Task\napiVersion: dicode/v1\nname: job\nruntime: deno\n"+
		"trigger:\n  manual: true\n")
	writeFile(t, dir, "job/task.js", "// task")
	tsPath := writeFile(t, dir, "taskset.yaml", "apiVersion: dicode/v1\nkind: TaskSet\nmetadata:\n  name: t\nspec:\n  entries:\n    handler:\n      ref:\n        path: "+filepath.Join(dir, "handler", "task.yaml")+"\n    job:\n      ref:\n        path: "+filepath.Join(dir, "job", "task.yaml")+"\n")

	log, logs := newObservedLogger()
	s := NewSource(tsPath, "ns", &Ref{Path: tsPath}, "", t.TempDir(), false, 0, log,
		WithDefaultsOnFailureChain(task.OnFailureChainSpec{Task: "ns/handler"}))

	count := func() int {
		n := 0
		for _, e := range logs.All() {
			if e.Message == "taskset: cross-task config warning" &&
				strings.Contains(e.ContextMap()["warning"].(string), "params.title") {
				n++
			}
		}
		return n
	}
	for i := 0; i < 2; i++ {
		_, _, err := s.resolve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := count(); n != 1 {
		t.Errorf("warning logged %d times across two passes, want 1", n)
	}
}
