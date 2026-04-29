package deno

import (
	"context"
	"testing"

	"github.com/dicode/dicode/pkg/runtime/envresolve"
)

// fakeProviderRunner is a no-op runner used purely to test that the
// NewExecutor copy preserves the field's identity.
type fakeProviderRunner struct{}

func (fakeProviderRunner) Run(_ context.Context, _ string, _ []envresolve.ProviderRequest) (*envresolve.ProviderResult, error) {
	return &envresolve.ProviderResult{Values: map[string]string{}}, nil
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
