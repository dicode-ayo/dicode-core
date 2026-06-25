package python

import (
	"context"
	"testing"
	"time"
)

// TestBuildExecContext_ZeroTimeoutInheritsParent verifies that a zero timeout
// causes buildExecContext to wrap the parent with WithCancel rather than
// imposing a new deadline. This locks in the fix for the Python/Deno timeout
// divergence (issue #389): previously Python hardcoded a 60 s default when
// Timeout==0.
func TestBuildExecContext_ZeroTimeoutInheritsParent(t *testing.T) {
	// Parent with a long, explicit deadline so we can detect if the child
	// incorrectly overrides it with a shorter 60 s window.
	parentCtx, parentCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer parentCancel()

	execCtx, cancel := buildExecContext(parentCtx, 0)
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

// TestBuildExecContext_ZeroTimeout_NoParentDeadline verifies that when the
// parent has no deadline, a zero timeout also produces no deadline.
func TestBuildExecContext_ZeroTimeout_NoParentDeadline(t *testing.T) {
	execCtx, cancel := buildExecContext(context.Background(), 0)
	defer cancel()

	if _, ok := execCtx.Deadline(); ok {
		t.Error("zero timeout with deadline-free parent: expected no deadline")
	}
}

// TestBuildExecContext_NonzeroTimeout sets a deadline on the child context.
func TestBuildExecContext_NonzeroTimeout(t *testing.T) {
	execCtx, cancel := buildExecContext(context.Background(), 5*time.Second)
	defer cancel()

	deadline, ok := execCtx.Deadline()
	if !ok {
		t.Fatal("non-zero timeout: expected a deadline on exec context")
	}
	if d := time.Until(deadline); d <= 0 || d > 6*time.Second {
		t.Errorf("non-zero timeout: unexpected deadline distance %v, want ~5s", d)
	}
}
