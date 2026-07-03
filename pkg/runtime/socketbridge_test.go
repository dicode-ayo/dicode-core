package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/runtime/envresolve"
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

// TestResolveRunEnv_PreferredPreflight verifies that a preflight-resolved
// env is used verbatim and that the resolver factory is NOT invoked — the
// whole point of forwarding PreResolvedEnv is to avoid re-spawning provider
// tasks (issue #235).
func TestResolveRunEnv_PreferredPreflight(t *testing.T) {
	pre := &envresolve.Resolved{
		Env:     map[string]string{"FOO": "bar", "TOKEN": "s3cret-value"},
		Secrets: map[string]string{"TOKEN": "s3cret-value"},
	}
	resolver := func() *envresolve.Resolver {
		t.Fatal("resolver must not be constructed when PreResolvedEnv is set")
		return nil
	}

	env, red, err := ResolveRunEnv(context.Background(), &task.Spec{ID: "t"}, pre, resolver)
	if err != nil {
		t.Fatalf("ResolveRunEnv: %v", err)
	}
	if env["FOO"] != "bar" || env["TOKEN"] != "s3cret-value" {
		t.Errorf("env not passed through: %v", env)
	}
	if got := red.RedactString("leak s3cret-value here"); got == "leak s3cret-value here" {
		t.Errorf("redactor does not cover preflight secrets: %q", got)
	}
}

// TestResolveRunEnv_InlineFallback verifies the legacy path: with no
// preflight result the resolver factory is invoked and its result is
// returned. A spec with no env permissions resolves to an empty env.
func TestResolveRunEnv_InlineFallback(t *testing.T) {
	called := false
	resolver := func() *envresolve.Resolver {
		called = true
		return envresolve.New(nil, nil, nil)
	}

	env, red, err := ResolveRunEnv(context.Background(), &task.Spec{ID: "t"}, nil, resolver)
	if err != nil {
		t.Fatalf("ResolveRunEnv: %v", err)
	}
	if !called {
		t.Fatal("resolver factory was not invoked on the inline path")
	}
	if len(env) != 0 {
		t.Errorf("expected empty env for spec without env permissions, got %v", env)
	}
	if red == nil {
		t.Error("expected a (no-op) redactor, got nil")
	}
}

// TestLiveResolver_Precedence pins the resolver selection order both
// runtimes relied on: parent's shared resolver > self's shared resolver >
// fresh instance.
func TestLiveResolver_Precedence(t *testing.T) {
	parentShared := envresolve.New(nil, nil, nil)
	selfShared := envresolve.New(nil, nil, nil)

	t.Run("parent shared wins", func(t *testing.T) {
		self := &BridgeDeps{SharedResolver: selfShared}
		parent := &BridgeDeps{SharedResolver: parentShared}
		if got := LiveResolver(self, parent); got != parentShared {
			t.Errorf("got %p, want parent's shared resolver %p", got, parentShared)
		}
	})

	t.Run("self shared when parent has none", func(t *testing.T) {
		self := &BridgeDeps{SharedResolver: selfShared}
		if got := LiveResolver(self, &BridgeDeps{}); got != selfShared {
			t.Error("expected self's shared resolver when parent's is nil")
		}
		if got := LiveResolver(self, nil); got != selfShared {
			t.Error("expected self's shared resolver when parent is nil (manager path)")
		}
	})

	t.Run("fresh instance as fallback", func(t *testing.T) {
		self := &BridgeDeps{}
		got := LiveResolver(self, nil)
		if got == nil {
			t.Fatal("expected a fresh resolver, got nil")
		}
		if got == parentShared || got == selfShared {
			t.Error("fallback must construct a new instance")
		}
	})
}

func TestMergeParams_NilOverrides(t *testing.T) {
	got := MergeParams([]task.Param{{Name: "a", Default: "x"}}, nil)
	if len(got) != 1 || got["a"] != "x" {
		t.Errorf("MergeParams with nil overrides = %v, want map[a:x]", got)
	}
}
