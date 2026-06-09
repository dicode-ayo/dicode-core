package taskset

// Footgun pins for the override-machinery unification.
//
// Each test below pins one of three latent footguns in the override stack.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

// ── Footgun #1: resolver skips post-merge validation ─────────────────────────
//
// Survey §5.1: A/B (resolver) sites do NOT call merged.Validate() after
// applying override layers. A taskset entry override that flips Runtime to
// "docker" without supplying a Docker section produces an invalid Spec —
// today the resolver returns it anyway and the failure is only caught later
// at engine.Register, with a less specific error.
//
// After the fix, the resolver must reject the invalid merged spec (logged
// + skipped, mirroring how it already handles failed LoadDirWithVars).
func TestFootgun_ResolverValidatesAfterMerge(t *testing.T) {
	repoDir := t.TempDir()
	taskDir := writeTaskDir(t, repoDir, "deploy")

	// Override flips runtime to docker on a task with no docker: section,
	// producing a merged spec that fails Spec.validate() ("runtime docker
	// requires a docker: section in task.yaml").
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
        runtime: docker
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)

	log, logs := newObservedLogger()
	r := newResolver(t)
	r.log = log

	results, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Resolve top-level error: %v", err)
	}

	// The malformed entry should be skipped (warn-and-continue), exactly
	// like a LoadDirWithVars failure today. The result list must NOT
	// contain a spec whose Runtime=docker and Docker=nil.
	for _, rt := range results {
		if rtSpec(rt).Runtime == task.RuntimeDocker && rtSpec(rt).Docker == nil {
			t.Fatalf("resolver returned a merged spec that fails Validate: id=%s runtime=%q docker=nil",
				rt.ID, rtSpec(rt).Runtime)
		}
	}

	// And the warning must surface so operators can find the bad override.
	var sawWarn bool
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "override") || strings.Contains(e.Message, "merged spec") || strings.Contains(e.Message, "validate") {
			sawWarn = true
			break
		}
	}
	if !sawWarn {
		t.Errorf("expected a warn log for invalid merged spec; got %v", logs.All())
	}
}

// ── Footgun #2: copySpec doesn't deep-clone OnFailureChain / RunInputs
// (survey §5.3 / §4) ──────────────────────────────────────────────────────────
//
// `out := *s` aliases the pointer fields (OnFailureChain, RunInputs); a
// per-firing mutation through the copy would silently corrupt the registry's
// canonical spec. copySpec must deep-clone these.
func TestFootgun_CopySpecDeepClonesNestedStructs(t *testing.T) {
	enabled := true
	orig := &task.Spec{
		Name:    "src",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{
			Daemon: true,
		},
		OnFailureChain: &task.OnFailureChainSpec{Task: "auto-fix"},
		RunInputs:      &task.RunInputsTaskOverride{Enabled: &enabled},
	}

	cp := copySpec(orig)

	// OnFailureChain pointer — must be cloned.
	cp.OnFailureChain.Task = "different-fixer"
	if orig.OnFailureChain.Task != "auto-fix" {
		t.Errorf("OnFailureChain shallow copy: orig.Task mutated to %q", orig.OnFailureChain.Task)
	}

	// RunInputs pointer — must be cloned.
	disabled := false
	cp.RunInputs.Enabled = &disabled
	if orig.RunInputs.Enabled == nil || !*orig.RunInputs.Enabled {
		t.Errorf("RunInputs shallow copy: orig.Enabled = %v", orig.RunInputs.Enabled)
	}
}

// ── Footgun #3: resolver mergeOverrides path lacks reserved-key enforcement ──
//
// Survey §5.2: reservedChainParamKeys (taskID, runID, status, output,
// _chain_depth) are rejected at config-load by validatePerEdgeOverrides for
// per-edge sites, but the resolver's *taskset-entry* override path goes
// through mergeOverrides / applyLayer, which do NOT enforce the invariant.
// A reserved-key Params entry inside a nested-taskset override silently
// produces a merged Overrides containing the reserved key.
//
// After the fix, mergeOverrides drops or errors on any reserved-key param,
// matching the per-edge validation contract.
func TestFootgun_MergeOverridesRejectsReservedKey(t *testing.T) {
	a := &Overrides{}
	b := &Overrides{
		Params: []ParamOverride{
			{Name: "taskID", Default: "spoofed"},
			{Name: "real-param", Default: "ok"},
		},
	}

	got := mergeOverrides(a, b)
	if got == nil {
		t.Fatal("mergeOverrides returned nil")
	}

	// The merged result must NOT contain a reserved-key param. Today it
	// does — pinning the regression.
	for _, p := range got.Params {
		if p.Name == "taskID" {
			t.Errorf("mergeOverrides accepted reserved-key param %q (full result: %+v)", p.Name, got.Params)
		}
	}
	// And the legitimate param must still survive the merge.
	var sawReal bool
	for _, p := range got.Params {
		if p.Name == "real-param" {
			sawReal = true
		}
	}
	if !sawReal {
		t.Error("legitimate param real-param was dropped")
	}
}
