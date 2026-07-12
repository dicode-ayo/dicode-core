package ipc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dicode/dicode/pkg/audit"
	"github.com/dicode/dicode/pkg/db"
	mcpclient "github.com/dicode/dicode/pkg/mcp/client"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/schemavalidate"
	"github.com/dicode/dicode/pkg/secrets"
	gitsource "github.com/dicode/dicode/pkg/source/git"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"github.com/dicode/dicode/pkg/tasktest"
	"go.uber.org/zap"
)

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
	log      *zap.Logger
	audit    *audit.Store // best-effort task_called / mcp_called emission (#45); nil-safe

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

	gateway      *Gateway             // optional; enables http.register for daemon tasks
	inputStore   *registry.InputStore // optional; enables dicode.runs.delete_input blob deletion
	replayer     *registry.Replayer   // optional; enables dicode.runs.replay
	sourceMgr    SourceDevModeSetter  // optional; enables dicode.sources.set_dev_mode
	repoResolver RepoPathResolver     // optional; enables dicode.git.commit_push
	crypto       *cryptoHandler       // optional; enables dicode.crypto.{encrypt, decrypt}
	// testGuard vetoes dicode.tasks.test for a given task ID. The approval
	// gate wires its FireGuard here: a pending (unapproved) task's test file
	// runs with full host permissions, so it must be refused exactly like a
	// fire. Nil means allow.
	testGuard func(taskID string) error

	ctx        context.Context
	socketPath string
	socketDir  string // per-run 0700 parent dir; empty on Windows
	listener   net.Listener

	connWG   sync.WaitGroup // tracks in-flight handleConn goroutines
	acceptMu sync.Mutex     // serialises accept+Add against Stop's Wait

	mu      sync.Mutex
	output  *OutputResult
	suspend *SuspendResult // set when the task calls dicode.suspend (#95)
	retCh   chan any

	// resumeState / resumeInput are the prior-run payload injected when this
	// run is a resume of a suspended one (#95). Exposed to the task as
	// ctx.state / ctx.input via the "resume" IPC method. Both
	// are nil on a first (non-resume) invocation. Set via SetResume before
	// Start; read-only afterwards, so no mutex is needed.
	//
	// resumed is the authoritative resume signal reported to the SDK: carried
	// state can be genuinely null on a resume, so its presence cannot stand in
	// for it.
	resumed     bool
	resumeState json.RawMessage
	resumeInput json.RawMessage

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
		audit:        audit.NewStore(database),
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

// Start creates the Unix socket and begins accepting connections.
// Returns the socket path and a capability token to pass to the subprocess.
//
// On non-Windows platforms the socket is placed inside a per-run directory
// created with mode 0700 (e.g. /tmp/dicode-<runID>/ipc.sock). This removes
// the brief pre-chmod window and makes the socket unreachable by other local
// users independent of the umask at creation time. On Windows the flat
// /tmp/dicode-<runID>.sock path is kept because AF_UNIX directory semantics
// differ there.
func (s *Server) Start(ctx context.Context) (socketPath, token string, err error) {
	s.ctx = ctx

	if runtime.GOOS == "windows" {
		socketPath = fmt.Sprintf("/tmp/dicode-%s.sock", s.runID)
		_ = os.Remove(socketPath)
	} else {
		dir := filepath.Join("/tmp", "dicode-"+s.runID)
		// Remove any leftover dir from a previous (crashed) run.
		_ = os.RemoveAll(dir)
		if err := os.Mkdir(dir, 0700); err != nil {
			return "", "", fmt.Errorf("ipc: mkdir socket dir: %w", err)
		}
		s.socketDir = dir
		socketPath = filepath.Join(dir, "ipc.sock")
	}

	l, err := net.Listen("unix", socketPath)
	if err != nil {
		if s.socketDir != "" {
			_ = os.RemoveAll(s.socketDir)
		}
		return "", "", fmt.Errorf("ipc: listen %s: %w", socketPath, err)
	}
	// On Windows the socket is created in a shared /tmp without a 0700
	// parent dir, so we still chmod it 0600 to restrict access. On Unix the
	// 0700 parent dir already makes the socket unreachable; chmod is a
	// belt-and-suspenders extra that also closes the brief pre-chmod window
	// on non-Windows platforms that lack sticky-bit /tmp behaviour.
	if err := os.Chmod(socketPath, 0600); err != nil {
		_ = l.Close()
		if s.socketDir != "" {
			_ = os.RemoveAll(s.socketDir)
		}
		return "", "", fmt.Errorf("ipc: chmod socket: %w", err)
	}
	s.socketPath = socketPath
	s.listener = l

	// Build capability set for this task.
	// Core I/O caps are always granted; dicode.* API caps require explicit
	// opt-in via permissions.dicode in task.yaml.
	caps := defaultTaskCaps()
	if s.spec != nil && runtimeSupportsSuspend(s.spec.Runtime) {
		caps = append(caps, CapSuspend)
	}
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
		if dp.SecretsHas {
			caps = append(caps, CapSecretsHas)
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
		if dp.RunsGetInput {
			caps = append(caps, CapRunsGetInput)
		}
		if dp.TasksTest {
			caps = append(caps, CapTasksTest)
		}
		if dp.SourcesSetDevMode {
			caps = append(caps, CapSourcesSetDevMode)
		}
		if dp.GitCommitPush {
			caps = append(caps, CapGitCommitPush)
		}
		if len(dp.Crypto) > 0 {
			caps = append(caps, CapCryptoCall)
		}
		if dp.AuditQuery {
			caps = append(caps, CapAuditQuery)
		}
	}
	if s.spec != nil && s.spec.Trigger.Daemon && s.gateway != nil {
		caps = append(caps, CapHTTPRegister)
	}

	token, err = IssueToken(s.secret, "task:"+s.taskID, s.runID, caps)
	if err != nil {
		_ = l.Close()
		if s.socketDir != "" {
			_ = os.RemoveAll(s.socketDir)
		} else {
			_ = os.Remove(socketPath)
		}
		return "", "", fmt.Errorf("ipc: issue token: %w", err)
	}

	go s.accept()
	go s.flushLogs()
	return socketPath, token, nil
}

// Stop closes the listener (stopping new connections), waits for all
// in-flight handleConn goroutines to finish (so all bufferLog calls
// complete), then signals the flush goroutine for a final drain and waits
// for it to exit. This ordering ensures no log entries are silently lost.
func (s *Server) Stop() {
	// Step 1: stop accepting new connections so no new handleConn goroutines
	// are spawned after this point.
	if s.listener != nil {
		_ = s.listener.Close()
	}
	// On Unix the socket lives inside a per-run 0700 dir; remove the whole
	// tree. On Windows (socketDir == "") fall back to removing the flat file.
	if s.socketDir != "" {
		_ = os.RemoveAll(s.socketDir)
	} else if s.socketPath != "" {
		_ = os.Remove(s.socketPath)
	}

	// Step 2: wait for all in-flight handleConn goroutines to exit. Each
	// goroutine may still be calling bufferLog, so we must drain them before
	// triggering the final flush — otherwise those log entries arrive in the
	// buffer after the flush goroutine has already exited and are lost.
	//
	// Acquire acceptMu first to close the TOCTOU window: if accept() is
	// between Accept() returning a live conn and connWG.Add(1), Wait()
	// would see zero and proceed while a handler is about to be spawned.
	// The lock ensures accept() finishes its Add+spawn before we Wait.
	s.acceptMu.Lock()   //nolint:SA2001 // barrier: ensures accept() finishes its Add+spawn
	s.acceptMu.Unlock() // before we observe connWG's count
	s.connWG.Wait()

	// Step 3: signal the flush goroutine to do a final drain and exit.
	select {
	case <-s.logFlushCh:
		// already closed
	default:
		close(s.logFlushCh)
	}
	// Wait for the goroutine to complete the final flush before returning.
	<-s.logFlushDone
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

// RepoPathResolver maps a source name to its on-disk repo path. Defined in
// pkg/ipc to avoid an upward import; satisfied by *webui.SourceManager.
type RepoPathResolver interface {
	ResolveRepoPath(sourceName string) (string, error)
}

// SetRepoResolver attaches a RepoPathResolver (typically *webui.SourceManager)
// for dicode.git.commit_push dispatch. nil disables.
func (s *Server) SetRepoResolver(r RepoPathResolver) { s.repoResolver = r }

// SetCryptoHandler installs a generic encrypt/decrypt handler used by
// dicode.crypto.{encrypt, decrypt}. Daemon wires this in at boot once
// secrets.LocalProvider is initialised.
func (s *Server) SetCryptoHandler(d SubKeyDeriver) {
	s.crypto = newCryptoHandler(d)
}

// SetTestGuard installs the approval gate's veto for dicode.tasks.test.
// A non-nil error from the guard refuses the test run (the target task's
// test file executes with full host permissions, so an unapproved task must
// not reach it). nil allows everything. Must be called before Start.
func (s *Server) SetTestGuard(g func(taskID string) error) { s.testGuard = g }

// ReturnCh receives the task return value once the subprocess sends "return".
func (s *Server) ReturnCh() <-chan any { return s.retCh }

// Output returns the captured output, or nil if none was set.
func (s *Server) Output() *OutputResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.output
}

// Suspend returns the payload captured when the task called dicode.suspend,
// or nil if it did not (#95). The runtime reads it after the subprocess exits
// to build a suspended RunResult.
func (s *Server) Suspend() *SuspendResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.suspend
}

// SetResume injects the prior-run state and user input for a resumed run,
// exposed to the task as ctx.state / ctx.input (#95). resumed is the
// authoritative resume signal the SDK dispatches on; state/input are opaque JSON
// blobs and may be nil even on a resume. Must be called before Start.
func (s *Server) SetResume(resumed bool, state, input json.RawMessage) {
	s.resumed = resumed
	s.resumeState = state
	s.resumeInput = input
}

func (s *Server) accept() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		// Hold acceptMu around Add+spawn so Stop() cannot observe a
		// zero WaitGroup count while we hold a live conn. Stop()
		// acquires acceptMu before connWG.Wait(), closing the TOCTOU
		// window between Accept returning and Add(1) executing.
		s.acceptMu.Lock()
		s.connWG.Add(1)
		go func(c net.Conn) {
			defer s.connWG.Done()
			s.handleConn(c)
		}(conn)
		s.acceptMu.Unlock()
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
				// Fold these values into the run's redactor, preserving
				// what it already scrubs — the secrets resolved at launch
				// and any ephemeral per-run MCP token. Replacing it wholesale
				// would drop those and re-expose them on later log lines from
				// this same run.
				//
				// atomic.Store synchronises with the atomic.Load on the
				// "log" hot path; no mutex needed here.
				extra := make([]string, 0, len(sm))
				for _, v := range sm {
					extra = append(extra, v)
				}
				s.redactor.Store(s.redactor.Load().WithExtra(extra...))

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

		case "resume":
			// Returns the prior-run state + user input for a resumed run
			// (#95), exposed to the task as ctx.state / ctx.input.
			// Gated by the same cap as input — it is contextual run input.
			// `resumed` is the resume signal the SDK dispatches on; state/input
			// are nil on a first invocation and may be nil even on a resume.
			if !hasCap(caps, CapInputRead) {
				reply(req.ID, nil, "ipc: permission denied (input.read)")
				continue
			}
			reply(req.ID, map[string]any{
				"resumed": s.resumed,
				"state":   s.resumeState,
				"input":   s.resumeInput,
			}, "")

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

		case "dicode.suspend":
			// The task is pausing: capture the state/form/deadline and ack.
			// dicode.suspend() is a request/response call so the ack ordering
			// guarantees the payload is recorded before the shim throws its
			// SuspendSignal and the subprocess exits — the runtime then reads
			// Suspend() to build a suspended RunResult. Never resolves in task
			// code (the shim throws on ack).
			if !hasCap(caps, CapSuspend) {
				reply(req.ID, nil, "ipc: permission denied (suspend)")
				continue
			}
			// A null or empty schema means "no constraint": normalize it to nil
			// so it is neither probed as an invalid document nor persisted as the
			// literal `null` (which would 400 every resume). A present schema must
			// compile now, while the task can still react — an un-compilable schema
			// stored here would brick every resume until the TTL sweep, with no
			// author feedback.
			schema := req.Schema
			if len(bytes.TrimSpace(schema)) == 0 || bytes.Equal(bytes.TrimSpace(schema), []byte("null")) {
				schema = nil
			} else if _, err := schemavalidate.Compile(schema); err != nil {
				reply(req.ID, nil, "ipc: invalid suspend schema: "+err.Error())
				continue
			}
			s.mu.Lock()
			s.suspend = &SuspendResult{
				State:    req.State,
				Schema:   schema,
				Deadline: req.Deadline,
			}
			s.mu.Unlock()
			reply(req.ID, true, "")

		case "dicode.set_group":
			if !hasCap(caps, CapSetGroup) {
				reply(req.ID, nil, "ipc: permission denied (set_group)")
				continue
			}
			if s.runID == "" {
				reply(req.ID, nil, "ipc: set_group requires an active run context")
				continue
			}
			if err := s.registry.SetRunGroup(context.Background(), s.runID, req.Group); err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, true, "")

		case "dicode.run_task":
			if !hasCap(caps, CapTaskTrigger) {
				s.auditTaskCall(req, false, "permission denied (tasks.trigger)", "")
				reply(req.ID, nil, "ipc: permission denied (tasks.trigger)")
				continue
			}
			if s.engine == nil {
				reply(req.ID, nil, "ipc: engine not available")
				continue
			}
			if !s.taskAllowed(req.TaskID) {
				s.auditTaskCall(req, false, "not in security.allowed_tasks", "")
				reply(req.ID, nil, fmt.Sprintf("ipc: task %q not in security.allowed_tasks", req.TaskID))
				continue
			}
			// When the caller signals MCP context, only allow invocation of
			// tasks that have explicitly opted in via mcp_exposed: true.
			if req.MCPContext {
				if targetSpec, ok := s.registry.Get(req.TaskID); !ok {
					s.auditTaskCall(req, false, "task not found", "")
					reply(req.ID, nil, fmt.Sprintf("ipc: task %q not found", req.TaskID))
					continue
				} else if !targetSpec.MCPExposed {
					s.auditTaskCall(req, false, "not exposed via MCP", "")
					reply(req.ID, nil, fmt.Sprintf("ipc: task %q is not exposed via MCP", req.TaskID))
					continue
				}
			}
			var callParams map[string]string
			if len(req.Params) > 0 {
				_ = json.Unmarshal(req.Params, &callParams)
			}
			// Pass s.runID as the parent so the new run is linked to the
			// caller (#116). When the caller is the CLI control socket
			// s.runID is "" and FireFromTask falls back to a plain manual.
			runID, err := s.engine.FireFromTask(s.ctx, req.TaskID, s.runID, callParams)
			if err != nil {
				s.auditTaskCall(req, true, "fire failed: "+err.Error(), "")
				reply(req.ID, nil, err.Error())
				continue
			}
			s.auditTaskCall(req, true, "", runID)
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
			// Summary trimmed: id, name, description, params,
			// template, webhook, enabled. NOT exposed: TaskDir (filesystem
			// leakage), Permissions (could hint at secret env-var names),
			// Trigger.WebhookSecret (would defeat HMAC auth). The fields
			// here are all already visible via the WebUI's /api/tasks JSON.
			type taskSummary struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				Description string `json:"description,omitempty"`
				Params      any    `json:"params,omitempty"`
				Template    string `json:"template,omitempty"`
				Webhook     string `json:"webhook,omitempty"`
				Enabled     bool   `json:"enabled"`
			}
			all := s.registry.All()
			summaries := make([]taskSummary, 0, len(all))
			for _, sp := range all {
				// When the caller signals MCP context, only surface tasks
				// that have explicitly opted in via mcp_exposed: true.
				if req.MCPContext && !sp.MCPExposed {
					continue
				}
				summaries = append(summaries, taskSummary{
					ID:          sp.ID,
					Name:        sp.Name,
					Description: sp.Description,
					Params:      sp.Params,
					Template:    sp.Template,
					Webhook:     sp.Trigger.Webhook,
					Enabled:     sp.Enabled,
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
			// Granted via permissions.dicode.runs_get_input. Subject to a
			// caller-scope ownership check (#246) symmetric to
			// dicode.runs.replay: a task may read a run's input only when
			// (a) the run belongs to the caller's task, OR (b) the caller's
			// own run was fired with parent_run_id == requested run id —
			// the auto-fix lineage where a chain-fired task reads its
			// failed parent.
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
			// Ownership / lineage check.
			if s.taskID != "" {
				ownsRun := run.TaskID == s.taskID
				lineageOK := false
				if !ownsRun && s.runID != "" {
					if callerRun, cerr := s.registry.GetRun(s.ctx, s.runID); cerr == nil {
						lineageOK = callerRun.ParentRunID == req.RunID
					}
				}
				if !ownsRun && !lineageOK {
					reply(req.ID, nil, "ipc: caller task may not read this run's input")
					continue
				}
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
			// Look up the calling run's parent_run_id for the lineage check (#246).
			var callerParentRunID string
			if s.runID != "" {
				if callerRun, err := s.registry.GetRun(s.ctx, s.runID); err == nil {
					callerParentRunID = callerRun.ParentRunID
				}
			}
			newRunID, err := s.replayer.Replay(s.ctx, req.RunID, req.TaskID, s.taskID, callerParentRunID)
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
			// Approval-gate veto: the test file runs with full host perms,
			// so a pending task must be refused here just like a fire.
			if s.testGuard != nil {
				if gerr := s.testGuard(req.TaskID); gerr != nil {
					reply(req.ID, nil, "ipc: "+gerr.Error())
					continue
				}
			}
			spec, ok := s.registry.Get(req.TaskID)
			if !ok {
				reply(req.ID, nil, "task not registered: "+req.TaskID)
				continue
			}
			// 5min cap so a hung test suite doesn't wedge the per-task IPC
			// connection forever — handleConn dispatches sequentially.
			tctx, cancel := context.WithTimeout(s.ctx, 5*time.Minute)
			result, err := tasktest.Run(tctx, spec)
			cancel()
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

		case "dicode.git.commit_push":
			if !hasCap(caps, CapGitCommitPush) {
				reply(req.ID, nil, "ipc: permission denied (git.commit_push)")
				continue
			}
			if req.SourceID == "" {
				reply(req.ID, nil, "ipc: source_id required")
				continue
			}
			if s.repoResolver == nil {
				reply(req.ID, nil, "ipc: repo resolver not configured")
				continue
			}
			// Validate auth_token_env against permissions.env to prevent
			// arbitrary daemon env var exfiltration. Skip when empty (no auth).
			if req.AuthTokenEnv != "" && !authTokenEnvAllowed(s.spec, req.AuthTokenEnv) {
				reply(req.ID, nil, fmt.Sprintf("ipc: auth_token_env %q not declared in permissions.env", req.AuthTokenEnv))
				continue
			}
			repoPath, err := s.repoResolver.ResolveRepoPath(req.SourceID)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			authToken := ""
			if req.AuthTokenEnv != "" {
				authToken = os.Getenv(req.AuthTokenEnv)
			}
			hash, err := gitsource.CommitPush(s.ctx, repoPath, gitsource.CommitPushOptions{
				Message:      req.CommitMsg,
				Branch:       req.Branch,
				BranchPrefix: req.BranchPrefix,
				AllowMain:    req.AllowMain,
				Files:        req.Files,
				Author: gitsource.Signature{
					Name:  req.AuthorName,
					Email: req.AuthorEmail,
				},
				AuthToken: authToken,
			})
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, map[string]any{"commit": hash}, "")

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

		case "dicode.secrets.has":
			// Presence-only check — never returns the value. Requires
			// permissions.dicode.secrets_has: true (CapSecretsHas).
			if !hasCap(caps, CapSecretsHas) {
				reply(req.ID, nil, "ipc: permission denied (secrets.has)")
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
			found, err := s.secrets.Has(context.Background(), req.Key)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, found, "")

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
				s.auditMCPCall(req, false, "permission denied (mcp.call)")
				reply(req.ID, nil, "ipc: permission denied (mcp.call)")
				continue
			}
			if !s.mcpAllowed(req.MCPName) {
				s.auditMCPCall(req, false, "not in security.allowed_mcp")
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
			// Audit before the network round-trip so every authorized
			// invocation is recorded even when the MCP server errors.
			s.auditMCPCall(req, true, "")
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

		// ── dicode.crypto.* ──────────────────────────────────────────────

		case "dicode.crypto.encrypt":
			if !hasCap(caps, CapCryptoCall) {
				reply(req.ID, nil, "ipc: permission denied (crypto.call)")
				continue
			}
			if s.crypto == nil {
				reply(req.ID, nil, "ipc: crypto handler not initialised")
				continue
			}
			if !s.cryptoContextAllowed(req.Context) {
				reply(req.ID, nil, fmt.Sprintf("ipc: context %q not in permissions.dicode.crypto", req.Context))
				continue
			}
			pt, err := base64.StdEncoding.DecodeString(req.PlaintextB64)
			if err != nil {
				reply(req.ID, nil, fmt.Sprintf("invalid plaintext_b64: %v", err))
				continue
			}
			ct, err := s.crypto.Encrypt(req.Context, pt)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, map[string]string{"ciphertext_b64": base64.StdEncoding.EncodeToString(ct)}, "")

		case "dicode.crypto.decrypt":
			if !hasCap(caps, CapCryptoCall) {
				reply(req.ID, nil, "ipc: permission denied (crypto.call)")
				continue
			}
			if s.crypto == nil {
				reply(req.ID, nil, "ipc: crypto handler not initialised")
				continue
			}
			if !s.cryptoContextAllowed(req.Context) {
				reply(req.ID, nil, fmt.Sprintf("ipc: context %q not in permissions.dicode.crypto", req.Context))
				continue
			}
			ct, err := base64.StdEncoding.DecodeString(req.CiphertextB64)
			if err != nil {
				reply(req.ID, nil, fmt.Sprintf("invalid ciphertext_b64: %v", err))
				continue
			}
			pt, err := s.crypto.Decrypt(req.Context, ct)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, map[string]string{"plaintext_b64": base64.StdEncoding.EncodeToString(pt)}, "")

		// ── dicode.audit.* ───────────────────────────────────────────────

		case "dicode.audit.query":
			if !hasCap(caps, CapAuditQuery) {
				reply(req.ID, nil, "ipc: permission denied (audit.query)")
				continue
			}
			if s.audit == nil {
				reply(req.ID, nil, "ipc: audit log unavailable (no database)")
				continue
			}
			after, err := audit.DecodeCursor(req.After)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			ascending := false
			switch req.Order {
			case "", "desc":
			case "asc":
				ascending = true
			default:
				reply(req.ID, nil, "ipc: order must be asc or desc")
				continue
			}
			events, err := s.audit.Query(context.Background(), audit.Filter{
				TaskID:    req.TaskID,
				Actor:     req.Actor,
				EventType: req.EventType,
				Limit:     req.Limit,
				After:     after,
				Ascending: ascending,
			})
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			var next string
			if n := len(events); n > 0 {
				next = audit.EncodeCursor(audit.CursorOf(events[n-1]))
			}
			reply(req.ID, AuditQueryResult{Events: events, NextCursor: next}, "")

		default:
			if req.ID != "" {
				reply(req.ID, nil, fmt.Sprintf("ipc: unknown method %q", req.Method))
			}
		}
	}
}

// auditTaskCall records a dicode.run_task invocation (#45). The event type
// is mcp_called when the caller signalled MCP context (i.e. the buildin/mcp
// task forwarding a tools/call), task_called otherwise. firedRunID is the
// newly fired run when the call succeeded; the event otherwise falls back
// to the caller's own run ID for correlation. Best-effort and nil-safe.
func (s *Server) auditTaskCall(req Request, allowed bool, reason, firedRunID string) {
	eventType := audit.EventTaskCalled
	if req.MCPContext {
		eventType = audit.EventMCPCalled
	}
	var callParams map[string]string
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &callParams)
	}
	runID := firedRunID
	if runID == "" {
		runID = s.runID
	}
	s.audit.Emit(context.Background(), audit.Event{
		EventType:  eventType,
		ActorKind:  "task",
		ActorID:    s.taskID,
		TargetKind: "task",
		TargetID:   req.TaskID,
		Params:     audit.SanitizeParams(callParams),
		RunID:      runID,
		Allowed:    allowed,
		Reason:     reason,
	})
}

// auditMCPCall records an outbound mcp.call to an external MCP server (#45).
// Tool arguments are sanitized recursively before storage. Best-effort and
// nil-safe.
func (s *Server) auditMCPCall(req Request, allowed bool, reason string) {
	var args map[string]any
	if len(req.Args) > 0 {
		_ = json.Unmarshal(req.Args, &args)
	}
	var params string
	if args != nil {
		params = audit.SanitizeAny(args)
	}
	s.audit.Emit(context.Background(), audit.Event{
		EventType:  audit.EventMCPCalled,
		ActorKind:  "task",
		ActorID:    s.taskID,
		TargetKind: "mcp",
		TargetID:   req.MCPName + "/" + req.Tool,
		Params:     params,
		RunID:      s.runID,
		Allowed:    allowed,
		Reason:     reason,
	})
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

// daemonPrivateCryptoContexts is the explicit denylist of sub-key context
// strings the daemon uses for its own internal encrypted storage. Tasks may
// not access these even with a wildcard ("*") grant, because doing so would
// let a task derive the same key the daemon uses to encrypt data on behalf of
// all users/tasks (e.g. persisted run inputs).
//
// Note: buildin tasks that legitimately use "dicode/"-prefixed contexts
// (e.g. "dicode/relay-identity/v1") are granted those specific contexts
// explicitly in their task.yaml and are NOT listed here — they are a
// different namespace from these daemon-private keys.
var daemonPrivateCryptoContexts = map[string]bool{
	"dicode/run-inputs/v1":    true, // pkg/registry/inputcrypto.go
	"dicode/approval-lock/v1": true, // pkg/approval/lock.go — lock-signing key
}

// cryptoContextAllowed reports whether the requested context string is
// allowed by this task's permissions.dicode.crypto list. Mirrors the
// taskAllowed pattern used by dicode.run_task.
//
// Daemon-private contexts (see daemonPrivateCryptoContexts) are always
// denied even when "*" is granted — otherwise a task could decrypt every
// persisted run input stored by the daemon.
func (s *Server) cryptoContextAllowed(ctx string) bool {
	if daemonPrivateCryptoContexts[ctx] {
		return false
	}
	dp := dicodePerms(s.spec)
	if dp == nil {
		return false
	}
	for _, allowed := range dp.Crypto {
		if allowed == "*" || allowed == ctx {
			return true
		}
	}
	return false
}

// authTokenEnvAllowed reports whether envVar is declared in the task's
// permissions.env list. An entry matches when either:
//   - entry.From == envVar (the host env var being sourced), or
//   - entry.From == "" AND entry.Name == envVar (bare name entry).
func authTokenEnvAllowed(spec *task.Spec, envVar string) bool {
	if spec == nil {
		return false
	}
	for _, e := range spec.Permissions.Env {
		if e.From == envVar {
			return true
		}
		if e.From == "" && e.Name == envVar {
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
		s.flushBatch(context.Background(), capBatch, "cap")
	}
	if flush {
		s.flushBatch(context.Background(), batch, "size threshold")
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
	s.flushBatch(ctx, batch, "periodic")
}

// flushBatch attempts to write a batch of log entries to the DB using the
// bulk path. If the batch transaction fails (e.g. transient SQLite error),
// it falls back to per-row inserts via AppendLog so that as many entries as
// possible are salvaged rather than silently discarded.
func (s *Server) flushBatch(ctx context.Context, batch []registry.PendingLogEntry, reason string) {
	if err := s.registry.BulkAppendLogs(ctx, batch); err != nil {
		s.log.Error("ipc: bulk log flush failed, falling back to per-row inserts",
			zap.String("run", s.runID),
			zap.String("reason", reason),
			zap.Int("entries", len(batch)),
			zap.Error(err),
		)
		// Per-row fallback: salvage as many entries as possible. Each insert
		// uses AppendLog which captures a fresh timestamp — that is acceptable
		// here because we are already in a degraded error path and the
		// original TsMs is preserved in the batch entry's level/message.
		for _, e := range batch {
			if rerr := s.registry.AppendLog(ctx, e.RunID, e.Level, e.Message); rerr != nil {
				s.log.Error("ipc: per-row log fallback failed",
					zap.String("run", e.RunID),
					zap.Error(rerr),
				)
			}
		}
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
