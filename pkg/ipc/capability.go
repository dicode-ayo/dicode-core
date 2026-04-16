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

	// Conditionally granted to tasks based on security config.
	CapTaskTrigger = "tasks.trigger" // dicode.run_task — also checked against allowed_tasks list
	CapTasksList   = "tasks.list"    // dicode.list_tasks
	CapRunsList    = "runs.list"     // dicode.get_runs
	CapConfigRead  = "config.read"   // dicode.get_config
	CapMCPCall   = "mcp.call"   // mcp.list_tools, mcp.call — also checked against allowed_mcp
	CapOAuthInit = "oauth.init" // dicode.oauth.build_auth_url — for the auth-start built-in task
	CapOAuthStore = "oauth.store" // dicode.oauth.store_token — for the auth-relay built-in task

	// Reserved for CLI and WebUI clients (not issued to task shims today).
	CapHTTPRegister  = "http.register" // register HTTP handler routes (issue #54)
	CapSourcesManage = "sources.manage"
	CapSecretsWrite  = "secrets.write"

	// CLI capabilities — granted to dicode CLI clients on the control socket.
	CapCLIRun     = "cli.run"     // trigger a task run and stream its output
	CapCLIList    = "cli.list"    // list tasks and their last-run status
	CapCLILogs    = "cli.logs"    // fetch log entries for a run
	CapCLIStatus  = "cli.status"  // daemon health and uptime
	CapCLISecrets      = "cli.secrets"       // list / set / delete secrets
	CapCLIRelayRotate  = "cli.relay.rotate"  // rotate the relay identity (irreversible)
)

// cliCaps is the full capability set granted to every CLI client.
func cliCaps() []string {
	return []string{
		CapCLIRun,
		CapCLIList,
		CapCLILogs,
		CapCLIStatus,
		CapCLISecrets,
		CapCLIRelayRotate,
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
	}
}
