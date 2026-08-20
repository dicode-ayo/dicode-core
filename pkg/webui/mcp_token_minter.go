package webui

import (
	"context"

	pkgruntime "github.com/dicode/dicode/pkg/runtime"
)

var _ pkgruntime.MCPTokenMinter = (*mcpTokenMinter)(nil)

// mcpTokenMinter implements pkgruntime.MCPTokenMinter over apiKeyStore: one
// dck_-prefixed API key per run, named under ephemeralKeyPrefix so
// revokeByName / revokeByNamePrefix can find and delete it without touching
// operator- or CLI-managed keys.
type mcpTokenMinter struct {
	keys *apiKeyStore
}

func newMCPTokenMinter(keys *apiKeyStore) *mcpTokenMinter {
	return &mcpTokenMinter{keys: keys}
}

// Mint generates a fresh API key named for this run and returns the raw
// value. The key is stored with scope, restricting the MCP tool surface it
// may call (via /mcp's mcpScopeCheck) to exactly the calling task's own
// declared permissions.dicode — never more than the task could already do
// directly. scope is stored as given, including the zero value MCPScope{}
// (a spec with no dicode permissions declared), which correctly denies
// every scoped tool call; it is never treated as "unscoped".
//
// RunID is stamped here rather than derived from the spec: it is what binds
// switch_dev_mode's clone directory to this run, so it must come from the
// mint, not from the tool call that later uses it.
func (m *mcpTokenMinter) Mint(ctx context.Context, runID string, scope pkgruntime.MCPScope) (string, error) {
	scope.RunID = runID
	raw, _, err := m.keys.generateScoped(ctx, ephemeralKeyPrefix+runID, &scope)
	return raw, err
}

// Revoke deletes the run's key.
func (m *mcpTokenMinter) Revoke(ctx context.Context, runID string) error {
	return m.keys.revokeByName(ctx, ephemeralKeyPrefix+runID)
}
