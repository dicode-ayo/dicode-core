package python

import (
	"context"
	"testing"

	"github.com/dicode/dicode/pkg/registry"
)

// fakeTaskRunner satisfies registry.TaskRunner for InputStore construction.
type fakeTaskRunner struct{}

func (fakeTaskRunner) RunTaskSync(_ context.Context, _ string, _ map[string]string) (any, error) {
	return map[string]any{"ok": true}, nil
}

// newTestInputStore builds a minimal InputStore backed by a deterministic key.
func newTestInputStore() *registry.InputStore {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return registry.NewInputStore(registry.NewInputCrypto(key), fakeTaskRunner{}, "fake-storage")
}

// TestNewExecutor_SeesLateInputStore is the Python-runtime analogue of the
// deno regression test for issue #233 pass-2. NewExecutor (called inside
// buildRuntimes) ran BEFORE daemon.go called SetInputStore, so the executor
// permanently saw inputStore == nil and delete_input / get_input were broken
// for any task using a non-default Python version.
//
// The fix stores a parent back-reference in the executor and reads
// parent.inputStore at IPC server creation time. This test constructs an
// executor BEFORE SetInputStore is called and then verifies the executor sees
// the late-set store — confirming the live-lookup path.
func TestNewExecutor_SeesLateInputStore(t *testing.T) {
	rt, err := New(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("python.New: %v", err)
	}

	// Create the executor BEFORE wiring the InputStore — this mirrors the
	// daemon ordering that triggered the original bug.
	exec, ok := rt.NewExecutor("/usr/bin/uv").(*executor)
	if !ok {
		t.Fatalf("NewExecutor did not return *executor")
	}

	// Sanity: parent back-reference must point to rt.
	if exec.parent != rt {
		t.Errorf("executor.parent = %v, want %v", exec.parent, rt)
	}

	// Before SetInputStore the live lookup must return nil.
	if got := exec.parent.inputStore; got != nil {
		t.Errorf("expected nil before SetInputStore; got %v", got)
	}

	// Wire the store AFTER executor creation.
	is := newTestInputStore()
	rt.SetInputStore(is)

	// The executor must now see the store via the parent back-reference.
	if got := exec.parent.inputStore; got != is {
		t.Errorf("executor did not pick up late-set InputStore: got %v, want %v", got, is)
	}
}
