// Package ipc implements the dicode unified IPC protocol.
//
// All clients (task shims, CLI, WebUI daemon task) connect to a single
// Unix socket and present a capability token. The token is issued by the
// daemon at task-launch time and encodes the client identity plus the set
// of capabilities it is granted.
//
// Wire format: 4-byte little-endian length prefix + JSON payload.
//
// Handshake (first exchange on every new connection):
//
//	Client → {"token":"<signed-token>"}
//	Server → {"proto":1,"caps":["log","params.read",...]}   // success
//	Server → {"error":"invalid token"}                       // rejected → close
//
// Subsequent messages follow the normal request/response pattern:
//
//	Request  (needs reply): {"id":"1","method":"kv.get","key":"x"}
//	Request  (fire+forget): {"method":"log","level":"info","message":"hi"}
//	Response (success):     {"id":"1","result":{...}}
//	Response (error):       {"id":"1","error":"something went wrong"}
package ipc

import "github.com/dicode/dicode/pkg/task"

// Capability constants — used in token claims and capability checks.
const (
	// Task-shim capabilities (all task tokens include these by default).
	CapLog         = "log"
	CapParamsRead  = "params.read"
	CapInputRead   = "input.read"
	CapKVRead      = "kv.read"
	CapKVWrite     = "kv.write"
	CapOutputWrite = "output.write"
	CapReturn      = "return"
	// CapSetGroup allows a task to label its own run via dicode.set_group()
	// (#116). Self-affecting and granted to every task token by default; the
	// cap exists only so future denial policies can revoke it.
	CapSetGroup = "set_group"

	// CapOutputSecret allows a task to call dicode.output(map, {
	// secret: true }) — flagging values for daemon-side redaction and
	// (for provider tasks) routing to the env-resolver waiting on the
	// caller side. Granted to every task token by default; the cap exists
	// only so future denial policies can revoke it.
	CapOutputSecret = "output.secret"

	// CapSuspend allows a task to call dicode.suspend() — pause the run,
	// hand the runtime an opaque state blob plus a form schema, and exit
	// cleanly for later resume (#95). Granted only to runtimes that read
	// srv.Suspend() to build a suspended result (deno, python); withheld from
	// container runtimes (docker/podman) so a suspend attempt fails with a
	// clear permission-denied error instead of being acked then dropped.
	// See runtimeSupportsSuspend.
	CapSuspend = "suspend"

	// Conditionally granted to tasks based on security config.
	CapTaskTrigger = "tasks.trigger" // dicode.run_task — also checked against allowed_tasks list
	CapTasksList   = "tasks.list"    // dicode.list_tasks
	CapRunsList    = "runs.list"     // dicode.get_runs
	CapMCPCall     = "mcp.call"      // mcp.list_tools, mcp.call — also checked against allowed_mcp

	// Run-input retention management — gated per-task via permissions.dicode.
	CapRunsListExpired = "runs.list_expired" // dicode.runs.list_expired
	CapRunsDeleteInput = "runs.delete_input" // dicode.runs.delete_input
	CapRunsPinInput    = "runs.pin_input"    // dicode.runs.pin_input
	CapRunsUnpinInput  = "runs.unpin_input"  // dicode.runs.unpin_input
	// CapRunsGetInput is granted via permissions.dicode.runs_get_input. Sensitive —
	// grants cross-task input read. Redaction at write time (#233) bounds the surface.
	CapRunsGetInput      = "runs.get_input"
	CapRunsReplay        = "runs.replay"          // dicode.runs.replay — re-fire a persisted run
	CapTasksTest         = "tasks.test"           // dicode.tasks.test — run a task's sibling test file
	CapSourcesList       = "sources.list"         // dicode.sources.list
	CapSourcesSetDevMode = "sources.set_dev_mode" // dicode.sources.set_dev_mode
	CapGitCommitPush     = "git.commit_push"      // dicode.git.commit_push (#234)

	// CapCryptoCall gates dicode.crypto.encrypt() and dicode.crypto.decrypt().
	// Granted if Permissions.Dicode.Crypto is non-empty. Per-call enforcement
	// of the context allow-list happens in the dispatch case.
	CapCryptoCall = "crypto.call"

	// CapAuditQuery gates dicode.audit.query() — read access to the security
	// audit trail (#415). Sensitive: the log enumerates every actor, target,
	// and denial across the system, so it is opt-in via
	// permissions.dicode.audit_query and never granted by default.
	CapAuditQuery = "audit.query"

	// Reserved for CLI and WebUI clients (not issued to task shims today).
	CapHTTPRegister  = "http.register" // register HTTP handler routes (issue #54)
	CapSourcesManage = "sources.manage"
	CapSecretsWrite  = "secrets.write"
	// CapSecretsHas allows a task to call dicode.secrets.has(key) — a
	// boolean presence check. Never returns the value. Symmetric with
	// SecretsWrite but a distinct cap so tasks can check presence without
	// write rights.
	CapSecretsHas = "secrets.has"

	// CLI capabilities — granted to dicode CLI clients on the control socket.
	CapCLIRun     = "cli.run"     // trigger a task run and stream its output
	CapCLIList    = "cli.list"    // list tasks and their last-run status
	CapCLILogs    = "cli.logs"    // fetch log entries for a run
	CapCLIStatus  = "cli.status"  // daemon health and uptime
	CapCLISecrets = "cli.secrets" // list / set / delete secrets
	CapCLIAI      = "cli.ai"      // fire the configured ai task with a prompt
	// CapCLITaskPending gates cli.task.pending — listing tasks the approval
	// gate is holding. Read-only and strictly weaker than cli.task.approve
	// (which cli clients already hold), but a distinct cap so a future policy
	// could grant discovery without approval rights.
	CapCLITaskPending = "cli.task.pending"
)

// cliCaps is the full capability set granted to every CLI client.
func cliCaps() []string {
	return []string{
		CapCLIRun,
		CapCLIList,
		CapCLILogs,
		CapCLIStatus,
		CapCLISecrets,
		CapCLIAI,
		CapCLITaskPending,
	}
}

// defaultTaskCaps returns the base capability set granted to every task shim token.
// Only the core I/O caps are always granted; all dicode.* API caps are opt-in
// via permissions.dicode in task.yaml.
func defaultTaskCaps() []string {
	return []string{
		CapLog,
		CapParamsRead,
		CapInputRead,
		CapKVRead,
		CapKVWrite,
		CapOutputWrite,
		CapReturn,
		CapOutputSecret,
		CapSetGroup,
	}
}

// runtimeSupportsSuspend reports whether a runtime implements the suspend
// mechanism — reading srv.Suspend() after the task exits to turn a
// dicode.suspend() into a suspended RunResult. Only the managed subprocess
// runtimes do. Container runtimes (docker/podman) run an opaque image and
// never read the payload, so granting CapSuspend there would let a suspend be
// acked then silently dropped; withholding it yields a clear cap-denied error.
func runtimeSupportsSuspend(rt task.Runtime) bool {
	switch rt {
	case task.RuntimeDocker, task.RuntimePodman:
		return false
	default:
		return true
	}
}
