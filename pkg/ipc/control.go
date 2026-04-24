package ipc

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/relay"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/tasktest"
	"go.uber.org/zap"
)

// RelayIdentityRotator rotates the daemon's relay identity on demand.
// It is implemented by main.go (which owns the db handle, the pending
// store, and the running relay client) and passed into ControlServer so
// the cli.relay.rotate_identity handler can trigger a rotation without
// ControlServer needing direct access to any of those pieces.
//
// The returned string is the new UUID. The rotator is responsible for:
//   - generating + persisting a fresh keypair in the daemon DB
//   - invalidating any in-memory state tied to the old key (pending
//     OAuth sessions, running relay WSS connection)
//   - returning cleanly even if the relay client is currently dialing
type RelayIdentityRotator func(ctx context.Context) (newUUID string, err error)

// ControlServer is the daemon's persistent control socket. It listens at a
// fixed path (dataDir/daemon.sock) and accepts connections from the dicode CLI.
//
// Each CLI client authenticates with a pre-shared token that the daemon writes
// to dataDir/daemon.token on startup. The token is signed with a per-run secret
// (same HMAC machinery as task shim tokens) and grants the full cliCaps() set.
type ControlServer struct {
	socketPath string
	tokenPath  string
	secret     []byte // HMAC key, generated on New
	token      string // pre-issued CLI token written to tokenPath

	reg             *registry.Registry
	engine          EngineRunner
	secrets         secrets.Manager // nil if no local provider configured
	metricsProvider MetricsProvider
	database        db.DB                // for broker pubkey trust pinning; nil in tests
	rotateRelay     RelayIdentityRotator // nil if relay not enabled
	log             *zap.Logger

	// defaultAITask is cfg.AI.Task — the task id that `dicode ai` fires when
	// the client doesn't supply --task. Empty when the daemon was started
	// without config (tests).
	defaultAITask string

	startedAt time.Time
	version   string
}

// NewControlServer creates a ControlServer. Call Start to begin accepting
// connections. socketPath is the Unix socket path; tokenPath is where the CLI
// token is written. defaultAITask is cfg.AI.Task — resolved at daemon startup
// so the control server can fire the right task when the CLI invokes `dicode ai`
// without --task.
func NewControlServer(
	socketPath, tokenPath string,
	reg *registry.Registry,
	engine EngineRunner,
	secretsMgr secrets.Manager,
	mp MetricsProvider,
	version string,
	log *zap.Logger,
	database db.DB,
	defaultAITask string,
) (*ControlServer, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("control: generate secret: %w", err)
	}

	cs := &ControlServer{
		socketPath:      socketPath,
		tokenPath:       tokenPath,
		secret:          secret,
		reg:             reg,
		engine:          engine,
		secrets:         secretsMgr,
		metricsProvider: mp,
		database:        database,
		log:             log,
		defaultAITask:   defaultAITask,
		startedAt:       time.Now(),
		version:         version,
	}

	// Issue the CLI token with a long TTL — the daemon re-issues on every restart,
	// so expiry is not the right protection mechanism here.
	tok, err := IssueTokenWithTTL(secret, "cli", "cli", cliCaps(), tokenCLITTL)
	if err != nil {
		return nil, fmt.Errorf("control: issue token: %w", err)
	}
	cs.token = tok
	if err := writeCLITokenFile(tokenPath, tok); err != nil {
		return nil, fmt.Errorf("control: write token file: %w", err)
	}
	return cs, nil
}

// Start begins accepting connections. It removes any stale socket file first.
// Run blocks until ctx is cancelled.
func (cs *ControlServer) Start(ctx context.Context) error {
	_ = os.Remove(cs.socketPath)
	if err := os.MkdirAll(filepath.Dir(cs.socketPath), 0700); err != nil {
		return fmt.Errorf("control: mkdir: %w", err)
	}

	ln, err := net.Listen("unix", cs.socketPath)
	if err != nil {
		return fmt.Errorf("control: listen: %w", err)
	}
	if err := os.Chmod(cs.socketPath, 0600); err != nil {
		ln.Close()
		return fmt.Errorf("control: chmod socket: %w", err)
	}
	cs.log.Info("control socket ready", zap.String("path", cs.socketPath))

	go func() {
		<-ctx.Done()
		ln.Close()
		_ = os.Remove(cs.socketPath)
		_ = os.Remove(cs.tokenPath)
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // clean shutdown
			}
			cs.log.Warn("control: accept error", zap.Error(err))
			continue
		}
		go cs.handleConn(ctx, conn)
	}
}

// handleConn runs the handshake and then the request loop for one CLI client.
func (cs *ControlServer) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// Handshake — 5-second deadline.
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	var hs handshakeReq
	if err := readMsg(conn, &hs); err != nil {
		return
	}

	// On Linux, SO_PEERCRED on a 0600 socket is strictly more secure than a
	// token file on disk — the kernel fills ucred at connect() time, so the
	// check is race-safe, and there is no credential to steal via filesystem
	// access. When the peer UID matches the daemon UID, grant full CLI caps
	// without requiring a token. Non-Linux (peerCredSupported == false) and
	// same-host UID-mismatch cases fall through to token verification.
	caps := cliCaps()
	if match, _ := peerUIDMatches(conn); !match {
		claims, err := VerifyToken(cs.secret, hs.Token)
		if err != nil || claims.Identity != "cli" {
			_ = writeMsg(conn, handshakeErr{Error: "invalid token"})
			return
		}
		caps = claims.Caps
	}
	_ = writeMsg(conn, handshakeResp{Proto: 1, Caps: caps})
	_ = conn.SetDeadline(time.Time{})

	// Derive a per-connection context so that when the client disconnects,
	// in-flight requests (e.g. cli.run) can be cancelled.
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Request loop.
	for {
		var req Request
		if err := readMsg(conn, &req); err != nil {
			return
		}
		if req.ID == "" {
			continue // fire-and-forget not used on control socket
		}
		result, rerr := cs.dispatch(connCtx, req)
		resp := Response{ID: req.ID}
		if rerr != nil {
			resp.Error = rerr.Error()
		} else {
			resp.Result = result
		}
		if err := writeMsg(conn, resp); err != nil {
			return
		}
	}
}

func (cs *ControlServer) dispatch(ctx context.Context, req Request) (any, error) {
	switch req.Method {
	case "cli.ping":
		return cs.handlePing(), nil

	case "cli.list":
		return cs.handleList()

	case "cli.run":
		return cs.handleRun(ctx, req)

	case "cli.logs":
		return cs.handleLogs(ctx, req)

	case "cli.status":
		return cs.handleStatus(ctx, req)

	case "cli.secrets.list":
		return cs.handleSecretsList(ctx)

	case "cli.secrets.set":
		return nil, cs.handleSecretsSet(ctx, req)

	case "cli.secrets.delete":
		return nil, cs.handleSecretsDelete(ctx, req)

	case "cli.metrics":
		return cs.handleMetrics(), nil

	case "cli.relay.trust_broker":
		return cs.handleTrustBroker(ctx)

	case "cli.ai":
		return cs.handleAI(ctx, req)

	case "cli.relay.rotate_identity":
		return cs.handleRelayRotate(ctx)

	case "cli.task.test":
		return cs.handleTaskTest(ctx, req)

	default:
		return nil, fmt.Errorf("unknown method: %s", req.Method)
	}
}

func (cs *ControlServer) handlePing() DaemonStatus {
	all := cs.reg.All()
	return DaemonStatus{
		Version:   cs.version,
		UptimeSec: int64(time.Since(cs.startedAt).Seconds()),
		TaskCount: len(all),
	}
}

func (cs *ControlServer) handleList() ([]TaskSummary, error) {
	ctx := context.Background()
	specs := cs.reg.All()
	out := make([]TaskSummary, 0, len(specs))
	for _, s := range specs {
		summary := TaskSummary{
			ID:          s.ID,
			Name:        s.Name,
			Description: s.Description,
			Trigger:     triggerLabel(s),
		}
		if runs, err := cs.reg.ListRuns(ctx, s.ID, 1); err == nil && len(runs) > 0 {
			r := runs[0]
			summary.LastStatus = r.Status
			summary.LastRunID = r.ID
			if !r.StartedAt.IsZero() {
				summary.LastRunAt = r.StartedAt.UTC().Format(time.RFC3339)
			}
		}
		out = append(out, summary)
	}
	return out, nil
}

func (cs *ControlServer) handleRun(ctx context.Context, req Request) (RunResult, error) {
	if req.TaskID == "" {
		return RunResult{}, errors.New("taskID required")
	}
	var params map[string]string
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return RunResult{}, fmt.Errorf("params: %w", err)
		}
	}
	runID, err := cs.engine.FireManual(ctx, req.TaskID, params)
	if err != nil {
		return RunResult{}, err
	}
	return cs.engine.WaitRun(ctx, runID)
}

func (cs *ControlServer) handleLogs(ctx context.Context, req Request) ([]LogEntry, error) {
	if req.RunID == "" {
		return nil, errors.New("runID required")
	}
	entries, err := cs.reg.GetRunLogs(ctx, req.RunID)
	if err != nil {
		return nil, err
	}
	out := make([]LogEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, LogEntry{
			RunID:     e.RunID,
			Timestamp: e.Ts.UTC().Format(time.RFC3339),
			Level:     e.Level,
			Message:   e.Message,
		})
	}
	return out, nil
}

func (cs *ControlServer) handleStatus(ctx context.Context, req Request) (any, error) {
	if req.TaskID == "" {
		return cs.handlePing(), nil
	}
	// Return the latest run for the given task.
	runs, err := cs.reg.ListRuns(ctx, req.TaskID, 1)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, fmt.Errorf("no runs found for task %q", req.TaskID)
	}
	return runs[0], nil
}

func (cs *ControlServer) handleSecretsList(ctx context.Context) ([]string, error) {
	if cs.secrets == nil {
		return nil, errors.New("no secrets provider configured")
	}
	return cs.secrets.List(ctx)
}

func (cs *ControlServer) handleSecretsSet(ctx context.Context, req Request) error {
	if cs.secrets == nil {
		return errors.New("no secrets provider configured")
	}
	if req.Key == "" {
		return errors.New("key required")
	}
	return cs.secrets.Set(ctx, req.Key, req.StringValue)
}

func (cs *ControlServer) handleSecretsDelete(ctx context.Context, req Request) error {
	if cs.secrets == nil {
		return errors.New("no secrets provider configured")
	}
	if req.Key == "" {
		return errors.New("key required")
	}
	return cs.secrets.Delete(ctx, req.Key)
}

// MetricsProvider is a pair of functions injected at startup to avoid import
// cycles between pkg/ipc and pkg/metrics / pkg/runtime/deno.
type MetricsProvider struct {
	// ReadDaemon returns current daemon heap/goroutine/CPU metrics.
	ReadDaemon func() (heapAllocMB, heapSysMB float64, goroutines int, cpuMs *int64)
	// ActivePIDs returns the PIDs of live child (Deno) processes.
	ActivePIDs func() []int
	// ReadChildren returns aggregate RSS and CPU for the given PIDs.
	ReadChildren func(pids []int, activeTasks int) (rssTotal float64, cpuTotal *int64)
}

// handleMetrics returns a MetricsSnapshot populated from the runtime/metrics
// package and, on Linux, from /proc. It is wired to the cli.metrics command so
// that the CLI and TUI can query live daemon health over the control socket
// without going through the HTTP API.
func (cs *ControlServer) handleMetrics() MetricsSnapshot {
	var snap MetricsSnapshot
	snap.Tasks.ActiveTasks = cs.engine.ActiveRunCount()
	snap.Tasks.ActiveTaskSlots = cs.engine.ActiveTaskSlots()
	snap.Tasks.MaxConcurrentTasks = cs.engine.MaxConcurrentTasks()
	snap.Tasks.WaitingTasks = cs.engine.WaitingTasks()

	if cs.metricsProvider.ReadDaemon != nil {
		heapAlloc, heapSys, goroutines, cpuMs := cs.metricsProvider.ReadDaemon()
		snap.Daemon.HeapAllocMB = heapAlloc
		snap.Daemon.HeapSysMB = heapSys
		snap.Daemon.Goroutines = goroutines
		snap.Daemon.CPUMs = cpuMs
	}

	if cs.metricsProvider.ActivePIDs != nil && cs.metricsProvider.ReadChildren != nil {
		pids := cs.metricsProvider.ActivePIDs()
		rss, cpu := cs.metricsProvider.ReadChildren(pids, snap.Tasks.ActiveTasks)
		snap.Tasks.ChildRSSMB = rss
		snap.Tasks.ChildCPUMs = cpu
	}

	return snap
}

// handleAI fires the task pointed at by cfg.AI.Task (or an explicit override
// in req.TaskID) with {prompt, session_id} as params, waits for the run to
// finish, and extracts {session_id, reply} from the return value. The task
// must return either a JSON object with "session_id" / "reply" fields, a
// bare string (treated as the reply with an empty session id), or any other
// value (marshalled back into reply as JSON text).
func (cs *ControlServer) handleAI(ctx context.Context, req Request) (AIResult, error) {
	if req.Prompt == "" {
		return AIResult{}, errors.New("prompt required")
	}
	taskID := req.TaskID
	if taskID == "" {
		taskID = cs.defaultAITask
	}
	if taskID == "" {
		return AIResult{}, errors.New("no ai task configured — set ai.task in dicode.yaml or pass --task")
	}
	if _, ok := cs.reg.Get(taskID); !ok {
		return AIResult{}, fmt.Errorf("ai task %q not registered", taskID)
	}

	params := map[string]string{"prompt": req.Prompt}
	if req.SessionID != "" {
		params["session_id"] = req.SessionID
	}

	runID, err := cs.engine.FireManual(ctx, taskID, params)
	if err != nil {
		return AIResult{}, err
	}
	run, err := cs.engine.WaitRun(ctx, runID)
	if err != nil {
		return AIResult{}, err
	}
	out := AIResult{
		TaskID:    taskID,
		RunID:     run.RunID,
		SessionID: req.SessionID,
	}
	if run.Status != "success" {
		// Surface the run id in the error so the CLI user can jump straight
		// to `dicode logs <run-id>` — the control-socket dispatch loop only
		// serialises `out` when err == nil, so TaskID/RunID on out would
		// otherwise be dropped on failure.
		return out, fmt.Errorf("task run %s finished with status %s — see 'dicode logs %s'",
			run.RunID, run.Status, run.RunID)
	}

	// Extract reply + session_id from the return value. Accept both the full
	// ai-agent envelope {session_id, reply} and simpler shapes so alternative
	// tasks don't have to match the buildin schema exactly.
	switch v := run.ReturnValue.(type) {
	case nil:
		// nothing to do — empty reply.
	case string:
		out.Reply = v
	case map[string]any:
		if s, ok := v["reply"].(string); ok {
			out.Reply = s
		}
		// Accept session_id as any scalar — alternative tasks may emit numeric
		// ids that would otherwise be silently dropped (and the user would
		// see a fresh session every turn). fmt.Sprint is nil-safe.
		if sid, ok := v["session_id"]; ok && sid != nil {
			out.SessionID = fmt.Sprint(sid)
		}
	default:
		b, _ := json.Marshal(v)
		out.Reply = string(b)
	}
	return out, nil
}

// handleTrustBroker deletes the TOFU-pinned broker pubkey so the next relay
// reconnect will re-pin whatever the broker announces. This is the recovery
// path when the broker operator rotates their signing key.
func (cs *ControlServer) handleTrustBroker(ctx context.Context) (any, error) {
	if cs.database == nil {
		return nil, fmt.Errorf("database not available")
	}
	if err := relay.ReplaceBrokerPubkey(ctx, cs.database, ""); err != nil {
		return nil, fmt.Errorf("clear broker pubkey: %w", err)
	}
	cs.log.Warn("broker pubkey pin cleared — next relay reconnect will TOFU-pin the new key")
	return map[string]string{
		"status":  "ok",
		"message": "Broker pubkey pin cleared. Restart the daemon (or wait for reconnect) to accept the new broker key.",
	}, nil
}

// SetRelayIdentityRotator wires the rotation callback. The ControlServer
// owns nothing except the function; main.go constructs a closure that holds
// the db, pending store, and relay client, and passes it in after relay
// initialization. Passing nil (or not calling this at all) leaves
// cli.relay.rotate_identity disabled.
//
// Must be called before Start(). There is no synchronisation on the field;
// swapping the rotator after the control socket is accepting connections is
// not supported.
func (cs *ControlServer) SetRelayIdentityRotator(fn RelayIdentityRotator) {
	cs.rotateRelay = fn
}

// RelayRotateResult is returned over the control socket after a successful
// rotation. The new UUID is surfaced so the CLI can print it; the warning
// reminds the operator that live webhook URLs pointing at the old UUID are
// now dead.
type RelayRotateResult struct {
	NewUUID string `json:"new_uuid"`
	Warning string `json:"warning"`
}

func (cs *ControlServer) handleRelayRotate(ctx context.Context) (any, error) {
	if cs.rotateRelay == nil {
		return nil, fmt.Errorf("relay not enabled on this daemon")
	}
	newUUID, err := cs.rotateRelay(ctx)
	if err != nil {
		return nil, fmt.Errorf("rotate: %w", err)
	}
	// The rotator closure in main.go emits a detailed audit entry with
	// old_uuid, new_uuid, and dropped_sessions. We log here too for the
	// control-socket-level audit trail (separate from the relay-level one).
	cs.log.Warn("relay identity rotated via control socket",
		zap.String("new_uuid", newUUID),
	)
	return RelayRotateResult{
		NewUUID: newUUID,
		Warning: relayRotateWarning,
	}, nil
}

// TaskTestResult mirrors pkg/tasktest.Result for the control-socket wire
// shape. Defined here (not imported) so pkg/ipc has no dep on pkg/tasktest,
// keeping the IPC message surface stable if the executor's internals evolve.
type TaskTestResult struct {
	TaskID   string `json:"taskID"`
	Runtime  string `json:"runtime"`
	TestFile string `json:"testFile"`
	Passed   int    `json:"passed"`
	Failed   int    `json:"failed"`
	Skipped  int    `json:"skipped"`
	DurMs    int64  `json:"durationMs"`
	ExitCode int    `json:"exitCode"`
	Output   string `json:"output"`
	Error    string `json:"error,omitempty"`
}

// handleTaskTest resolves the task from the registry, locates its sibling
// test file, and invokes the appropriate runtime's test runner. Phase 1
// supports the Deno runtime only; other runtimes return a clear error.
func (cs *ControlServer) handleTaskTest(ctx context.Context, req Request) (TaskTestResult, error) {
	if req.TaskID == "" {
		return TaskTestResult{}, errors.New("taskID required")
	}
	spec, ok := cs.reg.Get(req.TaskID)
	if !ok {
		return TaskTestResult{}, fmt.Errorf("task %q not found", req.TaskID)
	}
	// tasktest.Run returns both a partial Result and err on certain paths
	// (e.g. unsupported runtime, deno-not-available). Convert the Result to
	// the wire shape regardless so the CLI can surface the partial info.
	res, err := tasktest.Run(ctx, spec)
	wire := TaskTestResult{
		TaskID:   res.TaskID,
		Runtime:  res.Runtime,
		TestFile: res.TestFile,
		Passed:   res.Passed,
		Failed:   res.Failed,
		Skipped:  res.Skipped,
		DurMs:    res.Duration.Milliseconds(),
		ExitCode: res.ExitCode,
		Output:   res.Output,
		Error:    res.Error,
	}
	if err != nil {
		if wire.Error == "" {
			wire.Error = err.Error()
		}
		return wire, err
	}
	return wire, nil
}

// relayRotateWarning is the operator-facing message surfaced to the CLI after
// a successful rotation. It documents the three non-obvious consequences:
// (a) UUID invalidation breaks every shared webhook URL, (b) the still-
// connected WSS session holds the OLD identity in memory until the daemon is
// restarted (so a stolen old key remains impersonation-capable for that
// window), and (c) dicode.oauth.* IPC is now refused until restart (issue
// #144) to avoid handing out new URLs signed under the retired identity.
const relayRotateWarning = "Old UUID is permanently invalidated. " +
	"Any public webhook URLs you previously shared under the old UUID will stop working. " +
	"IMPORTANT: the running relay WSS connection still uses the old key in memory. " +
	"An attacker who has the old key can impersonate this daemon until you restart. " +
	"dicode.oauth.build_auth_url / store_token are refused until the daemon restarts. " +
	"Restart the daemon now to complete the rotation."

// triggerLabel returns a human-readable trigger description for a task spec.
func triggerLabel(s *task.Spec) string {
	t := s.Trigger
	switch {
	case t.Cron != "":
		return "cron:" + t.Cron
	case t.Webhook != "":
		return "webhook:" + t.Webhook
	case t.Daemon:
		return "daemon"
	default:
		return "manual"
	}
}
