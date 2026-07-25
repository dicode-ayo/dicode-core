package runtime

import (
	"context"
	"reflect"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

// TestMCPScopeFor covers the derivation of an ephemeral token's MCP
// capability scope from a spec's own declared permissions.dicode — the
// scope must mirror exactly what the task itself is allowed to do, never
// more.
func TestMCPScopeFor(t *testing.T) {
	tests := []struct {
		name string
		spec *task.Spec
		want MCPScope
	}{
		{
			name: "nil spec yields zero scope",
			spec: nil,
			want: MCPScope{},
		},
		{
			name: "spec with nil Permissions.Dicode yields zero scope",
			spec: &task.Spec{},
			want: MCPScope{},
		},
		{
			name: "ListTasks true is carried through",
			spec: &task.Spec{Permissions: task.Permissions{
				Dicode: &task.DicodePermissions{ListTasks: true},
			}},
			want: MCPScope{ListTasks: true},
		},
		{
			name: "Tasks wildcard is carried through as RunTaskIDs",
			spec: &task.Spec{Permissions: task.Permissions{
				Dicode: &task.DicodePermissions{Tasks: []string{"*"}},
			}},
			want: MCPScope{RunTaskIDs: []string{"*"}},
		},
		{
			name: "Tasks explicit list is carried through as RunTaskIDs",
			spec: &task.Spec{Permissions: task.Permissions{
				Dicode: &task.DicodePermissions{Tasks: []string{"a", "b"}},
			}},
			want: MCPScope{RunTaskIDs: []string{"a", "b"}},
		},
		{
			name: "ListTasks and Tasks together",
			spec: &task.Spec{Permissions: task.Permissions{
				Dicode: &task.DicodePermissions{ListTasks: true, Tasks: []string{"a"}},
			}},
			want: MCPScope{ListTasks: true, RunTaskIDs: []string{"a"}},
		},
		{
			name: "Dicode declared but no MCP-relevant fields set yields zero scope",
			spec: &task.Spec{Permissions: task.Permissions{
				Dicode: &task.DicodePermissions{GetRuns: true},
			}},
			want: MCPScope{},
		},
		{
			name: "TasksTest true is carried through",
			spec: &task.Spec{Permissions: task.Permissions{
				Dicode: &task.DicodePermissions{TasksTest: true},
			}},
			want: MCPScope{TestTasks: true},
		},
		{
			name: "TasksTest unset yields TestTasks false",
			spec: &task.Spec{Permissions: task.Permissions{
				Dicode: &task.DicodePermissions{ListTasks: true},
			}},
			want: MCPScope{ListTasks: true, TestTasks: false},
		},
		{
			name: "ListTasks, Tasks, and TasksTest together",
			spec: &task.Spec{Permissions: task.Permissions{
				Dicode: &task.DicodePermissions{ListTasks: true, Tasks: []string{"a"}, TasksTest: true},
			}},
			want: MCPScope{ListTasks: true, RunTaskIDs: []string{"a"}, TestTasks: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MCPScopeFor(tt.spec)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("MCPScopeFor() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// fakeMCPTokenMinter is a test double for MCPTokenMinter that records the
// scope it was minted with, so ApplyMCPToken's derivation can be asserted
// without a real key store.
type fakeMCPTokenMinter struct {
	mintedScope MCPScope
	mintCalls   int
	revoked     map[string]bool
}

func newFakeMCPTokenMinter() *fakeMCPTokenMinter {
	return &fakeMCPTokenMinter{revoked: map[string]bool{}}
}

func (f *fakeMCPTokenMinter) Mint(_ context.Context, _ string, scope MCPScope) (string, error) {
	f.mintCalls++
	f.mintedScope = scope
	return "fake-mcp-token", nil
}

func (f *fakeMCPTokenMinter) Revoke(_ context.Context, runID string) error {
	f.revoked[runID] = true
	return nil
}

// TestApplyMCPToken_PassesDerivedScope covers the wiring between
// ApplyMCPToken and MCPScopeFor: the scope handed to the minter's Mint call
// must be exactly what MCPScopeFor computes for the run's spec.
func TestApplyMCPToken_PassesDerivedScope(t *testing.T) {
	spec := &task.Spec{
		Permissions: task.Permissions{
			Env:    []task.EnvEntry{{Name: MCPTokenEnvName}},
			Dicode: &task.DicodePermissions{ListTasks: true, Tasks: []string{"repo/a", "repo/b"}},
		},
	}
	minter := newFakeMCPTokenMinter()
	live := &BridgeDeps{MCPTokenMinter: minter}
	resolved := map[string]string{}

	token, revoke, err := ApplyMCPToken(context.Background(), live, nil, spec, "run-1", resolved)
	if err != nil {
		t.Fatalf("ApplyMCPToken: %v", err)
	}
	defer revoke()

	if minter.mintCalls != 1 {
		t.Fatalf("expected exactly one Mint call, got %d", minter.mintCalls)
	}
	want := MCPScopeFor(spec)
	if !reflect.DeepEqual(minter.mintedScope, want) {
		t.Errorf("scope passed to Mint = %+v, want %+v", minter.mintedScope, want)
	}
	if token == "" {
		t.Fatal("expected a non-empty minted token")
	}
	if resolved[MCPTokenEnvName] != token {
		t.Errorf("resolved[%s] = %q, want %q", MCPTokenEnvName, resolved[MCPTokenEnvName], token)
	}
}

// TestApplyMCPToken_ZeroScopeWhenNoDicodePermissions covers the
// deny-by-default case: a spec that opts into the ephemeral token but
// declares no permissions.dicode at all must still get a token minted (the
// env opt-in and the dicode capability grant are independent), but with a
// zero-value scope that authorizes nothing at the MCP layer.
func TestApplyMCPToken_ZeroScopeWhenNoDicodePermissions(t *testing.T) {
	spec := &task.Spec{
		Permissions: task.Permissions{
			Env: []task.EnvEntry{{Name: MCPTokenEnvName}},
		},
	}
	minter := newFakeMCPTokenMinter()
	live := &BridgeDeps{MCPTokenMinter: minter}
	resolved := map[string]string{}

	_, revoke, err := ApplyMCPToken(context.Background(), live, nil, spec, "run-2", resolved)
	if err != nil {
		t.Fatalf("ApplyMCPToken: %v", err)
	}
	defer revoke()

	if minter.mintCalls != 1 {
		t.Fatalf("expected exactly one Mint call, got %d", minter.mintCalls)
	}
	if !reflect.DeepEqual(minter.mintedScope, MCPScope{}) {
		t.Errorf("scope passed to Mint = %+v, want zero value", minter.mintedScope)
	}
}
