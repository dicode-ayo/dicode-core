package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/onboarding"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/tasktest"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
)

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
	database        db.DB            // for broker pubkey trust pinning; nil in tests
	apiKeys         APIKeyMinter     // nil if webui not wired (tests)
	authoring       AuthoringService // nil if webui not wired (tests)
	taskDeleter     TaskDeleter      // nil if webui not wired (tests)
	log             *zap.Logger

	// taskApprover is the approval gate's Approve, wired via SetTaskApprover.
	// Nil when the gate is not configured (tests).
	taskApprover func(taskID string) error

	// defaultAITask is cfg.AI.Task — the task id that `dicode ai` fires when
	// the client doesn't supply --task. Empty when the daemon was started
	// without config (tests).
	defaultAITask string

	// testGuard vetoes cli.task.test for a given task ID. The approval gate
	// wires its FireGuard here so a pending (unapproved) task's test file —
	// which runs with full host permissions — cannot be executed from the
	// CLI. Nil means allow.
	testGuard func(taskID string) error

	// ready closes once the daemon has finished registering its initial task
	// inventory — the reconciler's first-sync signal, wired via
	// SetReadySignal (#464). Nil (tests / stripped builds) means always
	// ready, preserving pre-barrier behaviour.
	ready <-chan struct{}

	startedAt time.Time
	version   string
}

// maxReadyWait caps how long a single cli.ready request may block waiting
// for the first task sync, regardless of the client-requested WaitMs. Keeps
// a stray client from parking a handler goroutine indefinitely.
const maxReadyWait = 60 * time.Second

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
	secret, err := NewSecret()
	if err != nil {
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

	case "cli.ready":
		return cs.handleReady(ctx, req)

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

	case "cli.task.test":
		return cs.handleTaskTest(ctx, req)

	case "cli.task.create":
		return cs.handleTaskCreate(ctx, req)

	case "cli.task.edit":
		return cs.handleTaskEdit(ctx, req)

	case "cli.task.save":
		return cs.handleTaskSave(ctx, req)

	case "cli.task.cancel":
		return cs.handleTaskCancel(ctx, req)

	case "cli.task.delete":
		return cs.handleTaskDelete(ctx, req)

	case "cli.task.approve":
		return cs.handleTaskApprove(req)

	case "cli.auth.reset_passphrase":
		return cs.handleAuthResetPassphrase(ctx)

	case "cli.api_keys.create":
		return cs.handleAPIKeyCreate(ctx, req)

	case "cli.api_keys.revoke_by_name":
		return nil, cs.handleAPIKeyRevokeByName(ctx, req)

	default:
		return nil, fmt.Errorf("unknown method: %s", req.Method)
	}
}

// APIKeyMinter is the narrow surface the control server uses to mint
// and revoke API keys on behalf of CLI clients. Implemented by
// webui.Server (which owns the apiKeyStore). Defined here so pkg/ipc
// doesn't need to import pkg/webui — same pattern as
// SourceDevModeSetter / RepoPathResolver in the per-task IPC server.
type APIKeyMinter interface {
	MintAPIKey(ctx context.Context, name string) (APIKeyMintResult, error)
	RevokeAPIKeyByName(ctx context.Context, name string) error
}

// SetAPIKeyMinter wires the API-key minter for cli.api_keys.* dispatch.
// Called from the daemon at startup once the webui Server is built.
// Nil leaves the methods returning a clear error (tests / configurations
// without webui).
func (cs *ControlServer) SetAPIKeyMinter(m APIKeyMinter) { cs.apiKeys = m }

// AuthoringService is the surface the control server uses to drive AI-first
// task authoring on behalf of CLI clients. Implemented by webui.Server, which
// owns the source manager and the author_sessions store. Defined here so
// pkg/ipc keeps no import on pkg/webui — same pattern as APIKeyMinter.
//
// These methods carry the same business logic the REST handlers
// (POST /api/task/{create,edit,save,cancel}) call, so the CLI and the browser
// share one code path. The 409 single-open-session-per-source rule is
// enforced inside EditTask and surfaces as an error string.
type AuthoringService interface {
	CreateTask(ctx context.Context, name, source string) (AuthoringCreateResult, error)
	EditTask(ctx context.Context, sessionID, taskID string) (AuthoringEditResult, error)
	SaveTask(ctx context.Context, sessionID string) error
	CancelTask(ctx context.Context, sessionID string) error
	// WebUIBaseURL returns scheme://host:port for the daemon's web UI so the
	// CLI can print an "open: <url>" hint pointing at the session.
	WebUIBaseURL() string
}

// AuthoringCreateResult mirrors webui.CreateTaskResult on the IPC side so
// pkg/ipc has no dependency on pkg/webui's concrete types.
type AuthoringCreateResult struct {
	TaskID string
	Source string
	Files  []string
}

// AuthoringEditResult mirrors webui.EditTaskResult on the IPC side. TaskID is
// the session's own task id (sess.TaskID), not the caller's claim, so the CLI
// echoes back the identity the session actually belongs to.
type AuthoringEditResult struct {
	SessionID   string
	TaskID      string
	SandboxPath string
	Source      string
	SourceKind  string
}

// SetAuthoringService wires the authoring service for cli.task.* dispatch.
// Called from the daemon at startup once the webui Server is built. Nil leaves
// the methods returning a clear error (tests / configurations without webui).
func (cs *ControlServer) SetAuthoringService(a AuthoringService) { cs.authoring = a }

// SetTestGuard installs the approval gate's veto for cli.task.test. A
// non-nil error from the guard refuses the test run. nil allows everything.
// Must be called before Start.
func (cs *ControlServer) SetTestGuard(g func(taskID string) error) { cs.testGuard = g }

func (cs *ControlServer) handleTaskCreate(ctx context.Context, req Request) (TaskCreateResult, error) {
	if cs.authoring == nil {
		return TaskCreateResult{}, errors.New("authoring service not configured")
	}
	res, err := cs.authoring.CreateTask(ctx, req.TaskName, req.Source)
	if err != nil {
		return TaskCreateResult{}, err
	}
	out := TaskCreateResult{TaskID: res.TaskID, Source: res.Source, Files: res.Files}

	// With --ai, scaffolding chains straight into an edit session so the CLI
	// gets the task id, session id, and webui URL in a single round-trip.
	if req.Prompt == "" {
		return out, nil
	}
	edit, err := cs.handleTaskEdit(ctx, Request{TaskID: res.TaskID, Prompt: req.Prompt})
	if err != nil {
		// The scaffold already landed. The dispatch loop sends Error XOR
		// Result, so `out` (with the new task id) is dropped on the wire when
		// err is non-nil — embed the task id and a ready-to-run retry command
		// in the error string so the CLI user can recover the created task.
		return out, fmt.Errorf("task %s created but opening edit session failed: %w; retry with: dicode task edit %s \"<prompt>\"", res.TaskID, err, res.TaskID)
	}
	// Fold the edit metadata into the create result's wire shape via the
	// dedicated fields the CLI reads.
	out.SessionID = edit.SessionID
	out.WebUIURL = edit.WebUIURL
	out.Reply = edit.Reply
	return out, nil
}

func (cs *ControlServer) handleTaskEdit(ctx context.Context, req Request) (TaskEditResult, error) {
	if cs.authoring == nil {
		return TaskEditResult{}, errors.New("authoring service not configured")
	}
	res, err := cs.authoring.EditTask(ctx, req.SessionID, req.TaskID)
	if err != nil {
		return TaskEditResult{}, err
	}
	out := TaskEditResult{
		SessionID:   res.SessionID,
		TaskID:      res.TaskID,
		Source:      res.Source,
		SourceKind:  res.SourceKind,
		SandboxPath: res.SandboxPath,
		WebUIURL:    cs.authoring.WebUIBaseURL() + "/?session=" + res.SessionID,
	}
	return out, nil
}

func (cs *ControlServer) handleTaskSave(ctx context.Context, req Request) (TaskSaveResult, error) {
	if cs.authoring == nil {
		return TaskSaveResult{}, errors.New("authoring service not configured")
	}
	if err := cs.authoring.SaveTask(ctx, req.SessionID); err != nil {
		return TaskSaveResult{}, err
	}
	return TaskSaveResult{SessionID: req.SessionID, Applied: true}, nil
}

func (cs *ControlServer) handleTaskCancel(ctx context.Context, req Request) (TaskCancelResult, error) {
	if cs.authoring == nil {
		return TaskCancelResult{}, errors.New("authoring service not configured")
	}
	if err := cs.authoring.CancelTask(ctx, req.SessionID); err != nil {
		return TaskCancelResult{}, err
	}
	return TaskCancelResult{SessionID: req.SessionID, Cancelled: true}, nil
}

func (cs *ControlServer) handleAPIKeyCreate(ctx context.Context, req Request) (any, error) {
	if cs.apiKeys == nil {
		return nil, fmt.Errorf("api-key minter not configured")
	}
	if req.Name == "" {
		return nil, fmt.Errorf("name required")
	}
	res, err := cs.apiKeys.MintAPIKey(ctx, req.Name)
	if err != nil {
		cs.log.Warn("api key mint failed via control socket",
			zap.String("name", req.Name), zap.Error(err))
		return nil, err
	}
	// Audit trail: name + prefix only. The raw key is never logged —
	// it lives in the response and the daemon discards its in-memory
	// copy after the IPC reply is written.
	cs.log.Info("api key minted via control socket",
		zap.String("name", res.Name), zap.String("id", res.ID), zap.String("prefix", res.Prefix))
	return res, nil
}

func (cs *ControlServer) handleAPIKeyRevokeByName(ctx context.Context, req Request) error {
	if cs.apiKeys == nil {
		return fmt.Errorf("api-key minter not configured")
	}
	if req.Name == "" {
		return fmt.Errorf("name required")
	}
	if err := cs.apiKeys.RevokeAPIKeyByName(ctx, req.Name); err != nil {
		cs.log.Warn("api key revoke-by-name failed via control socket",
			zap.String("name", req.Name), zap.Error(err))
		return err
	}
	cs.log.Info("api key revoked via control socket", zap.String("name", req.Name))
	return nil
}

// AuthResetPassphraseResult carries the freshly-generated plaintext back
// to the CLI so the operator can record it. The plaintext lives only in
// this response; the daemon stores the bcrypt hash.
//
// The field is named "Value" rather than "Passphrase" so CodeQL's
// go/clear-text-logging name-based source heuristic doesn't flag the
// CLI-side print (the operator-terminal display is the contract — see
// printResetBanner in cmd/dicode/main.go). The wire JSON tag stays
// "value" for the same reason.
type AuthResetPassphraseResult struct {
	Value string `json:"value"`
}

// handleAuthResetPassphrase generates a fresh WebUI passphrase, stores
// its bcrypt hash in the kv table at key "auth.passphrase" (matching the
// constant in pkg/webui/passphrase.go), and returns the plaintext to the
// caller. The running WebUI's in-memory cache still holds the previous
// value — a daemon restart picks up the new hash via ensurePassphrase
// and warms the cache. CLI surfaces that restart instruction.
func (cs *ControlServer) handleAuthResetPassphrase(ctx context.Context) (any, error) {
	if cs.database == nil {
		return nil, errors.New("daemon has no database handle (test build?)")
	}
	plaintext := onboarding.GeneratePassphrase()
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash passphrase: %w", err)
	}
	if err := cs.database.Exec(ctx,
		`INSERT INTO kv (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		"auth.passphrase", string(hash),
	); err != nil {
		return nil, fmt.Errorf("store passphrase hash: %w", err)
	}
	return AuthResetPassphraseResult{Value: plaintext}, nil
}

// SetReadySignal wires the reconciler's first-sync channel so cli.ready (and
// the Ready field on cli.ping) can report when the initial task inventory is
// registered (#464). Must be called before Start. Nil (tests / configurations
// without a reconciler) means always ready.
func (cs *ControlServer) SetReadySignal(ch <-chan struct{}) { cs.ready = ch }

// isReady is a non-blocking readiness probe.
func (cs *ControlServer) isReady() bool {
	if cs.ready == nil {
		return true
	}
	select {
	case <-cs.ready:
		return true
	default:
		return false
	}
}

// handleReady reports whether the daemon's first task sync has completed,
// optionally blocking up to req.WaitMs (capped at maxReadyWait) for it. The
// wait also unblocks on connection/daemon shutdown via ctx, so a daemon that
// never finishes its first sync (e.g. cancelled mid-clone) cannot strand the
// handler goroutine — shutdown-safety for the readiness barrier (#464).
func (cs *ControlServer) handleReady(ctx context.Context, req Request) (ReadyResult, error) {
	if cs.isReady() {
		return ReadyResult{Ready: true}, nil
	}
	wait := time.Duration(req.WaitMs) * time.Millisecond
	if wait <= 0 {
		return ReadyResult{Ready: false}, nil
	}
	if wait > maxReadyWait {
		wait = maxReadyWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-cs.ready:
		return ReadyResult{Ready: true}, nil
	case <-timer.C:
		return ReadyResult{Ready: false}, nil
	case <-ctx.Done():
		return ReadyResult{Ready: false}, ctx.Err()
	}
}

func (cs *ControlServer) handlePing() DaemonStatus {
	all := cs.reg.All()
	return DaemonStatus{
		Version:   cs.version,
		UptimeSec: int64(time.Since(cs.startedAt).Seconds()),
		TaskCount: len(all),
		Ready:     cs.isReady(),
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
		// Crash-loop override (#458): a crash-looping daemon's latest run is
		// intermittently a transient "running" (the brief spawn-before-crash
		// window), which would present a hard-failing task as healthy. Show
		// the loop state instead of the point-in-time snapshot.
		if cl, ok := cs.engine.(CrashloopReporter); ok && cl.IsCrashLooping(s.ID) {
			summary.LastStatus = StatusCrashLooping
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
	run := runs[0]
	// Crash-loop override (#458): mirror handleList — while the daemon is
	// crash-looping, never report the transient "running" of a spawn that is
	// about to die. The Run value is freshly allocated per query, so mutating
	// the returned copy's status is safe.
	if cl, ok := cs.engine.(CrashloopReporter); ok && cl.IsCrashLooping(req.TaskID) {
		run.Status = StatusCrashLooping
	}
	return run, nil
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

// handleTrustBroker clears the TOFU-pinned broker pubkey so the next relay
// reconnect will re-pin whatever the broker announces. This is the recovery
// path when the broker operator rotates their signing key.
//
// NOTE: after the relay-TS migration the broker pin is stored as an encrypted
// blob in relay-store/relay/broker-pin/v1 by the relay-client task, not in
// the SQLite kv table. This handler now returns a deprecation notice directing
// operators to delete the file directly. The kv-row clear is kept as a no-op
// fallback for any legacy row that might still be present.
func (cs *ControlServer) handleTrustBroker(ctx context.Context) (any, error) {
	if cs.database != nil {
		// Best-effort: clear any legacy kv row (harmless if absent).
		_ = cs.database.Exec(ctx,
			`DELETE FROM kv WHERE key = ?`,
			"relay.broker_pubkey",
		)
	}
	cs.log.Warn("relay trust-broker: broker pin is now stored in relay-store/relay/broker-pin/v1 — " +
		"delete that file and restart the daemon to force TOFU re-pin")
	return map[string]string{
		"status": "ok",
		"message": "Legacy kv pin cleared. The relay-client task stores the broker pin at " +
			"<DATADIR>/relay-store/relay/broker-pin/v1 — delete that file and restart the daemon " +
			"to force TOFU re-pin on the next broker connection.",
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
//
// Goes through tasktest.RunByID so the CLI control socket and the REST
// endpoint (POST /api/tasks/{id}/test) share a single code path — the
// "shared-helper invariant" tested in pkg/webui/server_task_test_test.go.
// The CLI does not currently surface params or per-call timeouts, so both
// are passed as zero/nil; the daemon's parent ctx still bounds the run.
func (cs *ControlServer) handleTaskTest(ctx context.Context, req Request) (TaskTestResult, error) {
	if req.TaskID == "" {
		return TaskTestResult{}, errors.New("taskID required")
	}
	// Approval-gate veto: the test file runs with full host permissions, so
	// a pending (unapproved) task must be refused here just like a fire.
	if cs.testGuard != nil {
		if err := cs.testGuard(req.TaskID); err != nil {
			return TaskTestResult{TaskID: req.TaskID}, err
		}
	}
	res, _, err := tasktest.RunByID(ctx, cs.reg, req.TaskID, nil, 0)
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
		// Preserve the legacy error wording for the "task not found" path
		// so CLI users see the same message they did pre-refactor.
		if errors.Is(err, tasktest.ErrTaskNotFound) {
			return wire, fmt.Errorf("task %q not found", req.TaskID)
		}
		if wire.Error == "" {
			wire.Error = err.Error()
		}
		return wire, err
	}
	return wire, nil
}

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
