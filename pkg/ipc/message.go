package ipc

import (
	"context"
	"encoding/json"

	"github.com/dicode/dicode/pkg/audit"
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
// These fields are NOT omitempty: task code uses task_id as
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

	// MCPContext signals that this IPC call originates from the MCP endpoint.
	// When true, dicode.list_tasks filters to tasks with mcp_exposed: true,
	// and dicode.run_task rejects tasks that are not MCP-exposed.
	MCPContext bool `json:"mcpContext,omitempty"`

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
	WaitMs      int    `json:"waitMs,omitempty"`      // cli.ready — max ms to block for readiness (0 = probe only)

	// dicode.runs.* — run-input retention management (#233)
	BeforeTs int64 `json:"before_ts,omitempty"` // dicode.runs.list_expired: unix timestamp cutoff

	// dicode.set_group — free-text label for the caller's run (#116).
	Group string `json:"group,omitempty"`

	// dicode.suspend — pause the run and collect user input (#512). State is
	// the opaque JSON blob to rehydrate on resume; Schema is the JSON Schema
	// (draft 2020-12) the submission is validated against; Deadline is an
	// optional Unix-ms TTL (0 = unset, engine applies its default). Kept as raw
	// JSON so the runtime threads the blobs through without re-encoding.
	State    json.RawMessage `json:"state,omitempty"`
	Schema   json.RawMessage `json:"schema,omitempty"`
	Deadline int64           `json:"deadline,omitempty"`

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

	// cli.task.{create,edit,save,cancel} — AI-first task authoring.
	// TaskName is the create verb's task name. TaskID/SessionID/Prompt are
	// reused from the fields above.
	TaskName string `json:"taskName,omitempty"`

	// Source names the owning source: the create verb's target, or the
	// delete verb's ID-prefix-resolution override. Force skips the delete
	// verb's dangling-reference / confirmation guard.
	Source string `json:"source,omitempty"`
	Force  bool   `json:"force,omitempty"`

	// dicode.crypto.{encrypt, decrypt} inputs
	Context       string `json:"context,omitempty"`
	PlaintextB64  string `json:"plaintext_b64,omitempty"`
	CiphertextB64 string `json:"ciphertext_b64,omitempty"`

	// dicode.audit.query inputs (#415). After is an opaque resume cursor
	// (from a prior response's next_cursor); Order is "asc" or "desc"
	// (default desc); Actor/EventType filter alongside TaskID and Limit
	// reused from the fields above.
	After     string `json:"after,omitempty"`
	Order     string `json:"order,omitempty"`
	Actor     string `json:"actor,omitempty"`
	EventType string `json:"event_type,omitempty"`
}

// AuditQueryResult is the dicode.audit.query response: the matched events
// plus the opaque cursor to resume after the last one.
type AuditQueryResult struct {
	Events     []audit.Event `json:"events"`
	NextCursor string        `json:"next_cursor"`
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

// SuspendResult captures a dicode.suspend() request (#512): the opaque state
// blob to rehydrate on resume, the JSON Schema the submission is validated
// against, and an optional Unix-ms deadline (0 = unset). State and Schema are
// kept as raw JSON so the runtime forwards them to the engine unchanged.
type SuspendResult struct {
	State    json.RawMessage `json:"state,omitempty"`
	Schema   json.RawMessage `json:"schema,omitempty"`
	Deadline int64           `json:"deadline,omitempty"`
}

// EngineRunner allows the IPC server to fire and await task runs, and to
// query live concurrency state. Implemented by the trigger engine; injected
// to avoid import cycles.
type EngineRunner interface {
	FireManual(ctx context.Context, taskID string, params map[string]string) (string, error)
	// FireFromTask is FireManual with a parent run ID — used by dicode.run_task
	// so the IPC server can stamp the caller's run ID on the new run (#116).
	FireFromTask(ctx context.Context, taskID, parentRunID string, params map[string]string) (string, error)
	WaitRun(ctx context.Context, runID string) (RunResult, error)
	// WaitRunSettled stops at a suspended run instead of following its resume
	// chain. The CLI needs this to render the resume form; dicode.run_task wants
	// WaitRun's "block until genuinely terminal" contract.
	WaitRunSettled(ctx context.Context, runID string) (RunResult, error)
	// KillRun cancels an in-flight run by id, e.g. the CLI's Ctrl+C-during-a-turn
	// cancellation (cli.run.cancel). Returns false when the run is not currently
	// cancellable (already terminal, or the id is unknown).
	KillRun(runID string) bool
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

// StatusCrashLooping is the synthesized task-level status reported by
// cli.list / cli.status for a daemon task the trigger engine has flagged as
// crash-looping (issue #458). It replaces whatever the latest run row says —
// notably the transient "running" of a spawn that is about to die — so a
// hard-failing daemon never samples as healthy. Mirrors the wire value of
// trigger.DaemonCrashLooping; duplicated here because pkg/trigger imports
// pkg/ipc, so this package cannot import the constant.
const StatusCrashLooping = "crashlooping"

// CrashloopReporter is an optional extension of EngineRunner. The trigger
// engine implements it; test fakes may not. The control server type-asserts
// for it when deriving the displayed status of a task, so crash-loop
// surfacing degrades gracefully to plain last-run status when absent.
type CrashloopReporter interface {
	// IsCrashLooping reports whether the daemon task's body has failed
	// several consecutive starts and is currently in a spawn/crash/backoff
	// loop. See pkg/trigger/crashloop.go for the exact detection rule.
	IsCrashLooping(taskID string) bool
}

// TaskSummary is a single row in the cli.list response.
type TaskSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Trigger     string `json:"trigger"`    // "manual" | "cron:..." | "webhook:..." | "daemon"
	LastStatus  string `json:"lastStatus"` // "success" | "failure" | "running" | "crashlooping" | ""
	LastRunID   string `json:"lastRunID"`  // "" if never run
	LastRunAt   string `json:"lastRunAt"`  // RFC3339 or ""
	// Pending is true when the approval gate is holding this task awaiting
	// approval — a state distinct from any run status. cli.task.pending carries
	// the detail (short content hash).
	Pending bool `json:"pending,omitempty"`
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
	// Ready reports whether the reconciler's first sync has completed —
	// i.e. the initial task inventory is registered and lookups by task ID
	// are meaningful (#464). Point-in-time snapshot; cli.ready is the
	// blocking wait.
	Ready bool `json:"ready"`
}

// ReadyResult is the cli.ready response. Ready is false when the daemon's
// first task sync had not completed within the requested wait window.
type ReadyResult struct {
	Ready bool `json:"ready"`
}

// RunResult is returned by EngineRunner.WaitRun.
type RunResult struct {
	RunID       string `json:"runID"`
	Status      string `json:"status"`
	ReturnValue any    `json:"returnValue"`
}

// APIKeyMintResult is the cli.api_keys.create response. The raw `Key` is
// returned exactly once — the daemon stores only its hash.
type APIKeyMintResult struct {
	Key       string `json:"key"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Prefix    string `json:"prefix"`
	CreatedAt int64  `json:"created_at"`
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
	// Suspended is true when a blank-prompt chat run paused awaiting the first
	// message. The CLI then drives the suspend/resume loop from RunID rather
	// than printing Reply.
	Suspended bool `json:"suspended,omitempty"`
}

// TaskCreateResult is the cli.task.create response. TaskID is the
// piped value the CLI prints to stdout; the rest is metadata. The SessionID /
// WebUIURL / Reply fields are populated only on the --ai path, where create
// chains straight into an edit session in one round-trip.
type TaskCreateResult struct {
	TaskID    string   `json:"taskID"`
	Source    string   `json:"source"`
	Files     []string `json:"files"`
	SessionID string   `json:"sessionID,omitempty"`
	WebUIURL  string   `json:"webuiURL,omitempty"`
	Reply     string   `json:"reply,omitempty"`
}

// TaskEditResult is the cli.task.edit response. Reply is the AI turn's
// text (stdout); SessionID and the source metadata go to stderr. WebUIURL is
// the page the user can open to continue the session in the browser.
type TaskEditResult struct {
	SessionID   string `json:"sessionID"`
	TaskID      string `json:"taskID"`
	Source      string `json:"source"`
	SourceKind  string `json:"sourceKind"`
	SandboxPath string `json:"sandboxPath"`
	WebUIURL    string `json:"webuiURL"`
	Reply       string `json:"reply"`
	// RunID is the AI turn's run id (#568) — set whenever a non-empty
	// Prompt actually fired a turn (success or suspended). Empty when the
	// prompt was blank, matching AIResult's shape for the same case.
	RunID string `json:"runID,omitempty"`
	// Suspended is true when the AI turn's run paused awaiting further
	// input (e.g. a dicode.suspend() call inside the underlying task).
	// Mirrors AIResult.Suspended — task-create doesn't use suspend for
	// clarification yet (that's Phase 1, docs/design/ai-task-authoring.md),
	// but the underlying ai-agent task is generic, so a suspending run must
	// still surface cleanly instead of hanging or erroring.
	Suspended bool `json:"suspended,omitempty"`
}

// TaskSaveResult is the cli.task.save response. TaskID or PRURL is the
// piped value (stdout); the rest is metadata.
type TaskSaveResult struct {
	SessionID string `json:"sessionID"`
	TaskID    string `json:"taskID"`
	PRURL     string `json:"prURL,omitempty"`
	Applied   bool   `json:"applied"`
}

// TaskCancelResult is the cli.task.cancel response.
type TaskCancelResult struct {
	SessionID string `json:"sessionID"`
	Cancelled bool   `json:"cancelled"`
}

// TaskDeleteResult is the cli.task.delete response.
//
// Mode is "local" when the task directory was removed in place, or "git"
// when the deletion was pushed to a branch and a PR was filed. For "git",
// PRRunID is the run id of the buildin/git-pr task that opened the PR and
// PRValue is whatever that task returned (typically the PR URL). Refs lists
// task ids that chain off the deleted task (on_failure / chain triggers) —
// surfaced so the CLI can warn before the deletion takes effect.
type TaskDeleteResult struct {
	TaskID  string   `json:"taskID"`
	Source  string   `json:"source"`
	Mode    string   `json:"mode"` // "preview" | "local" | "git"
	Trigger string   `json:"trigger,omitempty"`
	Branch  string   `json:"branch,omitempty"`
	PRRunID string   `json:"prRunID,omitempty"`
	PRValue string   `json:"prValue,omitempty"`
	Refs    []string `json:"refs,omitempty"`
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
