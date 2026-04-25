// Package mcp exposes dicode capabilities as MCP (Model Context Protocol) tools.
// Any MCP-capable agent (Claude Code, Cursor, custom agents) can connect to
// the endpoint at /mcp and use these tools to develop, test, and deploy tasks.
//
// Transport: JSON-RPC 2.0 over HTTP POST. Protocol plumbing is delegated to
// github.com/mark3labs/mcp-go; this file only declares tools and wires their
// handlers to dicode internals (registry, source manager, tasktest).
package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/tasktest"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
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
	mcp       *mcpserver.MCPServer
}

// New constructs an MCP server with the given registry and optional source manager.
func New(reg *registry.Registry, sourceMgr SourceLister) *Server {
	s := &Server{
		registry:  reg,
		sourceMgr: sourceMgr,
		mcp: mcpserver.NewMCPServer(
			"dicode", "dev",
			mcpserver.WithToolCapabilities(false),
		),
	}
	s.registerTools()
	return s
}

// Handler returns an http.Handler that serves MCP JSON-RPC 2.0 at the mounted path.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.handleHTTP)
}

// Shutdown gracefully stops the MCP server.
func (s *Server) Shutdown(_ context.Context) error { return nil }

// handleHTTP wraps mcp-go's request handler in dicode's plain JSON-RPC-over-HTTP
// transport. mcp-go's bundled SSE/streamable HTTP servers are richer but the
// existing dicode CLI consumer speaks plain POST — keeping the transport
// surface stable avoids a synchronized client/server upgrade.
func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method == http.MethodGet {
		// Convenience: GET /mcp returns server info for liveness probes.
		_ = json.NewEncoder(w).Encode(map[string]string{
			"name":     "dicode",
			"version":  "dev",
			"protocol": mcp.LATEST_PROTOCOL_VERSION,
		})
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	resp := s.mcp.HandleMessage(r.Context(), body)
	_ = json.NewEncoder(w).Encode(resp)
}

// registerTools wires every dicode capability into the mcp-go tool registry.
// New tools go here; the handler functions below them carry the actual logic.
func (s *Server) registerTools() {
	s.mcp.AddTool(
		mcp.NewTool("list_tasks",
			mcp.WithDescription("List all registered tasks with their IDs, trigger types, and last run status."),
		),
		s.toolListTasks,
	)
	s.mcp.AddTool(
		mcp.NewTool("list_sources",
			mcp.WithDescription("List all configured task sources (git, local, taskset) with their current dev mode state."),
		),
		s.toolListSources,
	)
	s.mcp.AddTool(
		mcp.NewTool("switch_dev_mode",
			mcp.WithDescription("Enable or disable dev mode for a named taskset source. When enabled with a local_path, dicode resolves tasks from that local directory instead of the git ref — ideal for developing new tasks locally."),
			mcp.WithString("source",
				mcp.Required(),
				mcp.Description("Source name (from list_sources)"),
			),
			mcp.WithBoolean("enabled",
				mcp.Required(),
				mcp.Description("true to enable dev mode, false to disable"),
			),
			mcp.WithString("local_path",
				mcp.Description("Absolute path to a local taskset.yaml file. Required when enabling dev mode without a dev_ref configured on the source."),
			),
		),
		s.toolSwitchDevMode,
	)
	s.mcp.AddTool(
		mcp.NewTool("run_task",
			mcp.WithDescription("Trigger a manual run of a task by ID."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Namespaced task ID, e.g. infra/backend/deploy")),
		),
		s.toolRunTask,
	)
	s.mcp.AddTool(
		mcp.NewTool("get_task",
			mcp.WithDescription("Get the full spec and last run info for a task."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Namespaced task ID")),
		),
		s.toolGetTask,
	)
	s.mcp.AddTool(
		mcp.NewTool("test_task",
			mcp.WithDescription("Run the task's sibling test file (task.test.ts / .js / .mjs) through its runtime and return structured pass/fail counts plus the full stdout+stderr. Deno runtime only for now; other runtimes return a clear 'not yet supported' error."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Namespaced task ID (same as list_tasks returns)")),
		),
		s.toolTestTask,
	)
}

// --- Tool implementations ---

func (s *Server) toolListTasks(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.registry == nil {
		return mcp.NewToolResultText("No registry available."), nil
	}
	specs := s.registry.All()
	if len(specs) == 0 {
		return mcp.NewToolResultText("No tasks registered."), nil
	}
	b, _ := json.MarshalIndent(specs, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

func (s *Server) toolListSources(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.sourceMgr == nil {
		return mcp.NewToolResultText("Source manager not available."), nil
	}
	entries := s.sourceMgr.List()
	b, _ := json.MarshalIndent(entries, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

func (s *Server) toolSwitchDevMode(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	source, err := req.RequireString("source")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	enabled, err := req.RequireBool("enabled")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	localPath := ""
	if v, ok := req.GetArguments()["local_path"].(string); ok {
		localPath = v
	}
	if s.sourceMgr == nil {
		return mcp.NewToolResultError("source manager not available"), nil
	}
	if err := s.sourceMgr.SetDevMode(ctx, source, enabled, localPath); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	msg := "Dev mode " + onOff(enabled) + " for source \"" + source + "\""
	if localPath != "" {
		msg += " (local path: " + localPath + ")"
	}
	return mcp.NewToolResultText(msg), nil
}

func (s *Server) toolRunTask(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if s.registry == nil {
		return mcp.NewToolResultError("registry not available"), nil
	}
	if _, ok := s.registry.Get(id); !ok {
		return mcp.NewToolResultError("task \"" + id + "\" not found"), nil
	}
	return mcp.NewToolResultText("Use POST /api/tasks/" + id + "/run to trigger this task."), nil
}

func (s *Server) toolGetTask(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if s.registry == nil {
		return mcp.NewToolResultError("registry not available"), nil
	}
	spec, ok := s.registry.Get(id)
	if !ok {
		return mcp.NewToolResultError("task \"" + id + "\" not found"), nil
	}
	b, _ := json.MarshalIndent(spec, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

func (s *Server) toolTestTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if s.registry == nil {
		return mcp.NewToolResultError("registry not available"), nil
	}
	spec, ok := s.registry.Get(id)
	if !ok {
		return mcp.NewToolResultError("task \"" + id + "\" not found"), nil
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
	return mcp.NewToolResultText(res.Output + "\n\n--- summary ---\n" + string(b) + "\n"), nil
}

// --- helpers ---

func onOff(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}
