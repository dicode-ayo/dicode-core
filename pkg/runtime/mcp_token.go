package runtime

import (
	"context"
	"fmt"

	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// MCPTokenEnvName is the permissions.env name a task declares to opt into
// receiving an ephemeral per-run dicode MCP API key in place of a static
// secret under the same name.
const MCPTokenEnvName = "DICODE_MCP_API_KEY"

// MCPTokenMinter mints and revokes a short-lived dicode MCP API key scoped
// to a single run, removing the need for `dicode mcp install`'s
// operator-managed key for internal agent tasks. Implemented by pkg/webui
// over its API-key store; the interface lives here so pkg/runtime does not
// need to import pkg/webui.
//
// MVP scope: the minted token is full-surface — the same permissions as an
// operator-managed key — not yet scoped to the task's declared capabilities.
// Per-capability scoping is a follow-on.
type MCPTokenMinter interface {
	Mint(ctx context.Context, runID string) (token string, err error)
	Revoke(ctx context.Context, runID string) error
}

// WantsMCPToken reports whether spec declares a permissions.env entry named
// MCPTokenEnvName — the signal a task uses to opt into an ephemeral per-run
// MCP token.
func WantsMCPToken(spec *task.Spec) bool {
	if spec == nil {
		return false
	}
	for _, e := range spec.Permissions.Env {
		if e.Name == MCPTokenEnvName {
			return true
		}
	}
	return false
}

// ApplyMCPToken mints an ephemeral MCP token and stores it into
// resolved[MCPTokenEnvName] when spec opts in (WantsMCPToken) and a minter
// is wired (live.MCPTokenMinter). The mint overrides any secret-store value
// already present under that name — the ephemeral per-run token always
// wins. Nothing is minted, and the no-op revoke is returned, when the spec
// doesn't declare the env entry or no minter is wired (legacy/test path:
// the static-secret env value, if any, is left untouched).
//
// The minted token value is returned so the caller can fold it into the
// run-log redactor (secrets.Redactor.WithExtra) before any log stream or IPC
// server is wired: the redactor is snapshot from the secrets resolved for the
// run, which do not include a token minted after resolution, so without this
// the ephemeral key would pass through run logs unredacted. token is "" when
// nothing was minted.
//
// Callers MUST defer the returned revoke unconditionally, immediately after
// this call returns, so the token is revoked on every exit path (success,
// error, timeout, panic). Revoke runs against context.Background() rather
// than the run's ctx: a timed-out or canceled run is exactly the case where
// revocation matters most, and it must not be defeated by the same
// cancellation that ended the run.
func ApplyMCPToken(ctx context.Context, live *BridgeDeps, log *zap.Logger, spec *task.Spec, runID string, resolved map[string]string) (token string, revoke func(), err error) {
	noop := func() {}
	minter := live.MCPTokenMinter
	if minter == nil || !WantsMCPToken(spec) {
		return "", noop, nil
	}
	token, err = minter.Mint(ctx, runID)
	if err != nil {
		return "", noop, fmt.Errorf("mint mcp token: %w", err)
	}
	resolved[MCPTokenEnvName] = token
	return token, func() {
		if rerr := minter.Revoke(context.Background(), runID); rerr != nil && log != nil {
			log.Warn("revoke ephemeral mcp token failed", zap.String("run", runID), zap.Error(rerr))
		}
	}, nil
}
