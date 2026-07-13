package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	pkgruntime "github.com/dicode/dicode/pkg/runtime"
)

// ── generateScoped / scopeFor round-trip ────────────────────────────────────

// TestGenerateScoped_ScopeFor_RoundTrip covers the storage round-trip: a
// scope handed to generateScoped comes back unchanged from scopeFor.
func TestGenerateScoped_ScopeFor_RoundTrip(t *testing.T) {
	ctx := context.Background()
	keys := newTestAPIKeyStore(t)

	scope := &pkgruntime.MCPScope{ListTasks: true, RunTaskIDs: []string{"repo/a", "repo/b"}}
	raw, _, err := keys.generateScoped(ctx, "scoped-key", scope)
	if err != nil {
		t.Fatalf("generateScoped: %v", err)
	}

	got, found := keys.scopeFor(ctx, raw)
	if !found {
		t.Fatal("scopeFor: key not found")
	}
	if got == nil {
		t.Fatal("scopeFor: expected a non-nil scope")
	}
	if !reflect.DeepEqual(*got, *scope) {
		t.Errorf("scopeFor() = %+v, want %+v", *got, *scope)
	}
}

// TestGenerateScoped_ZeroScope_DeniesEverything covers the deny-by-default
// case: MCPScope{} is stored and read back as a non-nil, empty scope — not
// conflated with "unscoped" (nil).
func TestGenerateScoped_ZeroScope_DeniesEverything(t *testing.T) {
	ctx := context.Background()
	keys := newTestAPIKeyStore(t)

	raw, _, err := keys.generateScoped(ctx, "zero-scope-key", &pkgruntime.MCPScope{})
	if err != nil {
		t.Fatalf("generateScoped: %v", err)
	}
	got, found := keys.scopeFor(ctx, raw)
	if !found {
		t.Fatal("scopeFor: key not found")
	}
	if got == nil {
		t.Fatal("scopeFor: expected a non-nil (zero-value) scope, got nil (unscoped)")
	}
	if !reflect.DeepEqual(*got, pkgruntime.MCPScope{}) {
		t.Errorf("scopeFor() = %+v, want zero value", *got)
	}
}

// TestGenerate_ScopeFor_Unscoped covers the default/back-compat path: keys
// created via generate (dashboard/CLI) or generateScoped(..., nil) come back
// as scopeFor == nil, meaning full/unscoped access.
func TestGenerate_ScopeFor_Unscoped(t *testing.T) {
	ctx := context.Background()
	keys := newTestAPIKeyStore(t)

	rawGenerate, _, err := keys.generate(ctx, "dashboard-key")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	rawGenerateScopedNil, _, err := keys.generateScoped(ctx, "explicit-nil-key", nil)
	if err != nil {
		t.Fatalf("generateScoped(nil): %v", err)
	}

	for _, raw := range []string{rawGenerate, rawGenerateScopedNil} {
		got, found := keys.scopeFor(ctx, raw)
		if !found {
			t.Fatalf("scopeFor(%q): key not found", raw)
		}
		if got != nil {
			t.Errorf("scopeFor(%q) = %+v, want nil (unscoped)", raw, *got)
		}
	}
}

// TestScopeFor_UnknownKey covers the defensive not-found path.
func TestScopeFor_UnknownKey(t *testing.T) {
	ctx := context.Background()
	keys := newTestAPIKeyStore(t)
	if _, found := keys.scopeFor(ctx, "dck_does-not-exist"); found {
		t.Error("scopeFor should report not found for an unknown key")
	}
}

// ── mcpScopeCheck table tests ────────────────────────────────────────────────

func rpcBody(t *testing.T, id any, method, toolName string, args map[string]any) []byte {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if toolName != "" || args != nil {
		params := map[string]any{}
		if toolName != "" {
			params["name"] = toolName
		}
		if args != nil {
			params["arguments"] = args
		}
		body["params"] = params
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return b
}

func TestMcpScopeCheck(t *testing.T) {
	fullScope := &pkgruntime.MCPScope{ListTasks: true, RunTaskIDs: []string{"repo/allowed"}}
	wildcardScope := &pkgruntime.MCPScope{RunTaskIDs: []string{"*"}}
	zeroScope := &pkgruntime.MCPScope{}

	tests := []struct {
		name        string
		scope       *pkgruntime.MCPScope
		body        []byte
		wantAllowed bool
	}{
		{
			name:        "initialize always allowed",
			scope:       zeroScope,
			body:        rpcBody(t, 1, "initialize", "", nil),
			wantAllowed: true,
		},
		{
			name:        "tools/list always allowed",
			scope:       zeroScope,
			body:        rpcBody(t, 2, "tools/list", "", nil),
			wantAllowed: true,
		},
		{
			name:        "list_tasks allowed when ListTasks true",
			scope:       fullScope,
			body:        rpcBody(t, 3, "tools/call", "list_tasks", map[string]any{}),
			wantAllowed: true,
		},
		{
			name:        "list_tasks denied when ListTasks false",
			scope:       zeroScope,
			body:        rpcBody(t, 4, "tools/call", "list_tasks", map[string]any{}),
			wantAllowed: false,
		},
		{
			name:        "get_task allowed when ListTasks true",
			scope:       fullScope,
			body:        rpcBody(t, 5, "tools/call", "get_task", map[string]any{"id": "repo/x"}),
			wantAllowed: true,
		},
		{
			name:        "get_task denied when ListTasks false",
			scope:       zeroScope,
			body:        rpcBody(t, 6, "tools/call", "get_task", map[string]any{"id": "repo/x"}),
			wantAllowed: false,
		},
		{
			name:        "run_task allowed for a listed id",
			scope:       fullScope,
			body:        rpcBody(t, 7, "tools/call", "run_task", map[string]any{"id": "repo/allowed"}),
			wantAllowed: true,
		},
		{
			name:        "run_task denied for an unlisted id",
			scope:       fullScope,
			body:        rpcBody(t, 8, "tools/call", "run_task", map[string]any{"id": "repo/other"}),
			wantAllowed: false,
		},
		{
			name:        "run_task denied when RunTaskIDs is nil",
			scope:       zeroScope,
			body:        rpcBody(t, 9, "tools/call", "run_task", map[string]any{"id": "repo/anything"}),
			wantAllowed: false,
		},
		{
			name:        "run_task allowed for any id when RunTaskIDs contains *",
			scope:       wildcardScope,
			body:        rpcBody(t, 10, "tools/call", "run_task", map[string]any{"id": "repo/anything"}),
			wantAllowed: true,
		},
		{
			name:        "list_sources always allowed regardless of scope",
			scope:       zeroScope,
			body:        rpcBody(t, 11, "tools/call", "list_sources", map[string]any{}),
			wantAllowed: true,
		},
		{
			name:        "switch_dev_mode always allowed regardless of scope",
			scope:       zeroScope,
			body:        rpcBody(t, 12, "tools/call", "switch_dev_mode", map[string]any{"source": "x", "enabled": true}),
			wantAllowed: true,
		},
		{
			name:        "test_task always allowed regardless of scope",
			scope:       zeroScope,
			body:        rpcBody(t, 13, "tools/call", "test_task", map[string]any{"id": "repo/x"}),
			wantAllowed: true,
		},
		{
			name:        "malformed non-JSON-RPC body passes through",
			scope:       zeroScope,
			body:        []byte("not json"),
			wantAllowed: true,
		},
		{
			name:        "unknown method passes through",
			scope:       zeroScope,
			body:        rpcBody(t, 14, "some/other-method", "", nil),
			wantAllowed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed, _, msg := mcpScopeCheck(tt.scope, tt.body)
			if allowed != tt.wantAllowed {
				t.Errorf("mcpScopeCheck() allowed = %v, want %v (msg: %q)", allowed, tt.wantAllowed, msg)
			}
			if !allowed && msg == "" {
				t.Error("expected a non-empty denial message")
			}
		})
	}
}

// TestMcpScopeCheck_EchoesRequestID pins that a denial echoes back the
// caller's JSON-RPC id, not null, so the client can correlate the error.
func TestMcpScopeCheck_EchoesRequestID(t *testing.T) {
	body := rpcBody(t, "my-id", "tools/call", "run_task", map[string]any{"id": "repo/x"})
	allowed, id, _ := mcpScopeCheck(&pkgruntime.MCPScope{}, body)
	if allowed {
		t.Fatal("expected denial")
	}
	if id != "my-id" {
		t.Errorf("id = %v, want %q", id, "my-id")
	}
}

// ── end-to-end handleMCP enforcement ────────────────────────────────────────

// TestHandleMCP_ScopedKey_DeniesDisallowedRunTask covers the full path: a
// scoped ephemeral key posting a run_task for an id outside its
// RunTaskIDs gets the -32001 JSON-RPC denial and never reaches the gateway
// (the gateway has no /hooks/mcp handler registered in this test server, so
// a request that DID reach it would 404 with a plain-text body instead of
// the JSON-RPC envelope).
func TestHandleMCP_ScopedKey_DeniesDisallowedRunTask(t *testing.T) {
	srv, _, _ := newApprovalTestServer(t, true)
	h := srv.Handler()

	raw, _, err := srv.apiKeys.generateScoped(context.Background(), "scoped-mcp-key",
		&pkgruntime.MCPScope{RunTaskIDs: []string{"repo/allowed"}})
	if err != nil {
		t.Fatalf("generateScoped: %v", err)
	}

	body := rpcBody(t, 1, "tools/call", "run_task", map[string]any{"id": "repo/forbidden"})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, w.Body.String())
	}
	if resp.Error == nil {
		t.Fatalf("expected a JSON-RPC error, got: %s", w.Body.String())
	}
	if resp.Error.Code != -32001 {
		t.Errorf("error code = %d, want -32001", resp.Error.Code)
	}
}

// TestHandleMCP_ScopedKey_AllowsPermittedRunTask covers the positive path:
// a run_task for an id the scope permits is forwarded to the gateway rather
// than denied inline. The test server's gateway has no /hooks/mcp route
// registered, so a forwarded request 404s with a plain-text body — the
// signal that enforcement let it through instead of answering itself.
func TestHandleMCP_ScopedKey_AllowsPermittedRunTask(t *testing.T) {
	srv, _, _ := newApprovalTestServer(t, true)
	h := srv.Handler()

	raw, _, err := srv.apiKeys.generateScoped(context.Background(), "scoped-mcp-key",
		&pkgruntime.MCPScope{RunTaskIDs: []string{"repo/allowed"}})
	if err != nil {
		t.Fatalf("generateScoped: %v", err)
	}

	body := rpcBody(t, 1, "tools/call", "run_task", map[string]any{"id": "repo/allowed"})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (forwarded to an empty gateway): %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "-32001") {
		t.Errorf("request was denied inline instead of forwarded: %s", w.Body.String())
	}
}

// TestHandleMCP_UnscopedKey_AlwaysForwarded covers operator/CLI/dashboard
// keys (scopeFor returns nil): even a run_task call is forwarded unchanged,
// never denied at this layer.
func TestHandleMCP_UnscopedKey_AlwaysForwarded(t *testing.T) {
	srv, _, _ := newApprovalTestServer(t, true)
	h := srv.Handler()

	raw, _, err := srv.apiKeys.generate(context.Background(), "operator-key")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	body := rpcBody(t, 1, "tools/call", "run_task", map[string]any{"id": "repo/anything"})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (forwarded to an empty gateway): %s", w.Code, w.Body.String())
	}
}
