package ipc

import (
	"context"
	"encoding/json"
)

// handshakeReq is the first message sent by the client after connecting.
type handshakeReq struct {
	Token string `json:"token"`
}

// handshakeResp is the server's reply to a successful handshake.
type handshakeResp struct {
	Proto int      `json:"proto"`
	Caps  []string `json:"caps"`
}

// handshakeErr is the server's reply when the handshake fails. The server
// closes the connection immediately after sending this.
type handshakeErr struct {
	Error string `json:"error"`
}

// Request is an inbound message from a connected client.
type Request struct {
	ID string `json:"id,omitempty"` // absent → fire-and-forget

	Method string `json:"method"`

	// log
	Level   string `json:"level,omitempty"`
	Message string `json:"message,omitempty"`

	// kv.* and return — both use "value" in the JSON payload
	Key    string          `json:"key,omitempty"`
	Value  json.RawMessage `json:"value,omitempty"`
	Prefix string          `json:"prefix,omitempty"`

	// output
	ContentType string          `json:"contentType,omitempty"`
	Content     string          `json:"content,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`

	// dicode.*
	TaskID  string          `json:"taskID,omitempty"`
	Limit   int             `json:"limit,omitempty"`
	Section string          `json:"section,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`

	// mcp.*
	MCPName string          `json:"mcpName,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`

	// http.register (daemon tasks register an HTTP pattern with the gateway)
	Pattern  string `json:"pattern,omitempty"`
	StreamID string `json:"streamID,omitempty"`

	// http.respond (task sends HTTP response back to a pending gateway request)
	RequestID   string            `json:"requestID,omitempty"`
	Status      int               `json:"status,omitempty"`
	RespHeaders map[string]string `json:"respHeaders,omitempty"`
	RespBody    []byte            `json:"respBody,omitempty"` // base64-encoded in JSON

	// cli.* (control socket — CLI client commands)
	RunID       string `json:"runID,omitempty"`
	StringValue string `json:"stringValue,omitempty"` // cli.secrets.set value
	Follow      bool   `json:"follow,omitempty"`      // cli.logs — reserved for streaming
}

// Response is an outbound message to a connected client.
type Response struct {
	ID     string `json:"id"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// OutputResult is a structured output produced by a task via the output.* API.
type OutputResult struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
	Data        any    `json:"data,omitempty"`
}

// IsSet reports whether any output was recorded.
func (o *OutputResult) IsSet() bool { return o != nil && o.ContentType != "" }

// EngineRunner allows the IPC server to fire and await task runs, and to
// query live concurrency state. Implemented by the trigger engine; injected
// to avoid import cycles.
type EngineRunner interface {
	FireManual(ctx context.Context, taskID string, params map[string]string) (string, error)
	WaitRun(ctx context.Context, runID string) (RunResult, error)
	ActiveRunCount() int
}

// HTTPInboundRequest is a server-initiated push to a daemon task that has
// registered an HTTP pattern via http.register. The task responds by sending
// an http.respond Request with the matching RequestID.
type HTTPInboundRequest struct {
	RequestID  string            `json:"requestID"`
	HTTPMethod string            `json:"httpMethod"`
	Path       string            `json:"path"`
	Query      string            `json:"query,omitempty"`
	ReqHeaders map[string]string `json:"reqHeaders,omitempty"`
	ReqBody    []byte            `json:"reqBody,omitempty"` // base64-encoded in JSON
}

// TaskSummary is a single row in the cli.list response.
type TaskSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Trigger     string `json:"trigger"`    // "manual" | "cron:..." | "webhook:..." | "daemon"
	LastStatus  string `json:"lastStatus"` // "success" | "failure" | "running" | ""
	LastRunID   string `json:"lastRunID"`  // "" if never run
	LastRunAt   string `json:"lastRunAt"`  // RFC3339 or ""
}

// LogEntry is one log line returned by cli.logs.
type LogEntry struct {
	RunID     string `json:"runID"`
	Timestamp string `json:"timestamp"` // RFC3339
	Level     string `json:"level"`
	Message   string `json:"message"`
}

// DaemonStatus is the cli.status response.
type DaemonStatus struct {
	Version   string `json:"version"`
	UptimeSec int64  `json:"uptimeSec"`
	TaskCount int    `json:"taskCount"`
	RunCount  int    `json:"runCount"` // runs in the last 24h
}

// RunResult is returned by EngineRunner.WaitRun.
type RunResult struct {
	RunID       string `json:"runID"`
	Status      string `json:"status"`
	ReturnValue any    `json:"returnValue"`
}

// MetricsSnapshot is the cli.metrics response.
// Fields sourced from /proc are omitted on non-Linux platforms.
type MetricsSnapshot struct {
	Daemon struct {
		HeapAllocMB float64 `json:"heap_alloc_mb"`
		HeapSysMB   float64 `json:"heap_sys_mb"`
		Goroutines  int     `json:"goroutines"`
		CPUMs       *int64  `json:"cpu_ms,omitempty"`
	} `json:"daemon"`
	Tasks struct {
		ActiveTasks int     `json:"active_tasks"`
		ChildRSSMB  float64 `json:"children_rss_mb,omitempty"`
		ChildCPUMs  *int64  `json:"children_cpu_ms,omitempty"`
	} `json:"tasks"`
}
