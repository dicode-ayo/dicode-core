// Package mcp exposes dicode capabilities as MCP (Model Context Protocol) tools.
// Any MCP-capable agent (Claude Code, Cursor, custom agents) can connect to
// the endpoint at /mcp and use these tools to develop, test, and deploy tasks.
//
// Protocol: JSON-RPC 2.0 over HTTP POST (stateless request/response).
// Clients send a single JSON-RPC message; the server replies synchronously.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/tasktest"
)

// SourceLister is satisfied by *webui.SourceManager.
// Defined here to avoid an import cycle (mcp ← webui ← mcp).
type SourceLister interface {
	// List returns the current source list with dev mode state.
	List() []SourceEntry
	// SetDevMode toggles dev mode for a named source.
	SetDevMode(ctx context.Context, name string, enabled bool, localPath string) error
}

// SourceEntry is the minimal source representation used by MCP tools.
type SourceEntry struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	URL     string `json:"url,omitempty"`
	Path    string `json:"path,omitempty"`
	Branch  string `json:"branch,omitempty"`
	DevMode bool   `json:"dev_mode"`
	DevPath string `json:"dev_path,omitempty"`

	// Pull-health fields — populated for live taskset git sources; nil
	// for local sources or taskset sources that haven't attempted a pull
	// yet. The frontend uses these to render a per-source status dot in
	// the task list. See #87.
	//
	// LastPullAt is a pointer because `time.Time` + `omitempty` does NOT
	// omit the zero value — it serializes as "0001-01-01T00:00:00Z",
	// which is truthy in JS and causes the frontend to render a spurious
	// dot for every local / never-pulled source.
	LastPullAt    *time.Time `json:"last_pull_at,omitempty"`
	LastPullOK    bool       `json:"last_pull_ok,omitempty"`
	LastPullError string     `json:"last_pull_error,omitempty"`
}

// Server is the MCP server. Mount it with Handler() on your HTTP router.
type Server struct {
	registry  *registry.Registry
	sourceMgr SourceLister // nil when not wired
}

// New constructs an MCP server with the given registry and optional source manager.
func New(reg *registry.Registry, sourceMgr SourceLister) *Server {
	return &Server{registry: reg, sourceMgr: sourceMgr}
}

// Handler returns an http.Handler that serves MCP JSON-RPC 2.0 at the mounted path.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRPC)
	return mux
}

// Shutdown gracefully stops the MCP server.
func (s *Server) Shutdown(_ context.Context) error { return nil }

// --- JSON-RPC 2.0 types ---

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func rpcOK(id any, result any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func rpcErr(id any, code int, msg string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// handleRPC is the single JSON-RPC 2.0 dispatcher.
func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method == http.MethodGet {
		// Convenience: GET /mcp returns server info.
		json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
			"name":     "dicode",
			"version":  "dev",
			"protocol": "mcp/2024-11-05",
		})
		return
	}

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(rpcErr(nil, -32700, "parse error")) //nolint:errcheck
		return
	}

	var resp rpcResponse
	switch req.Method {
	case "initialize":
		resp = s.methodInitialize(req)
	case "tools/list":
		resp = s.methodToolsList(req)
	case "tools/call":
		resp = s.methodToolsCall(r.Context(), req)
	default:
		resp = rpcErr(req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}

	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}

// --- MCP method handlers ---

func (s *Server) methodInitialize(req rpcRequest) rpcResponse {
	return rpcOK(req.ID, map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "dicode", "version": "dev"},
	})
}

// tool describes one MCP tool for the tools/list response.
type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func (s *Server) methodToolsList(req rpcRequest) rpcResponse {
	tools := []tool{
		{
			Name:        "list_tasks",
			Description: "List all registered tasks with their IDs, trigger types, and last run status.",
			InputSchema: jsonSchema(map[string]any{}),
		},
		{
			Name:        "list_sources",
			Description: "List all configured task sources (git, local, taskset) with their current dev mode state.",
			InputSchema: jsonSchema(map[string]any{}),
		},
		{
			Name:        "switch_dev_mode",
			Description: "Enable or disable dev mode for a named taskset source. When enabled with a local_path, dicode resolves tasks from that local directory instead of the git ref — ideal for developing new tasks locally.",
			InputSchema: jsonSchema(map[string]any{
				"source":     map[string]any{"type": "string", "description": "Source name (from list_sources)"},
				"enabled":    map[string]any{"type": "boolean", "description": "true to enable dev mode, false to disable"},
				"local_path": map[string]any{"type": "string", "description": "Absolute path to a local taskset.yaml file. Required when enabling dev mode without a dev_ref configured on the source."},
			}, "source", "enabled"),
		},
		{
			Name:        "run_task",
			Description: "Trigger a manual run of a task by ID.",
			InputSchema: jsonSchema(map[string]any{
				"id": map[string]any{"type": "string", "description": "Namespaced task ID, e.g. infra/backend/deploy"},
			}, "id"),
		},
		{
			Name:        "get_task",
			Description: "Get the full spec and last run info for a task.",
			InputSchema: jsonSchema(map[string]any{
				"id": map[string]any{"type": "string", "description": "Namespaced task ID"},
			}, "id"),
		},
		{
			Name:        "test_task",
			Description: "Run the task's sibling test file (task.test.ts / .js / .mjs) through its runtime and return structured pass/fail counts plus the full stdout+stderr. Deno runtime only for now; other runtimes return a clear 'not yet supported' error.",
			InputSchema: jsonSchema(map[string]any{
				"id": map[string]any{"type": "string", "description": "Namespaced task ID (same as list_tasks returns)"},
			}, "id"),
		},
	}
	return rpcOK(req.ID, map[string]any{"tools": tools})
}

func (s *Server) methodToolsCall(ctx context.Context, req rpcRequest) rpcResponse {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return rpcErr(req.ID, -32602, "invalid params: "+err.Error())
	}

	switch params.Name {
	case "list_tasks":
		return s.toolListTasks(req.ID)
	case "list_sources":
		return s.toolListSources(req.ID)
	case "switch_dev_mode":
		return s.toolSwitchDevMode(ctx, req.ID, params.Arguments)
	case "run_task":
		return s.toolRunTask(ctx, req.ID, params.Arguments)
	case "get_task":
		return s.toolGetTask(req.ID, params.Arguments)
	case "test_task":
		return s.toolTestTask(ctx, req.ID, params.Arguments)
	default:
		return rpcErr(req.ID, -32601, fmt.Sprintf("unknown tool: %s", params.Name))
	}
}

// --- Tool implementations ---

func (s *Server) toolListTasks(id any) rpcResponse {
	if s.registry == nil {
		return textResult(id, "No registry available.")
	}
	specs := s.registry.All()
	if len(specs) == 0 {
		return textResult(id, "No tasks registered.")
	}
	b, _ := json.MarshalIndent(specs, "", "  ")
	return textResult(id, string(b))
}

func (s *Server) toolListSources(id any) rpcResponse {
	if s.sourceMgr == nil {
		return textResult(id, "Source manager not available.")
	}
	entries := s.sourceMgr.List()
	b, _ := json.MarshalIndent(entries, "", "  ")
	return textResult(id, string(b))
}

func (s *Server) toolSwitchDevMode(ctx context.Context, id any, raw json.RawMessage) rpcResponse {
	var args struct {
		Source    string `json:"source"`
		Enabled   bool   `json:"enabled"`
		LocalPath string `json:"local_path"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return rpcErr(id, -32602, "invalid arguments: "+err.Error())
	}
	if args.Source == "" {
		return rpcErr(id, -32602, "source is required")
	}
	if s.sourceMgr == nil {
		return rpcErr(id, -32603, "source manager not available")
	}
	if err := s.sourceMgr.SetDevMode(ctx, args.Source, args.Enabled, args.LocalPath); err != nil {
		return rpcErr(id, -32603, err.Error())
	}
	msg := fmt.Sprintf("Dev mode %s for source %q", onOff(args.Enabled), args.Source)
	if args.LocalPath != "" {
		msg += fmt.Sprintf(" (local path: %s)", args.LocalPath)
	}
	return textResult(id, msg)
}

func (s *Server) toolRunTask(ctx context.Context, id any, raw json.RawMessage) rpcResponse {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return rpcErr(id, -32602, "invalid arguments: "+err.Error())
	}
	if args.ID == "" {
		return rpcErr(id, -32602, "id is required")
	}
	if s.registry == nil {
		return rpcErr(id, -32603, "registry not available")
	}
	if _, ok := s.registry.Get(args.ID); !ok {
		return rpcErr(id, -32603, fmt.Sprintf("task %q not found", args.ID))
	}
	return textResult(id, fmt.Sprintf("Use POST /api/tasks/%s/run to trigger this task.", args.ID))
}

func (s *Server) toolGetTask(id any, raw json.RawMessage) rpcResponse {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return rpcErr(id, -32602, "invalid arguments: "+err.Error())
	}
	if args.ID == "" {
		return rpcErr(id, -32602, "id is required")
	}
	if s.registry == nil {
		return rpcErr(id, -32603, "registry not available")
	}
	spec, ok := s.registry.Get(args.ID)
	if !ok {
		return rpcErr(id, -32603, fmt.Sprintf("task %q not found", args.ID))
	}
	b, _ := json.MarshalIndent(spec, "", "  ")
	return textResult(id, string(b))
}

func (s *Server) toolTestTask(ctx context.Context, id any, raw json.RawMessage) rpcResponse {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return rpcErr(id, -32602, "invalid arguments: "+err.Error())
	}
	if args.ID == "" {
		return rpcErr(id, -32602, "id is required")
	}
	if s.registry == nil {
		return rpcErr(id, -32603, "registry not available")
	}
	spec, ok := s.registry.Get(args.ID)
	if !ok {
		return rpcErr(id, -32603, fmt.Sprintf("task %q not found", args.ID))
	}
	res, runErr := tasktest.Run(ctx, spec)
	// Return structured JSON alongside the output text so MCP clients can
	// read counts programmatically without having to re-parse the stdout.
	payload := map[string]any{
		"taskID":     res.TaskID,
		"runtime":    res.Runtime,
		"testFile":   res.TestFile,
		"passed":     res.Passed,
		"failed":     res.Failed,
		"skipped":    res.Skipped,
		"durationMs": res.Duration.Milliseconds(),
		"exitCode":   res.ExitCode,
	}
	if res.Error != "" {
		payload["error"] = res.Error
	} else if runErr != nil {
		payload["error"] = runErr.Error()
	}
	b, _ := json.MarshalIndent(payload, "", "  ")
	text := fmt.Sprintf("%s\n\n--- summary ---\n%s\n", res.Output, string(b))
	return textResult(id, text)
}

// --- helpers ---

func textResult(id any, text string) rpcResponse {
	return rpcOK(id, map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	})
}

func jsonSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func onOff(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}
