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
// value. The token is full-surface — the same permissions as an
// operator-managed key — not yet scoped to the task's declared
// capabilities; per-capability scoping is a follow-on.
func (m *mcpTokenMinter) Mint(ctx context.Context, runID string) (string, error) {
	raw, _, err := m.keys.generate(ctx, ephemeralKeyPrefix+runID)
	return raw, err
}

// Revoke deletes the run's key.
func (m *mcpTokenMinter) Revoke(ctx context.Context, runID string) error {
	return m.keys.revokeByName(ctx, ephemeralKeyPrefix+runID)
}
