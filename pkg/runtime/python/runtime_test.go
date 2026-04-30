package python

import (
	"context"
	"testing"

	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/taskset"
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

// fakeSourceDevModeSetter satisfies ipc.SourceDevModeSetter for testing.
type fakeSourceDevModeSetter struct{}

func (fakeSourceDevModeSetter) SetDevMode(_ context.Context, _ string, _ bool, _ taskset.DevModeOpts) error {
	return nil
}

// fakeRepoPathResolver satisfies ipc.RepoPathResolver for testing.
type fakeRepoPathResolver struct{}

func (fakeRepoPathResolver) ResolveRepoPath(_ string) (string, error) { return "/repo", nil }

// TestRuntime_SetReplayer_Propagates verifies the late-wiring pattern for
// SetReplayer on the Python runtime: an executor created before SetReplayer is
// called on the parent must see the replayer via exec.parent.replayer.
func TestRuntime_SetReplayer_Propagates(t *testing.T) {
	rt, err := New(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("python.New: %v", err)
	}

	exec, ok := rt.NewExecutor("/usr/bin/uv").(*executor)
	if !ok {
		t.Fatalf("NewExecutor did not return *executor")
	}

	if exec.parent.replayer != nil {
		t.Error("expected nil before SetReplayer")
	}

	is := newTestInputStore()
	r := registry.NewReplayer(registry.New(nil), is, nil)
	rt.SetReplayer(r)

	if got := exec.parent.replayer; got != r {
		t.Errorf("executor did not pick up late-set Replayer: got %v, want %v", got, r)
	}
}

// TestRuntime_SetSourceManager_Propagates verifies the late-wiring pattern for
// SetSourceManager on the Python runtime.
func TestRuntime_SetSourceManager_Propagates(t *testing.T) {
	rt, err := New(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("python.New: %v", err)
	}

	exec, ok := rt.NewExecutor("/usr/bin/uv").(*executor)
	if !ok {
		t.Fatalf("NewExecutor did not return *executor")
	}

	if exec.parent.sourceMgr != nil {
		t.Error("expected nil before SetSourceManager")
	}

	var m ipc.SourceDevModeSetter = fakeSourceDevModeSetter{}
	rt.SetSourceManager(m)

	if got := exec.parent.sourceMgr; got != m {
		t.Errorf("executor did not pick up late-set SourceManager: got %v, want %v", got, m)
	}
}

// TestRuntime_SetRepoResolver_Propagates verifies the late-wiring pattern for
// SetRepoResolver on the Python runtime.
func TestRuntime_SetRepoResolver_Propagates(t *testing.T) {
	rt, err := New(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("python.New: %v", err)
	}

	exec, ok := rt.NewExecutor("/usr/bin/uv").(*executor)
	if !ok {
		t.Fatalf("NewExecutor did not return *executor")
	}

	if exec.parent.repoResolver != nil {
		t.Error("expected nil before SetRepoResolver")
	}

	var r ipc.RepoPathResolver = fakeRepoPathResolver{}
	rt.SetRepoResolver(r)

	if got := exec.parent.repoResolver; got != r {
		t.Errorf("executor did not pick up late-set RepoResolver: got %v, want %v", got, r)
	}
}
