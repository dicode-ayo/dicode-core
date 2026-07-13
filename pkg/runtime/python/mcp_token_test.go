//go:build !windows

package python

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	uvpkg "github.com/dicode/dicode/pkg/uv"
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

// newMCPTokenExecutor provisions uv the way the runtime does and returns a
// registry + runtime + executor, skipping when uv cannot be provisioned
// (offline CI) — mirrors newSuspendExecutor in suspend_test.go.
func newMCPTokenExecutor(t *testing.T) (*Runtime, *registry.Registry, pkgruntime.Executor) {
	t.Helper()
	uv, err := uvpkg.EnsureUv("")
	if err != nil {
		t.Skipf("uv provisioning failed (offline?): %v", err)
	}
	rt, reg := newTestRuntime(t)
	return rt, reg, rt.NewExecutor(uv)
}

// dumpRunLogs logs the run's log lines via t.Logf, for debugging a failed
// Execute call.
func dumpRunLogs(t *testing.T, reg *registry.Registry, runID string) {
	t.Helper()
	logs, err := reg.GetRunLogs(context.Background(), runID)
	if err != nil {
		t.Logf("GetRunLogs: %v", err)
		return
	}
	for _, l := range logs {
		t.Logf("[%s] %s", l.Level, l.Message)
	}
}

// TestExecute_MCPToken_MintedWhenDeclared covers the opt-in path: a spec
// that declares permissions.env DICODE_MCP_API_KEY gets a freshly minted
// token injected, and it is revoked once the run completes.
func TestExecute_MCPToken_MintedWhenDeclared(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rt, reg, ex := newMCPTokenExecutor(t)
	minter := newFakeMCPTokenMinter()
	rt.SetMCPTokenMinter(minter)

	spec := writePythonTask(t, "mcp-token-declared", `
result = env.get("DICODE_MCP_API_KEY")
`)
	spec.Permissions = task.Permissions{Env: []task.EnvEntry{{Name: pkgruntime.MCPTokenEnvName}}}

	runID := "test-run-mcp-declared"
	res, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{RunID: runID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Error != nil {
		dumpRunLogs(t, reg, runID)
		t.Fatalf("unexpected run error: %v", res.Error)
	}
	tok, _ := res.ChainInput.(string)
	if tok == "" {
		dumpRunLogs(t, reg, runID)
		t.Fatalf("task did not see a minted token, got %v", res.ChainInput)
	}
	if minter.mintCount() != 1 {
		t.Fatalf("expected exactly one mint, got %d", minter.mintCount())
	}
	if !minter.wasRevoked(runID) {
		t.Errorf("expected run %s to be revoked after completion", runID)
	}
}

// TestExecute_MCPToken_NotMintedWhenNotDeclared covers the gate: a spec
// that does not declare DICODE_MCP_API_KEY must never trigger a mint, even
// when a minter is wired.
func TestExecute_MCPToken_NotMintedWhenNotDeclared(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rt, reg, ex := newMCPTokenExecutor(t)
	minter := newFakeMCPTokenMinter()
	rt.SetMCPTokenMinter(minter)

	spec := writePythonTask(t, "mcp-token-not-declared", `result = "ok"`)

	runID := "test-run-mcp-not-declared"
	res, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{RunID: runID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Error != nil {
		dumpRunLogs(t, reg, runID)
		t.Fatalf("unexpected run error: %v", res.Error)
	}
	if minter.mintCount() != 0 {
		t.Fatalf("expected no mint for a spec that doesn't declare DICODE_MCP_API_KEY, got %d", minter.mintCount())
	}
}

// TestExecute_MCPToken_RevokedOnScriptError covers the revoke-on-every-path
// requirement: a task that raises must still have its ephemeral token
// revoked.
func TestExecute_MCPToken_RevokedOnScriptError(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rt, reg, ex := newMCPTokenExecutor(t)
	minter := newFakeMCPTokenMinter()
	rt.SetMCPTokenMinter(minter)

	spec := writePythonTask(t, "mcp-token-error", `raise RuntimeError("boom")`)
	spec.Permissions = task.Permissions{Env: []task.EnvEntry{{Name: pkgruntime.MCPTokenEnvName}}}

	runID := "test-run-mcp-error"
	res, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{RunID: runID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Error == nil {
		dumpRunLogs(t, reg, runID)
		t.Fatal("expected the run to fail")
	}
	if minter.mintCount() != 1 {
		t.Fatalf("expected exactly one mint even though the run errored, got %d", minter.mintCount())
	}
	if !minter.wasRevoked(runID) {
		t.Errorf("expected run %s to be revoked on the error path", runID)
	}
}
