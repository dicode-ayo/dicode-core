package ipc

import (
	"context"
	"encoding/json"
)

// handshakeReq is the first message sent by the client after connecting.
type handshakeReq struct {
	Token string `json:"token"`
}

// handshakeResp is the server's reply to a successful handshake. In
// addition to the protocol version and capability list, the server echoes
// the TaskID and RunID of the context that accepted the connection so the
// shim can expose them as dicode.task_id / dicode.run_id.
//
// These fields are intentionally NOT omitempty: task code uses task_id as
// its self-identity for operations like tool-recursion guards, and an
// empty task_id silently disables those guards. Forcing the wire to carry
// the value every time makes "missing task_id" a loud, detectable event.
// The CLI control channel fills both with "" — the task-side shim treats
// an empty task_id as a hard error.
type handshakeResp struct {
	Proto  int      `json:"proto"`
	Caps   []string `json:"caps"`
	TaskID string   `json:"task_id"`
	RunID  string   `json:"run_id"`
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

	// Secret/SecretMap (issue #119): when Secret is true, ContentType/
	// Content/Data are ignored and SecretMap (a flat map[string]string)
	// carries the resolved provider response. Values feed the run-log
	// redactor + the resolver awaiting the provider's run.
	Secret    bool            `json:"secret,omitempty"`
	SecretMap json.RawMessage `json:"secretMap,omitempty"`

	// dicode.*
	TaskID  string          `json:"taskID,omitempty"`
	Limit   int             `json:"limit,omitempty"`
	Section string          `json:"section,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`

	// mcp.*
	MCPName string          `json:"mcpName,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`

	// dicode.oauth.* — exposed only to the auth-relay/auth-start/auth-providers built-in tasks
	Provider  string          `json:"provider,omitempty"`  // oauth.build_auth_url
	Scope     string          `json:"scope,omitempty"`     // oauth.build_auth_url — optional scope override
	Envelope  json.RawMessage `json:"envelope,omitempty"`  // oauth.store_token — OAuthTokenDeliveryPayload JSON
	Providers []string        `json:"providers,omitempty"` // oauth.list_status — caller-supplied provider keys

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

	// dicode.runs.* — run-input retention management (#233)
	BeforeTs int64 `json:"before_ts,omitempty"` // dicode.runs.list_expired: unix timestamp cutoff

	// dicode.sources.set_dev_mode — toggles dev mode on a configured source (#234)
	Name      string `json:"name,omitempty"`       // source name
	Enabled   bool   `json:"enabled,omitempty"`    // true to enable
	LocalPath string `json:"local_path,omitempty"` // local-path mode
	Branch    string `json:"branch,omitempty"`     // clone-mode branch (also reused by git.commit_push)
	Base      string `json:"base,omitempty"`       // clone-mode base branch
	DevRunID  string `json:"run_id,omitempty"`     // clone-mode per-fix run ID

	// dicode.git.commit_push — add/commit/push in a source's repo (#234)
	SourceID     string   `json:"source_id,omitempty"`      // source name to resolve repo path
	CommitMsg    string   `json:"commit_message,omitempty"` // commit message
	BranchPrefix string   `json:"branch_prefix,omitempty"`  // branch must start with this
	AllowMain    bool     `json:"allow_main,omitempty"`     // bypass branch-prefix check
	Files        []string `json:"files,omitempty"`          // paths to git-add; empty = all tracked
	AuthorName   string   `json:"author_name,omitempty"`    // commit author name
	AuthorEmail  string   `json:"author_email,omitempty"`   // commit author email
	AuthTokenEnv string   `json:"auth_token_env,omitempty"` // env var holding HTTPS auth token

	// cli.ai — prompt, optional session_id, optional task id override.
	Prompt    string `json:"prompt,omitempty"`
	SessionID string `json:"sessionID,omitempty"`
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
	ActiveTaskSlots() int
	MaxConcurrentTasks() int
	WaitingTasks() int
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

// AIResult is the cli.ai response. reply is the text surfaced to the user;
// session_id is echoed back so the CLI can persist it for follow-up turns.
// TaskID is the task id that was actually fired — useful in case the caller
// asked for the configured default and wants to know what ran.
type AIResult struct {
	TaskID    string `json:"taskID"`
	RunID     string `json:"runID"`
	SessionID string `json:"sessionID"`
	Reply     string `json:"reply"`
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
		ActiveTasks        int     `json:"active_tasks"`
		ChildRSSMB         float64 `json:"children_rss_mb,omitempty"`
		ChildCPUMs         *int64  `json:"children_cpu_ms,omitempty"`
		ActiveTaskSlots    int     `json:"active_task_slots"`
		MaxConcurrentTasks int     `json:"max_concurrent_tasks"`
		WaitingTasks       int     `json:"waiting_tasks"`
	} `json:"tasks"`
}
