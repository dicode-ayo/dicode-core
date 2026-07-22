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
// The minted token carries the MCPScope passed to Mint, so it authorizes
// only the MCP capabilities the calling task's own permissions.dicode
// already grants it — never more than the task could already do directly.
type MCPTokenMinter interface {
	Mint(ctx context.Context, runID string, scope MCPScope) (token string, err error)
	Revoke(ctx context.Context, runID string) error
}

// MCPScope is the set of MCP-server capabilities an ephemeral per-run token
// authorizes, derived from the owning task's declared dicode permissions.
// A zero-value MCPScope (used when the calling spec declares no dicode
// permissions at all) authorizes nothing.
type MCPScope struct {
	ListTasks  bool     `json:"list_tasks,omitempty"`
	RunTaskIDs []string `json:"run_task_ids,omitempty"` // nil = deny run_task; "*" = any task
	// TestTasks gates POST /api/tasks/{id}/test — the REST endpoint the
	// JSON-RPC test_task hint tool points MCP clients at. The hint tool
	// call itself stays an unconditionally-allowed hint (see
	// mcpScopeCheck in pkg/webui/server.go), but the REST endpoint it
	// points to runs the task's sibling test file with full host
	// permissions, so it's separately gated on this flag (#590).
	TestTasks bool `json:"test_tasks,omitempty"`
}

// MCPScopeFor derives the MCP capability scope an ephemeral token minted for
// spec should carry, from spec's own declared permissions.dicode. Mirrors
// exactly what the task itself is allowed to do via the dicode SDK — the
// token must not grant the MCP caller anything the task couldn't already do
// directly. This covers both the JSON-RPC tools/call surface (ListTasks,
// RunTaskIDs) and the REST /api/tasks/{id}/test surface reachable with the
// same Bearer token (TestTasks).
func MCPScopeFor(spec *task.Spec) MCPScope {
	if spec == nil || spec.Permissions.Dicode == nil {
		return MCPScope{}
	}
	d := spec.Permissions.Dicode
	scope := MCPScope{ListTasks: d.ListTasks, TestTasks: d.TasksTest}
	if len(d.Tasks) > 0 {
		scope.RunTaskIDs = append([]string(nil), d.Tasks...)
	}
	return scope
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
	token, err = minter.Mint(ctx, runID, MCPScopeFor(spec))
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
