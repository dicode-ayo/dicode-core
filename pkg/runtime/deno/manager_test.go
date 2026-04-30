package deno

import (
	"context"
	"testing"

	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/runtime/envresolve"
	"github.com/dicode/dicode/pkg/taskset"
)

// fakeProviderRunner is a no-op runner used purely to test that the
// NewExecutor copy preserves the field's identity.
type fakeProviderRunner struct{}

func (fakeProviderRunner) Run(_ context.Context, _ string, _ []envresolve.ProviderRequest) (*envresolve.ProviderResult, error) {
	return &envresolve.ProviderResult{Values: map[string]string{}}, nil
}

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

// TestNewExecutor_PropagatesProviderFields pins the contract that
// Runtime.NewExecutor must copy the issue #119 fields to the new
// per-run Runtime. Regression test for the gap caught by the PR #232
// security review (commit d0731c9): the trigger-engine dispatch path
// goes through NewExecutor, so omitting these fields silently disables
// secret-provider routing in production.
func TestNewExecutor_PropagatesProviderFields(t *testing.T) {
	parent := &Runtime{
		secretOutputCh: make(chan map[string]string, 1),
		providerRunner: fakeProviderRunner{},
	}

	exec, ok := parent.NewExecutor("/usr/bin/deno").(*Runtime)
	if !ok {
		t.Fatalf("NewExecutor did not return *Runtime")
	}
	if exec.secretOutputCh != parent.secretOutputCh {
		t.Errorf("secretOutputCh not propagated: got %v, want %v", exec.secretOutputCh, parent.secretOutputCh)
	}
	if exec.providerRunner != parent.providerRunner {
		t.Errorf("providerRunner not propagated: got %v, want %v", exec.providerRunner, parent.providerRunner)
	}
}

// TestNewExecutor_SeesLateInputStore is a regression test for the bug fixed in
// issue #233 pass-2: NewExecutor runs inside buildRuntimes BEFORE the daemon's
// SetInputStore call, so any snapshot of inputStore taken at construction time
// would permanently be nil. The fix uses a parent back-reference so the live
// value is read at IPC server creation time.
//
// This test constructs an executor BEFORE calling SetInputStore on the parent,
// then verifies that effectiveInputStore() returns the store that was set
// afterwards — proving the live-lookup path works end-to-end.
func TestNewExecutor_SeesLateInputStore(t *testing.T) {
	parent := &Runtime{}

	exec, ok := parent.NewExecutor("/usr/bin/deno").(*Runtime)
	if !ok {
		t.Fatalf("NewExecutor did not return *Runtime")
	}

	// Sanity: before SetInputStore, both parent and executor see nil.
	if got := exec.effectiveInputStore(); got != nil {
		t.Errorf("expected nil before SetInputStore; got %v", got)
	}

	// Now wire the store — AFTER the executor was created, mirroring the
	// daemon ordering that triggered the original bug.
	is := newTestInputStore()
	parent.SetInputStore(is)

	// The executor must pick it up via the parent back-reference.
	if got := exec.effectiveInputStore(); got != is {
		t.Errorf("executor did not pick up late-set InputStore: got %v, want %v", got, is)
	}
}

// TestManagerRuntime_EffectiveInputStore_NilParent verifies that the
// manager-owned Runtime (parent == nil) reads its own inputStore field, i.e.
// SetInputStore on the manager-level object is immediately visible through
// effectiveInputStore() without requiring a parent.
func TestManagerRuntime_EffectiveInputStore_NilParent(t *testing.T) {
	rt := &Runtime{} // parent is nil — this is the manager-owned instance

	if rt.effectiveInputStore() != nil {
		t.Error("expected nil before SetInputStore")
	}

	is := newTestInputStore()
	rt.SetInputStore(is)

	if got := rt.effectiveInputStore(); got != is {
		t.Errorf("manager runtime: effectiveInputStore() = %v, want %v", got, is)
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
// SetReplayer: an executor created before SetReplayer is called on the parent
// must see the replayer via effectiveReplayer() (parent back-reference).
func TestRuntime_SetReplayer_Propagates(t *testing.T) {
	parent := &Runtime{}
	exec, ok := parent.NewExecutor("/usr/bin/deno").(*Runtime)
	if !ok {
		t.Fatalf("NewExecutor did not return *Runtime")
	}

	if exec.effectiveReplayer() != nil {
		t.Error("expected nil before SetReplayer")
	}

	is := newTestInputStore()
	r := registry.NewReplayer(registry.New(nil), is, nil)
	parent.SetReplayer(r)

	if got := exec.effectiveReplayer(); got != r {
		t.Errorf("executor did not pick up late-set Replayer: got %v, want %v", got, r)
	}
}

// TestRuntime_SetSourceManager_Propagates verifies the late-wiring pattern for
// SetSourceManager: an executor sees the manager via effectiveSourceMgr().
func TestRuntime_SetSourceManager_Propagates(t *testing.T) {
	parent := &Runtime{}
	exec, ok := parent.NewExecutor("/usr/bin/deno").(*Runtime)
	if !ok {
		t.Fatalf("NewExecutor did not return *Runtime")
	}

	if exec.effectiveSourceMgr() != nil {
		t.Error("expected nil before SetSourceManager")
	}

	var m ipc.SourceDevModeSetter = fakeSourceDevModeSetter{}
	parent.SetSourceManager(m)

	if got := exec.effectiveSourceMgr(); got != m {
		t.Errorf("executor did not pick up late-set SourceManager: got %v, want %v", got, m)
	}
}

// TestRuntime_SetRepoResolver_Propagates verifies the late-wiring pattern for
// SetRepoResolver: an executor sees the resolver via effectiveRepoResolver().
func TestRuntime_SetRepoResolver_Propagates(t *testing.T) {
	parent := &Runtime{}
	exec, ok := parent.NewExecutor("/usr/bin/deno").(*Runtime)
	if !ok {
		t.Fatalf("NewExecutor did not return *Runtime")
	}

	if exec.effectiveRepoResolver() != nil {
		t.Error("expected nil before SetRepoResolver")
	}

	var r ipc.RepoPathResolver = fakeRepoPathResolver{}
	parent.SetRepoResolver(r)

	if got := exec.effectiveRepoResolver(); got != r {
		t.Errorf("executor did not pick up late-set RepoResolver: got %v, want %v", got, r)
	}
}
