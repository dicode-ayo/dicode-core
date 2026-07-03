package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/task"
)

// The ExecContext tests were moved from pkg/runtime/python (where the helper
// was called buildExecContext) when the logic was shared with the Deno
// runtime (issue #388).

// TestExecContext_ZeroTimeoutInheritsParent verifies that a zero timeout
// causes ExecContext to wrap the parent with WithCancel rather than imposing
// a new deadline. This locks in the fix for the Python/Deno timeout
// divergence (issue #389): previously Python hardcoded a 60 s default when
// Timeout==0.
func TestExecContext_ZeroTimeoutInheritsParent(t *testing.T) {
	// Parent with a long, explicit deadline so we can detect if the child
	// incorrectly overrides it with a shorter 60 s window.
	parentCtx, parentCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer parentCancel()

	execCtx, cancel := ExecContext(parentCtx, 0)
	defer cancel()

	parentDeadline, parentHas := parentCtx.Deadline()
	execDeadline, execHas := execCtx.Deadline()

	if parentHas != execHas {
		t.Fatalf("parent hasDeadline=%v but exec hasDeadline=%v", parentHas, execHas)
	}
	if parentHas && execDeadline != parentDeadline {
		t.Errorf("zero timeout: exec deadline %v != parent deadline %v — a 60 s default is being imposed",
			execDeadline, parentDeadline)
	}
}

// TestExecContext_ZeroTimeout_NoParentDeadline verifies that when the parent
// has no deadline, a zero timeout also produces no deadline.
func TestExecContext_ZeroTimeout_NoParentDeadline(t *testing.T) {
	execCtx, cancel := ExecContext(context.Background(), 0)
	defer cancel()

	if _, ok := execCtx.Deadline(); ok {
		t.Error("zero timeout with deadline-free parent: expected no deadline")
	}
}

// TestExecContext_NonzeroTimeout sets a deadline on the child context.
func TestExecContext_NonzeroTimeout(t *testing.T) {
	execCtx, cancel := ExecContext(context.Background(), 5*time.Second)
	defer cancel()

	deadline, ok := execCtx.Deadline()
	if !ok {
		t.Fatal("non-zero timeout: expected a deadline on exec context")
	}
	if d := time.Until(deadline); d <= 0 || d > 6*time.Second {
		t.Errorf("non-zero timeout: unexpected deadline distance %v, want ~5s", d)
	}
}

func TestMergeParams(t *testing.T) {
	specParams := []task.Param{
		{Name: "with_default", Default: "d1"},
		{Name: "no_default"}, // empty default → omitted unless overridden
		{Name: "overridden", Default: "d2"},
	}
	got := MergeParams(specParams, map[string]string{
		"overridden": "o2",
		"extra":      "e1", // not declared in spec, still forwarded
	})

	want := map[string]string{
		"with_default": "d1",
		"overridden":   "o2",
		"extra":        "e1",
	}
	if len(got) != len(want) {
		t.Fatalf("MergeParams = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("MergeParams[%q] = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["no_default"]; ok {
		t.Error("param with empty default must be omitted when not overridden")
	}
}

func TestMergeParams_NilOverrides(t *testing.T) {
	got := MergeParams([]task.Param{{Name: "a", Default: "x"}}, nil)
	if len(got) != 1 || got["a"] != "x" {
		t.Errorf("MergeParams with nil overrides = %v, want map[a:x]", got)
	}
}
