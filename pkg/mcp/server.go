// Package mcp exposes dicode capabilities as MCP (Model Context Protocol) tools.
// Any MCP-capable agent (Claude Code, Cursor, custom agents) can connect to
// the endpoint at /mcp and use these tools to develop, test, and deploy tasks.
//
// The server is a thin adapter over the existing registry, runner, secrets
// chain, and source manager — no business logic lives here.
package mcp

import (
	"context"
	"net/http"
)

// Server is the MCP server. Mount it with Handler() on your HTTP router.
type Server struct {
	// deps injected at construction — filled in when components are implemented
}

// New constructs an MCP server. Dependencies are injected via options as
// each subsystem is implemented.
func New() *Server {
	return &Server{}
}

// Handler returns an http.Handler that serves the MCP protocol at the
// mounted path (e.g. /mcp).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// TODO: register MCP tool handlers as subsystems are implemented:
	//   list_tasks, get_task, get_js_api, get_example_tasks
	//   list_secrets, write_task_file
	//   validate_task, test_task, dry_run_task
	//   run_task, get_run_log, commit_task
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"dicode","version":"dev","protocol":"mcp"}`))
	})
	return mux
}

// Shutdown gracefully stops the MCP server.
func (s *Server) Shutdown(_ context.Context) error { return nil }
