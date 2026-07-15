package deno

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
)

// fakeMCPTokenMinter is a test double for pkgruntime.MCPTokenMinter that
// records Mint/Revoke calls without touching any real key store.
type fakeMCPTokenMinter struct {
	mu      sync.Mutex
	minted  map[string]string
	revoked map[string]bool
	nextTok int
}

func newFakeMCPTokenMinter() *fakeMCPTokenMinter {
	return &fakeMCPTokenMinter{minted: map[string]string{}, revoked: map[string]bool{}}
}

func (f *fakeMCPTokenMinter) Mint(_ context.Context, runID string, _ pkgruntime.MCPScope) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextTok++
	tok := fmt.Sprintf("fake-mcp-token-%d", f.nextTok)
	f.minted[runID] = tok
	return tok, nil
}

func (f *fakeMCPTokenMinter) Revoke(_ context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked[runID] = true
	return nil
}

func (f *fakeMCPTokenMinter) wasRevoked(runID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revoked[runID]
}

func (f *fakeMCPTokenMinter) mintCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.minted)
}

// TestRuntime_MCPToken_MintedWhenDeclared covers the opt-in path: a spec
// that declares permissions.env DICODE_MCP_API_KEY gets a freshly minted
// token injected, and it is revoked once the run completes.
func TestRuntime_MCPToken_MintedWhenDeclared(t *testing.T) {
	e := newTestEnv(t)
	minter := newFakeMCPTokenMinter()
	e.rt.SetMCPTokenMinter(minter)

	spec := &task.Spec{
		ID: "mcp-token-declared", Name: "mcp-token-declared", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		Permissions: task.Permissions{
			Env: []task.EnvEntry{{Name: pkgruntime.MCPTokenEnvName}},
		},
	}
	r := e.runSpec(t, `export default async function main() { return Deno.env.get("DICODE_MCP_API_KEY") ?? null }`, spec)
	if r.Error != nil {
		t.Fatalf("unexpected error: %v", r.Error)
	}
	tok, _ := r.ReturnValue.(string)
	if tok == "" {
		t.Fatalf("task did not see a minted token, got %v", r.ReturnValue)
	}
	if minter.mintCount() != 1 {
		t.Fatalf("expected exactly one mint, got %d", minter.mintCount())
	}
	if !minter.wasRevoked(r.RunID) {
		t.Errorf("expected run %s to be revoked after completion", r.RunID)
	}
}

// TestRuntime_MCPToken_NotMintedWhenNotDeclared covers the gate: a spec
// that does not declare DICODE_MCP_API_KEY must never trigger a mint, even
// when a minter is wired.
func TestRuntime_MCPToken_NotMintedWhenNotDeclared(t *testing.T) {
	e := newTestEnv(t)
	minter := newFakeMCPTokenMinter()
	e.rt.SetMCPTokenMinter(minter)

	r := e.run(t, `return "ok"`)
	if r.Error != nil {
		t.Fatalf("unexpected error: %v", r.Error)
	}
	if minter.mintCount() != 0 {
		t.Fatalf("expected no mint for a spec that doesn't declare DICODE_MCP_API_KEY, got %d", minter.mintCount())
	}
}

// TestRuntime_MCPToken_RevokedOnScriptError covers the revoke-on-every-path
// requirement: a task that throws must still have its ephemeral token
// revoked.
func TestRuntime_MCPToken_RevokedOnScriptError(t *testing.T) {
	e := newTestEnv(t)
	minter := newFakeMCPTokenMinter()
	e.rt.SetMCPTokenMinter(minter)

	spec := &task.Spec{
		ID: "mcp-token-error", Name: "mcp-token-error", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		Permissions: task.Permissions{
			Env: []task.EnvEntry{{Name: pkgruntime.MCPTokenEnvName}},
		},
	}
	r := e.runSpec(t, `export default async function main() { throw new Error("boom") }`, spec)
	if r.Error == nil {
		t.Fatal("expected the run to fail")
	}
	if minter.mintCount() != 1 {
		t.Fatalf("expected exactly one mint even though the run errored, got %d", minter.mintCount())
	}
	if !minter.wasRevoked(r.RunID) {
		t.Errorf("expected run %s to be revoked on the error path", r.RunID)
	}
}
