package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dicode/dicode/pkg/db"
	mcpclient "github.com/dicode/dicode/pkg/mcp/client"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/relay"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"github.com/dicode/dicode/pkg/tasktest"
	"go.uber.org/zap"
)

// oauthBrokerProtocolErr is the operator-facing error returned when a task
// invokes dicode.oauth.* against a broker whose welcome message did not
// advertise protocol >= 2 (issue #104). The message intentionally names
// the fix so operators know what to upgrade without digging through logs.
const oauthBrokerProtocolErr = "ipc: broker does not support split-key OAuth — upgrade dicode-relay to protocol >= 2"

// oauthRotationInProgressErr is the operator-facing error returned when a task
// invokes dicode.oauth.* after `dicode relay rotate-identity` has swapped the
// DB keys but the daemon has not yet been restarted. The rotated daemon still
// holds the OLD in-memory Identity pointer, so a new flow would be issued
// under the old SignKey — contradicting the rotation contract. Refuse here
// and tell the operator exactly what to do.
//
// Covers both build_auth_url (would sign a URL under the old key) and
// store_token (would persist a token that was encrypted to the old
// DecryptKey by the mid-flight broker session).
const oauthRotationInProgressErr = "ipc: relay rotation in progress — restart the daemon to complete rotation before issuing or accepting OAuth tokens"

// Server is a per-run Unix socket server that bridges a task subprocess and
// the Go host using the unified IPC protocol.
//
// Each task run gets its own socket. The subprocess connects, performs the
// capability handshake, then exchanges length-prefixed JSON messages.
type Server struct {
	runID  string
	taskID string
	secret []byte // daemon-level HMAC secret for token verification

	registry *registry.Registry
	db       db.DB
	params   map[string]string
	input    any
	spec     *task.Spec
	engine   EngineRunner
	secrets  secrets.Manager // optional; enables dicode.secrets_set / dicode.secrets_delete
	// secretsChain (read path) is used by dicode.oauth.list_status to walk
	// the env-fallback chain. SetSecretsChain wires it; nil means the
	// daemon has no chain configured (tests with read-only flows).
	secretsChain     secrets.Chain
	oauthID          *relay.Identity        // optional; enables dicode.oauth.* for the auth built-ins
	oauthURL         string                 // broker base URL, e.g. "https://relay.dicode.app"
	oauthPending     *relay.PendingSessions // tracks outstanding /auth/:provider flows by session id
	brokerPubkeyFn   func() string          // returns the TOFU-pinned broker pubkey (base64 SPKI DER)
	supportsOAuthFn  func() bool            // issue #104: reports broker protocol >= 2; nil means unchecked
	rotationActiveFn func() bool            // issue #144: reports whether a relay-identity rotation has started; nil means unchecked
	log              *zap.Logger

	// redactor strips secret values from inbound log messages before they
	// hit the run log. Nil load is safe (no redaction; RedactString is
	// nil-receiver safe). Wired via SetRedactor after construction;
	// runtimes that resolve env-sourced secrets should build a redactor
	// from the resolved set and install it here so tasks calling
	// `dicode.log` (the IPC log method, used by the Python SDK) get the
	// same leak-protection as tasks printing to stdout/stderr.
	//
	// Stored as atomic.Pointer because Bundle D's secret-output handler
	// can REPLACE the redactor mid-run (when a provider task calls
	// dicode.output(map, { secret: true })) while the log handler is
	// concurrently READING it from another connection's goroutine. A
	// plain pointer with mu-protected writes alone would still permit
	// torn reads under the Go memory model.
	redactor atomic.Pointer[secrets.Redactor]

	gateway    *Gateway             // optional; enables http.register for daemon tasks
	inputStore *registry.InputStore // optional; enables dicode.runs.delete_input blob deletion
	replayer   *registry.Replayer   // optional; enables dicode.runs.replay
	sourceMgr  SourceDevModeSetter  // optional; enables dicode.sources.set_dev_mode

	ctx        context.Context
	socketPath string
	listener   net.Listener

	mu     sync.Mutex
	output *OutputResult
	retCh  chan any

	// secretOut, when non-nil, receives the flat map produced by a
	// provider task calling dicode.output(map, { secret: true }). The
	// resolver waiting on the consumer's launch sets this via
	// SetSecretOutput; once received, the same values are also fed into
	// s.redactor for run-log scrubbing and the run log records key
	// names with [redacted] placeholders only.
	secretOut chan map[string]string

	// log buffer – accumulate log entries and flush in batches to reduce
	// per-line SQLite write-lock pressure (see flushLogs / flushLogsNow).
	logMu        sync.Mutex
	logBuf       []registry.PendingLogEntry
	logFlushCh   chan struct{} // closed when Stop is called
	logFlushDone chan struct{} // closed by flushLogs goroutine after final drain
}

// New creates a Server (not yet started).
//
// Both runID and taskID are required and MUST be non-empty. They flow
// into the issued IPC token's Identity claim and into the handshake
// response's task_id / run_id fields; an empty task_id would silently
// disable self-identity checks in task code (see the comment on
// handshakeResp in message.go). Construction-time enforcement keeps
// the invariant local to the boundary rather than relying on every
// consumer to re-validate.
func New(
	runID, taskID string,
	secret []byte,
	reg *registry.Registry,
	database db.DB,
	params map[string]string,
	input any,
	log *zap.Logger,
	spec *task.Spec,
	engine EngineRunner,
) *Server {
	if runID == "" {
		panic("ipc.New: runID must not be empty")
	}
	if taskID == "" {
		panic("ipc.New: taskID must not be empty")
	}
	return &Server{
		runID:        runID,
		taskID:       taskID,
		secret:       secret,
		registry:     reg,
		db:           database,
		params:       params,
		input:        input,
		spec:         spec,
		engine:       engine,
		log:          log,
		retCh:        make(chan any, 1),
		logFlushCh:   make(chan struct{}),
		logFlushDone: make(chan struct{}),
	}
}

// SetSecrets attaches the secrets manager so tasks with permissions.dicode.secrets_write
// can call dicode.secrets_set() and dicode.secrets_delete().
func (s *Server) SetSecrets(m secrets.Manager) { s.secrets = m }

// SetSecretsChain attaches the read-side chain (env-fallback aware) used by
// dicode.oauth.list_status to introspect provider connection metadata.
func (s *Server) SetSecretsChain(c secrets.Chain) { s.secretsChain = c }

// SetRedactor installs a log-message redactor. Messages received via the
// IPC "log" method are passed through r.RedactString before being
// persisted to the run log, matching the protection stdout/stderr piping
// already gets from runtime wrappers. Nil is safe (no redaction).
func (s *Server) SetRedactor(r *secrets.Redactor) { s.redactor.Store(r) }

// SetSecretOutput wires the channel that receives a provider task's
// secret map. Call BEFORE Start. Buffer >=1 is required so the IPC
// goroutine does not block on the channel send.
func (s *Server) SetSecretOutput(ch chan map[string]string) {
	// SAFETY: the field is read inside the goroutine spawned by Start;
	// calling SetSecretOutput before Start establishes the happens-before
	// edge via go's goroutine-launch semantics. Calling after Start is
	// unsupported and will race.
	s.secretOut = ch
}

// SetOAuthBroker wires the daemon's relay identity (used to sign /auth URLs
// and decrypt token deliveries) plus the broker base URL and the
// daemon-wide PendingSessions store. The task-side dicode.oauth.* API is
// inert until this is set, so the auth-relay built-in task must gracefully
// degrade when the relay client is not enabled in dicode.yaml.
//
// pending is shared across all per-run ipc.Server instances so that an
// auth-start run and the subsequent auth-complete webhook run can correlate
// on session id.
//
// supportsOAuthFn (issue #104) is consulted immediately before every OAuth
// IPC dispatch. If non-nil and it returns false, dicode.oauth.build_auth_url
// and dicode.oauth.store_token are refused with a clear operator error
// rather than silently failing at decrypt time. Passing nil leaves the IPC
// path unchecked — appropriate for test fixtures that don't run a full
// relay client.
//
// rotationActiveFn (issue #144) is consulted alongside supportsOAuthFn. If
// non-nil and it returns true, the same two OAuth methods are refused with
// an error telling the operator to restart the daemon to complete the
// rotation. Lives on the daemon (not the per-run Server) because Servers
// are recreated per task invocation but the rotation state persists for
// the daemon process lifetime.
func (s *Server) SetOAuthBroker(id *relay.Identity, baseURL string, pending *relay.PendingSessions, brokerPubkeyFn func() string, supportsOAuthFn func() bool, rotationActiveFn func() bool) {
	s.oauthID = id
	s.oauthURL = baseURL
	s.oauthPending = pending
	s.brokerPubkeyFn = brokerPubkeyFn
	s.supportsOAuthFn = supportsOAuthFn
	s.rotationActiveFn = rotationActiveFn
}

// Start creates the Unix socket and begins accepting connections.
// Returns the socket path and a capability token to pass to the subprocess.
func (s *Server) Start(ctx context.Context) (socketPath, token string, err error) {
	s.ctx = ctx
	socketPath = fmt.Sprintf("/tmp/dicode-%s.sock", s.runID)
	_ = os.Remove(socketPath)

	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return "", "", fmt.Errorf("ipc: listen %s: %w", socketPath, err)
	}
	s.socketPath = socketPath
	s.listener = l

	// Build capability set for this task.
	// Core I/O caps are always granted; dicode.* API caps require explicit
	// opt-in via permissions.dicode in task.yaml.
	caps := defaultTaskCaps()
	if dp := dicodePerms(s.spec); dp != nil {
		if len(dp.Tasks) > 0 {
			caps = append(caps, CapTaskTrigger)
		}
		if len(dp.MCP) > 0 {
			caps = append(caps, CapMCPCall)
		}
		if dp.ListTasks {
			caps = append(caps, CapTasksList)
		}
		if dp.GetRuns {
			caps = append(caps, CapRunsList)
		}
		if dp.SecretsWrite {
			caps = append(caps, CapSecretsWrite)
		}
		if dp.OAuthInit {
			caps = append(caps, CapOAuthInit)
		}
		if dp.OAuthStore {
			caps = append(caps, CapOAuthStore)
		}
		if dp.OAuthStatus {
			caps = append(caps, CapOAuthStatus)
		}
		if dp.RunsListExpired {
			caps = append(caps, CapRunsListExpired)
		}
		if dp.RunsDeleteInput {
			caps = append(caps, CapRunsDeleteInput)
		}
		if dp.RunsPinInput {
			caps = append(caps, CapRunsPinInput)
		}
		if dp.RunsUnpinInput {
			caps = append(caps, CapRunsUnpinInput)
		}
		if dp.RunsReplay {
			caps = append(caps, CapRunsReplay)
		}
		if dp.TasksTest {
			caps = append(caps, CapTasksTest)
		}
		if dp.SourcesSetDevMode {
			caps = append(caps, CapSourcesSetDevMode)
		}
	}
	if s.spec != nil && s.spec.Trigger.Daemon && s.gateway != nil {
		caps = append(caps, CapHTTPRegister)
	}

	token, err = IssueToken(s.secret, "task:"+s.taskID, s.runID, caps)
	if err != nil {
		_ = l.Close()
		_ = os.Remove(socketPath)
		return "", "", fmt.Errorf("ipc: issue token: %w", err)
	}

	go s.accept()
	go s.flushLogs()
	return socketPath, token, nil
}

// Stop signals the flush goroutine to perform a final drain, waits for it to
// finish, then closes the listener and removes the socket file.
// The goroutine is always the last writer so there is no race between a
// ticker-triggered flush and Stop's own drain.
func (s *Server) Stop() {
	// Signal the flush goroutine to do a final drain and exit.
	select {
	case <-s.logFlushCh:
		// already closed
	default:
		close(s.logFlushCh)
	}
	// Wait for the goroutine to complete the final flush before tearing down.
	<-s.logFlushDone

	if s.listener != nil {
		_ = s.listener.Close()
	}
	if s.socketPath != "" {
		_ = os.Remove(s.socketPath)
	}
}

// SetGateway attaches the HTTP gateway so daemon tasks can call http.register.
// Must be called before Start.
func (s *Server) SetGateway(g *Gateway) { s.gateway = g }

// SetInputStore attaches the InputStore so tasks with RunsDeleteInput permission
// can call dicode.runs.delete_input() to remove the blob before clearing the
// runs row. Must be called before Start.
func (s *Server) SetInputStore(is *registry.InputStore) { s.inputStore = is }

// SetReplayer attaches the Replayer so tasks with RunsReplay permission
// can call dicode.runs.replay. nil disables (dispatch returns error).
func (s *Server) SetReplayer(r *registry.Replayer) { s.replayer = r }

// SourceDevModeSetter is satisfied by webui.SourceManager. Defined in
// pkg/ipc so the daemon can wire the source manager without forcing
// pkg/ipc to import pkg/webui (which would invert the established
// dependency direction).
type SourceDevModeSetter interface {
	SetDevMode(ctx context.Context, name string, enabled bool, opts taskset.DevModeOpts) error
}

// SetSourceManager attaches a SourceDevModeSetter (typically *webui.SourceManager)
// for dicode.sources.set_dev_mode dispatch. nil disables.
func (s *Server) SetSourceManager(m SourceDevModeSetter) { s.sourceMgr = m }

// ReturnCh receives the task return value once the subprocess sends "return".
func (s *Server) ReturnCh() <-chan any { return s.retCh }

// Output returns the captured output, or nil if none was set.
func (s *Server) Output() *OutputResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.output
}

func (s *Server) accept() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	// For daemon tasks that call http.register: track the registered pattern
	// so we can unregister when the connection closes.
	var (
		registeredPattern string
		httpH             *ipcHandler
	)
	defer func() {
		if registeredPattern != "" && s.gateway != nil {
			s.gateway.Unregister(registeredPattern)
		}
	}()

	// ── handshake ────────────────────────────────────────────────────────────
	// Enforce a deadline so a subprocess that connects but never sends the
	// handshake token cannot hold this goroutine indefinitely.
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	var hs handshakeReq
	if err := readMsg(conn, &hs); err != nil {
		s.log.Warn("ipc: handshake read failed", zap.String("run", s.runID), zap.Error(err))
		return
	}
	_ = conn.SetDeadline(time.Time{}) // clear deadline for normal message loop
	claims, err := VerifyToken(s.secret, hs.Token)
	if err != nil {
		_ = writeMsg(conn, handshakeErr{Error: err.Error()})
		return
	}
	if claims.RunID != s.runID {
		_ = writeMsg(conn, handshakeErr{Error: "ipc: token run ID mismatch"})
		return
	}
	if err := writeMsg(conn, handshakeResp{
		Proto:  1,
		Caps:   claims.Caps,
		TaskID: s.taskID,
		RunID:  s.runID,
	}); err != nil {
		return
	}
	caps := claims.Caps

	// ── message loop ─────────────────────────────────────────────────────────
	reply := func(id string, result any, errMsg string) {
		if id == "" {
			return
		}
		r := Response{ID: id, Result: result}
		if errMsg != "" {
			r.Error = errMsg
			r.Result = nil
		}
		_ = writeMsg(conn, r)
	}

	for {
		var req Request
		if err := readMsg(conn, &req); err != nil {
			return // EOF or closed connection
		}

		switch req.Method {

		// ── fire-and-forget ───────────────────────────────────────────────

		case "log":
			if !hasCap(caps, CapLog) {
				continue
			}
			level := req.Level
			if level == "" {
				level = "info"
			}
			// Redact env-injected secret values from the message before
			// buffering. Nil redactor is pass-through — RedactString is
			// safe on a nil receiver (see pkg/secrets/redactor.go), so a
			// nil load from atomic.Pointer is fine and we don't need to
			// guard the call.
			s.bufferLog(level, s.redactor.Load().RedactString(req.Message))

		case "kv.set":
			if !hasCap(caps, CapKVWrite) {
				continue
			}
			var val any
			if len(req.Value) > 0 {
				_ = json.Unmarshal(req.Value, &val)
			}
			valJSON, _ := json.Marshal(val)
			ns := s.taskID + ":" + req.Key
			if err := s.db.Exec(context.Background(),
				`INSERT INTO kv (key, value) VALUES (?, ?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
				ns, string(valJSON),
			); err != nil {
				s.log.Error("ipc: kv.set", zap.String("key", req.Key), zap.Error(err))
			}

		case "kv.delete":
			if !hasCap(caps, CapKVWrite) {
				continue
			}
			ns := s.taskID + ":" + req.Key
			if err := s.db.Exec(context.Background(),
				`DELETE FROM kv WHERE key = ?`, ns,
			); err != nil {
				s.log.Error("ipc: kv.delete", zap.String("key", req.Key), zap.Error(err))
			}

		case "output":
			if !hasCap(caps, CapOutputWrite) {
				continue
			}
			if req.Secret {
				if !hasCap(caps, CapOutputSecret) {
					continue
				}
				// Flat map: decode SecretMap as map[string]string. Reject
				// nested objects per issue #119.
				var sm map[string]string
				if err := json.Unmarshal(req.SecretMap, &sm); err != nil {
					s.log.Warn("ipc: secret output: not a flat string map",
						zap.String("run", s.runID),
						zap.Error(err),
					)
					continue
				}
				// Replace the run's redactor with a new one carrying
				// these values. Prior redactor values (from secrets
				// resolved at consumer-launch time) are NOT preserved
				// here — the existing redactor doesn't expose its
				// inner values. This is acceptable because a provider
				// task is itself a CHILD run; its redactor only needs
				// to scrub the values the provider just returned.
				//
				// atomic.Store synchronises with the atomic.Load on the
				// "log" hot path; no mutex needed here.
				s.redactor.Store(secrets.NewRedactor(sm))

				// Persist key names + [redacted] placeholders to the run
				// log so operators can audit which secrets the provider
				// returned without leaking values. Sort the keys so the
				// log line is deterministic across runs (map iteration
				// order is randomised).
				keys := make([]string, 0, len(sm))
				for k := range sm {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				_ = s.registry.AppendLog(context.Background(), s.runID, "info",
					fmt.Sprintf("[dicode] secret output: %v = [redacted]", keys))

				if s.secretOut != nil {
					select {
					case s.secretOut <- sm:
					default:
						s.log.Warn("ipc: secretOut channel full or unread",
							zap.String("run", s.runID))
					}
				}
				continue
			}
			var data any
			if len(req.Data) > 0 {
				_ = json.Unmarshal(req.Data, &data)
			}
			s.mu.Lock()
			s.output = &OutputResult{
				ContentType: req.ContentType,
				Content:     req.Content,
				Data:        data,
			}
			s.mu.Unlock()

		// ── request / response ────────────────────────────────────────────

		case "params":
			if !hasCap(caps, CapParamsRead) {
				reply(req.ID, nil, "ipc: permission denied (params.read)")
				continue
			}
			reply(req.ID, s.params, "")

		case "input":
			if !hasCap(caps, CapInputRead) {
				reply(req.ID, nil, "ipc: permission denied (input.read)")
				continue
			}
			reply(req.ID, s.input, "")

		case "kv.get":
			if !hasCap(caps, CapKVRead) {
				reply(req.ID, nil, "ipc: permission denied (kv.read)")
				continue
			}
			ns := s.taskID + ":" + req.Key
			var raw string
			var found bool
			err := s.db.Query(context.Background(),
				`SELECT value FROM kv WHERE key = ?`, []any{ns},
				func(rows db.Scanner) error {
					if rows.Next() {
						found = true
						return rows.Scan(&raw)
					}
					return nil
				},
			)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			if !found {
				reply(req.ID, nil, "")
				continue
			}
			var out any
			_ = json.Unmarshal([]byte(raw), &out)
			reply(req.ID, out, "")

		case "kv.list":
			if !hasCap(caps, CapKVRead) {
				reply(req.ID, nil, "ipc: permission denied (kv.read)")
				continue
			}
			ns := s.taskID + ":"
			prefix := ns
			if req.Prefix != "" {
				prefix = ns + req.Prefix
			}
			var keys []string
			err := s.db.Query(context.Background(),
				`SELECT key FROM kv WHERE key LIKE ? ORDER BY key`,
				[]any{prefix + "%"},
				func(rows db.Scanner) error {
					for rows.Next() {
						var k string
						if err := rows.Scan(&k); err != nil {
							return err
						}
						keys = append(keys, k[len(ns):])
					}
					return nil
				},
			)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			if keys == nil {
				keys = []string{}
			}
			reply(req.ID, keys, "")

		case "return":
			if !hasCap(caps, CapReturn) {
				reply(req.ID, nil, "ipc: permission denied (return)")
				continue
			}
			var val any
			if len(req.Value) > 0 {
				_ = json.Unmarshal(req.Value, &val)
			}
			// Signal retCh BEFORE replying so the runtime's select sees it
			// before doneCh (which fires after the subprocess exits).
			select {
			case s.retCh <- val:
			default:
			}
			reply(req.ID, true, "")

		// ── dicode.* ──────────────────────────────────────────────────────

		case "dicode.run_task":
			if !hasCap(caps, CapTaskTrigger) {
				reply(req.ID, nil, "ipc: permission denied (tasks.trigger)")
				continue
			}
			if s.engine == nil {
				reply(req.ID, nil, "ipc: engine not available")
				continue
			}
			if !s.taskAllowed(req.TaskID) {
				reply(req.ID, nil, fmt.Sprintf("ipc: task %q not in security.allowed_tasks", req.TaskID))
				continue
			}
			var callParams map[string]string
			if len(req.Params) > 0 {
				_ = json.Unmarshal(req.Params, &callParams)
			}
			runID, err := s.engine.FireManual(s.ctx, req.TaskID, callParams)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			result, err := s.engine.WaitRun(s.ctx, runID)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, result, "")

		case "dicode.list_tasks":
			if !hasCap(caps, CapTasksList) {
				reply(req.ID, nil, "ipc: permission denied (tasks.list)")
				continue
			}
			type taskSummary struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				Description string `json:"description,omitempty"`
				Params      any    `json:"params,omitempty"`
			}
			all := s.registry.All()
			summaries := make([]taskSummary, 0, len(all))
			for _, sp := range all {
				summaries = append(summaries, taskSummary{
					ID:          sp.ID,
					Name:        sp.Name,
					Description: sp.Description,
					Params:      sp.Params,
				})
			}
			reply(req.ID, summaries, "")

		case "dicode.get_runs":
			if !hasCap(caps, CapRunsList) {
				reply(req.ID, nil, "ipc: permission denied (runs.list)")
				continue
			}
			limit := req.Limit
			if limit <= 0 {
				limit = 10
			}
			runs, err := s.registry.ListRuns(context.Background(), req.TaskID, limit)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, runs, "")

		// ── dicode.runs.* (retention management) ─────────────────────────

		case "dicode.runs.list_expired":
			if !hasCap(caps, CapRunsListExpired) {
				reply(req.ID, nil, "ipc: permission denied (runs.list_expired)")
				continue
			}
			beforeTs := req.BeforeTs
			if beforeTs == 0 {
				beforeTs = time.Now().Unix()
			}
			rows, err := s.registry.ListExpiredInputs(s.ctx, beforeTs)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, rows, "")

		case "dicode.runs.delete_input":
			if !hasCap(caps, CapRunsDeleteInput) {
				reply(req.ID, nil, "ipc: permission denied (runs.delete_input)")
				continue
			}
			if req.RunID == "" {
				reply(req.ID, nil, "ipc: runID required")
				continue
			}
			run, err := s.registry.GetRun(s.ctx, req.RunID)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			if run.InputStorageKey != "" && s.inputStore != nil {
				if err := s.inputStore.Delete(s.ctx, run.InputStorageKey); err != nil {
					// Sanitized log only — the full error chain may transit
					// env-resolver internals where CodeQL flags a secretKey
					// taint as go/clear-text-logging false-positive.
					_ = err
					s.log.Warn("delete_input: storage delete failed; will still clear columns",
						zap.String("run", req.RunID),
						zap.String("error_class", "storage_delete"))
				}
			}
			if err := s.registry.ClearRunInput(s.ctx, req.RunID); err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, map[string]any{"ok": true}, "")

		case "dicode.runs.pin_input":
			if !hasCap(caps, CapRunsPinInput) {
				reply(req.ID, nil, "ipc: permission denied (runs.pin_input)")
				continue
			}
			if req.RunID == "" {
				reply(req.ID, nil, "ipc: runID required")
				continue
			}
			if err := s.registry.PinRunInput(s.ctx, req.RunID); err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, map[string]any{"ok": true}, "")

		case "dicode.runs.unpin_input":
			if !hasCap(caps, CapRunsUnpinInput) {
				reply(req.ID, nil, "ipc: permission denied (runs.unpin_input)")
				continue
			}
			if req.RunID == "" {
				reply(req.ID, nil, "ipc: runID required")
				continue
			}
			if err := s.registry.UnpinRunInput(s.ctx, req.RunID); err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, map[string]any{"ok": true}, "")

		case "dicode.runs.get_input":
			// Internal-only: gated behind CapRunsGetInput which is not granted
			// to any task today. Wired now so #234's auto-fix driver can use it
			// once the capability is granted to that task.
			if !hasCap(caps, CapRunsGetInput) {
				reply(req.ID, nil, "ipc: permission denied (runs.get_input)")
				continue
			}
			if req.RunID == "" {
				reply(req.ID, nil, "ipc: runID required")
				continue
			}
			if s.inputStore == nil {
				reply(req.ID, nil, "ipc: input store not configured")
				continue
			}
			run, err := s.registry.GetRun(s.ctx, req.RunID)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			if run.InputStorageKey == "" {
				reply(req.ID, nil, "ipc: no persisted input for run "+req.RunID)
				continue
			}
			fetched, err := s.inputStore.Fetch(s.ctx, req.RunID, run.InputStorageKey, run.InputStoredAt)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, fetched, "")

		case "dicode.runs.replay":
			if !hasCap(caps, CapRunsReplay) {
				reply(req.ID, nil, "ipc: permission denied (runs.replay)")
				continue
			}
			if s.replayer == nil {
				reply(req.ID, nil, "ipc: replayer not configured")
				continue
			}
			if req.RunID == "" {
				reply(req.ID, nil, "ipc: runID required")
				continue
			}
			newRunID, err := s.replayer.Replay(s.ctx, req.RunID, req.TaskID)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, map[string]any{"run_id": newRunID}, "")

		case "dicode.tasks.test":
			if !hasCap(caps, CapTasksTest) {
				reply(req.ID, nil, "ipc: permission denied (tasks.test)")
				continue
			}
			if req.TaskID == "" {
				reply(req.ID, nil, "ipc: taskID required")
				continue
			}
			spec, ok := s.registry.Get(req.TaskID)
			if !ok {
				reply(req.ID, nil, "task not registered: "+req.TaskID)
				continue
			}
			result, err := tasktest.Run(s.ctx, spec)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, result, "")

		case "dicode.sources.set_dev_mode":
			if !hasCap(caps, CapSourcesSetDevMode) {
				reply(req.ID, nil, "ipc: permission denied (sources.set_dev_mode)")
				continue
			}
			if s.sourceMgr == nil {
				reply(req.ID, nil, "ipc: source manager not available")
				continue
			}
			if req.Name == "" {
				reply(req.ID, nil, "ipc: name required")
				continue
			}
			opts := taskset.DevModeOpts{
				LocalPath: req.LocalPath,
				Branch:    req.Branch,
				Base:      req.Base,
				RunID:     req.DevRunID,
			}
			if err := s.sourceMgr.SetDevMode(s.ctx, req.Name, req.Enabled, opts); err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, map[string]any{"ok": true}, "")

		// ── dicode.secrets_* ──────────────────────────────────────────────

		case "dicode.secrets_set":
			if !hasCap(caps, CapSecretsWrite) {
				reply(req.ID, nil, "ipc: permission denied (secrets.write)")
				continue
			}
			if s.secrets == nil {
				reply(req.ID, nil, "ipc: no secrets provider configured")
				continue
			}
			if req.Key == "" {
				reply(req.ID, nil, "ipc: key required")
				continue
			}
			if err := s.secrets.Set(context.Background(), req.Key, req.StringValue); err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, true, "")

		case "dicode.secrets_delete":
			if !hasCap(caps, CapSecretsWrite) {
				reply(req.ID, nil, "ipc: permission denied (secrets.write)")
				continue
			}
			if s.secrets == nil {
				reply(req.ID, nil, "ipc: no secrets provider configured")
				continue
			}
			if req.Key == "" {
				reply(req.ID, nil, "ipc: key required")
				continue
			}
			if err := s.secrets.Delete(context.Background(), req.Key); err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, true, "")

		// ── dicode.oauth.* ────────────────────────────────────────────────
		// Two purpose-specific primitives, each gated by its own capability.
		// Neither exposes raw sign-with-identity-key or raw-decrypt — the
		// payload layouts are hardcoded so a compromised task cannot coax
		// the identity key into signing a WSS handshake digest or acting
		// as a decryption oracle for ciphertexts the broker never issued.

		case "dicode.oauth.build_auth_url":
			if !hasCap(caps, CapOAuthInit) {
				reply(req.ID, nil, "ipc: permission denied (oauth.init)")
				continue
			}
			if s.oauthID == nil || s.oauthURL == "" || s.oauthPending == nil {
				reply(req.ID, nil, "ipc: oauth broker not configured on this daemon")
				continue
			}
			// Issue #144: refuse after `dicode relay rotate-identity` has
			// swapped the DB keys. The in-memory s.oauthID still points at
			// the OLD SignKey until the daemon restarts; issuing a new
			// URL under it would contradict the rotation contract the
			// operator just signed off on.
			//
			// Checked BEFORE the #104 protocol gate because rotation-in-
			// progress is the more immediate actionable condition (restart
			// the daemon) — telling the operator to "upgrade dicode-relay"
			// when they've just run `rotate-identity` would be misleading.
			if s.rotationActiveFn != nil && s.rotationActiveFn() {
				reply(req.ID, nil, oauthRotationInProgressErr)
				continue
			}
			// Issue #104: refuse OAuth flows when the connected broker has not
			// advertised protocol >= 2. A pre-split broker would encrypt the
			// delivery to the SignKey pubkey, which DecryptOAuthToken cannot
			// open with the DecryptKey — the failure would only surface on
			// the callback, after the user has already completed the upstream
			// consent. Refusing here turns a silent crypto failure into an
			// actionable operator message.
			if s.supportsOAuthFn != nil && !s.supportsOAuthFn() {
				reply(req.ID, nil, oauthBrokerProtocolErr)
				continue
			}
			if req.Provider == "" {
				reply(req.ID, nil, "ipc: provider required")
				continue
			}
			url, authReq, err := relay.BuildAuthURL(s.oauthURL, s.oauthID, req.Provider, req.Scope, time.Now().Unix())
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			// Track this flow so store_token can validate the delivery's
			// session id against a request we actually issued.
			s.oauthPending.Add(authReq)
			reply(req.ID, map[string]any{
				"url":        url,
				"session_id": authReq.SessionID,
				"provider":   authReq.Provider,
				"timestamp":  authReq.Timestamp,
				"relay_uuid": s.oauthID.UUID,
			}, "")

		case "dicode.oauth.store_token":
			if !hasCap(caps, CapOAuthStore) {
				reply(req.ID, nil, "ipc: permission denied (oauth.store)")
				continue
			}
			if s.oauthID == nil || s.oauthPending == nil {
				reply(req.ID, nil, "ipc: oauth broker not configured on this daemon")
				continue
			}
			// Issue #144: refuse delivery after rotation. A post-rotation
			// delivery was issued under the old SignKey and encrypted to
			// the old DecryptKey; accepting it here would persist a token
			// tied to an identity the operator has explicitly retired.
			// Checked before the #104 protocol gate — see the rationale on
			// build_auth_url above.
			if s.rotationActiveFn != nil && s.rotationActiveFn() {
				reply(req.ID, nil, oauthRotationInProgressErr)
				continue
			}
			// Issue #104 — see the note on dicode.oauth.build_auth_url.
			// Reject store_token against a pre-split broker so a stale
			// session delivered just after a downgrade can't coerce a
			// mismatched-key decrypt attempt.
			if s.supportsOAuthFn != nil && !s.supportsOAuthFn() {
				reply(req.ID, nil, oauthBrokerProtocolErr)
				continue
			}
			if s.secrets == nil {
				reply(req.ID, nil, "ipc: no secrets provider configured")
				continue
			}
			if len(req.Envelope) == 0 {
				reply(req.ID, nil, "ipc: envelope required")
				continue
			}
			var env relay.OAuthTokenDeliveryPayload
			if err := json.Unmarshal(req.Envelope, &env); err != nil {
				reply(req.ID, nil, "ipc: decode envelope: "+err.Error())
				continue
			}
			// Verify broker authenticity before consuming the pending
			// session or touching any crypto. If the broker pubkey is
			// pinned (TOFU on first connect), a forged envelope from a
			// local process or MitM is rejected here.
			if s.brokerPubkeyFn != nil {
				if bpk := s.brokerPubkeyFn(); bpk != "" {
					if err := relay.VerifyBrokerSig(bpk, &env); err != nil {
						reply(req.ID, nil, "ipc: "+err.Error())
						continue
					}
				}
			}
			authReq, err := s.oauthPending.Take(env.SessionID)
			if err != nil {
				reply(req.ID, nil, "ipc: unknown or expired session")
				continue
			}
			plaintext, err := relay.DecryptOAuthToken(s.oauthID, &env)
			if err != nil {
				reply(req.ID, nil, "ipc: decrypt failed")
				continue
			}
			written, err := storeOAuthToken(context.Background(), s.secrets, authReq.Provider, plaintext)
			// Best-effort zeroization; Go can't guarantee it but this shrinks
			// the window in process memory regardless.
			for i := range plaintext {
				plaintext[i] = 0
			}
			if err != nil {
				reply(req.ID, nil, "ipc: store secret: "+err.Error())
				continue
			}
			// Structured audit entry. Fields are deliberately metadata-only
			// so that an operator tailing the run log can trace which task
			// run received which delivery without the token ever touching
			// an observability pipeline.
			if s.log != nil {
				// Truncate session id to first 8 chars — enough for
				// correlation, avoids persisting the full signed-payload
				// component in long-term log storage.
				sid := authReq.SessionID
				if len(sid) > 8 {
					sid = sid[:8]
				}
				s.log.Info("oauth token delivered",
					zap.String("task", s.taskID),
					zap.String("run", s.runID),
					zap.String("provider", authReq.Provider),
					zap.String("session", sid),
					zap.Strings("secrets", written),
				)
			}
			reply(req.ID, map[string]any{
				"provider": authReq.Provider,
				"secrets":  written,
			}, "")

		case "dicode.oauth.list_status":
			if !hasCap(caps, CapOAuthStatus) {
				reply(req.ID, nil, "ipc: permission denied (oauth.status)")
				continue
			}
			if s.secretsChain == nil {
				reply(req.ID, nil, "ipc: secrets chain not configured")
				continue
			}
			out, err := listOAuthStatus(s.ctx, s.secretsChain, req.Providers)
			if err != nil {
				reply(req.ID, nil, "ipc: list status: "+err.Error())
				continue
			}
			reply(req.ID, out, "")

		// ── mcp.* ─────────────────────────────────────────────────────────

		case "mcp.list_tools":
			if !hasCap(caps, CapMCPCall) {
				reply(req.ID, nil, "ipc: permission denied (mcp.call)")
				continue
			}
			if !s.mcpAllowed(req.MCPName) {
				reply(req.ID, nil, fmt.Sprintf("ipc: %q not in security.allowed_mcp", req.MCPName))
				continue
			}
			port, err := s.getMCPPort(req.MCPName)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			tools, err := mcpclient.New(port).ListTools(context.Background())
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, tools, "")

		case "mcp.call":
			if !hasCap(caps, CapMCPCall) {
				reply(req.ID, nil, "ipc: permission denied (mcp.call)")
				continue
			}
			if !s.mcpAllowed(req.MCPName) {
				reply(req.ID, nil, fmt.Sprintf("ipc: %q not in security.allowed_mcp", req.MCPName))
				continue
			}
			port, err := s.getMCPPort(req.MCPName)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			var args map[string]any
			if len(req.Args) > 0 {
				_ = json.Unmarshal(req.Args, &args)
			}
			result, err := mcpclient.New(port).Call(context.Background(), req.Tool, args)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, result, "")

		// ── http.register / http.respond ─────────────────────────────────

		case "http.register":
			if !hasCap(caps, CapHTTPRegister) {
				reply(req.ID, nil, "ipc: permission denied (http.register)")
				continue
			}
			if s.gateway == nil {
				reply(req.ID, nil, "ipc: http gateway not available")
				continue
			}
			if req.Pattern == "" {
				reply(req.ID, nil, "ipc: pattern is required")
				continue
			}
			if httpH == nil {
				httpH = &ipcHandler{push: func(msg any) error { return writeMsg(conn, msg) }}
			}
			// Register the new pattern before removing the old one to avoid
			// a brief window where requests for the path return 404.
			s.gateway.Register(req.Pattern, httpH)
			if registeredPattern != "" && registeredPattern != req.Pattern {
				s.gateway.Unregister(registeredPattern)
			}
			registeredPattern = req.Pattern
			reply(req.ID, true, "")

		case "http.respond":
			if httpH != nil {
				if !httpH.complete(req.RequestID, req.Status, req.RespHeaders, req.RespBody) {
					s.log.Warn("ipc: http.respond for unknown requestID",
						zap.String("run", s.runID),
						zap.String("requestID", req.RequestID),
					)
				}
			}

		default:
			if req.ID != "" {
				reply(req.ID, nil, fmt.Sprintf("ipc: unknown method %q", req.Method))
			}
		}
	}
}

// dicodePerms returns the Dicode permission block for the current spec, or nil.
func dicodePerms(spec *task.Spec) *task.DicodePermissions {
	if spec == nil {
		return nil
	}
	return spec.Permissions.Dicode
}

func (s *Server) taskAllowed(taskID string) bool {
	dp := dicodePerms(s.spec)
	if dp == nil {
		return false
	}
	for _, a := range dp.Tasks {
		if a == "*" || a == taskID {
			return true
		}
	}
	return false
}

func (s *Server) mcpAllowed(name string) bool {
	dp := dicodePerms(s.spec)
	if dp == nil {
		return false
	}
	for _, a := range dp.MCP {
		if a == "*" || a == name {
			return true
		}
	}
	return false
}

func (s *Server) getMCPPort(taskID string) (int, error) {
	spec, ok := s.registry.Get(taskID)
	if !ok {
		return 0, fmt.Errorf("ipc: mcp task %q not found", taskID)
	}
	if spec.MCPPort == 0 {
		return 0, fmt.Errorf("ipc: task %q does not declare mcp_port", taskID)
	}
	return spec.MCPPort, nil
}

// ── log buffering ─────────────────────────────────────────────────────────────

const (
	logFlushInterval = 200 * time.Millisecond
	logFlushSize     = 50
	logBufMaxSize    = 1000 // hard cap to prevent unbounded memory growth
)

// bufferLog enqueues a log entry. If the buffer reaches logFlushSize the
// batch is flushed immediately (inline) to bound memory use. If the buffer
// would exceed logBufMaxSize a synchronous flush is triggered first to
// prevent unbounded memory growth from a high-frequency logging task.
func (s *Server) bufferLog(level, message string) {
	// Whitelist the level to prevent log injection via crafted IPC messages.
	switch level {
	case "debug", "info", "warn", "error":
		// valid
	default:
		level = "info"
	}

	entry := registry.PendingLogEntry{
		RunID:   s.runID,
		Level:   level,
		Message: message,
		TsMs:    time.Now().UnixMilli(),
	}

	s.logMu.Lock()
	// If we are at the hard cap, flush synchronously before appending so the
	// buffer never grows beyond logBufMaxSize entries.
	var capBatch []registry.PendingLogEntry
	if len(s.logBuf) >= logBufMaxSize {
		capBatch = s.logBuf
		s.logBuf = nil
	}
	s.logBuf = append(s.logBuf, entry)
	flush := len(s.logBuf) >= logFlushSize
	var batch []registry.PendingLogEntry
	if flush {
		batch = s.logBuf
		s.logBuf = nil
	}
	s.logMu.Unlock()

	if capBatch != nil {
		if err := s.registry.BulkAppendLogs(context.Background(), capBatch); err != nil {
			s.log.Error("ipc: bulk log flush (cap)", zap.String("run", s.runID), zap.Error(err))
		}
	}
	if flush {
		if err := s.registry.BulkAppendLogs(context.Background(), batch); err != nil {
			s.log.Error("ipc: bulk log flush (size threshold)", zap.String("run", s.runID), zap.Error(err))
		}
	}
}

// flushLogsNow drains the buffer and writes all pending entries to the DB.
// Safe to call from any goroutine.
func (s *Server) flushLogsNow(ctx context.Context) {
	s.logMu.Lock()
	batch := s.logBuf
	s.logBuf = nil
	s.logMu.Unlock()

	if len(batch) == 0 {
		return
	}
	if err := s.registry.BulkAppendLogs(ctx, batch); err != nil {
		s.log.Error("ipc: bulk log flush", zap.String("run", s.runID), zap.Error(err))
	}
}

// flushLogs is the background goroutine that periodically flushes the log
// buffer. It exits when logFlushCh is closed (Stop) or ctx is cancelled,
// performing a final drain in both cases so no buffered entries are lost.
// It signals logFlushDone before returning so Stop() can synchronise.
func (s *Server) flushLogs() {
	defer close(s.logFlushDone)
	ticker := time.NewTicker(logFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.flushLogsNow(context.Background())
		case <-s.logFlushCh:
			// Stop() was called — do one final drain then exit.
			// The goroutine is the sole writer here, so there is no
			// race with Stop() flushing independently.
			s.flushLogsNow(context.Background())
			return
		case <-s.ctx.Done():
			// Context cancelled (error path that skips Stop) — drain
			// before exiting to avoid goroutine leak.
			s.flushLogsNow(context.Background())
			return
		}
	}
}
