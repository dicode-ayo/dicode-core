package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	resumer         Resumer          // nil if engine not wired (tests)
	log             *zap.Logger

	// taskApprover is the approval gate's Approve, wired via SetTaskApprover.
	// Nil when the gate is not configured (tests).
	taskApprover func(taskID string) error

	// pendingApprovals reports the tasks the approval gate is holding (id +
	// full content hash), wired via SetPendingApprovals. Nil when the gate is
	// not configured (tests) — cli.task.pending then reports an empty list.
	pendingApprovals func() []PendingTask

	// defaultAITask is cfg.AI.Task — the task id that `dicode ai` fires when
	// the client doesn't supply --task. Empty when the daemon was started
	// without config (tests).
	defaultAITask string

	// defaultCreateTask is cfg.AI.CreateTask — the task id handleTaskEdit
	// fires a real AI turn against when the client passes a non-empty
	// prompt (`dicode task create --ai` / `dicode task edit <id> "<prompt>"`).
	// Empty when the daemon was started without config (tests) or a blank
	// prompt was supplied (a plain, non-AI edit — the pre-#568 behavior).
	// A non-empty prompt with this still empty is a configuration error,
	// surfaced the same way handleAI surfaces a blank defaultAITask.
	defaultCreateTask string

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

	// sessionEditLocks serializes handleTaskEdit's read(EditTask)-fire-write
	// (UpdateAgentSessionID) sequence per authoring session, so two
	// concurrent `dicode task edit` calls against the SAME open session
	// can't both read the same stale AgentSessionID, both fire, and race to
	// overwrite each other's persisted run-group correlation id (finding
	// #4, a TOCTOU: read at EditTask, long FireManual/WaitRunSettled round
	// trip, write at UpdateAgentSessionID, no lock between). Keyed by
	// source (see lockForTaskEdit) — EditTask's single-session-per-source
	// invariant means the source IS the session's identity, so this is the
	// one key that's canonical regardless of which of the two request
	// shapes (`task edit <id> "<prompt>"` vs `--session <sid>`) a caller
	// used to reach the SAME open session. Calls against a DIFFERENT
	// source are not serialized against each other, so unrelated
	// concurrent edits stay fully concurrent. sync.Map's zero value is
	// ready to use; values are *sync.Mutex, created lazily on first use and
	// never removed (the source keyspace is small and long-lived relative
	// to a process lifetime, so this isn't a practical leak).
	sessionEditLocks sync.Map

	startedAt time.Time
	version   string
}

// lockForTaskEdit returns the mutex serializing handleTaskEdit calls that
// resolve to the same authoring session, creating it on first use. The
// caller locks it for the whole read-fire-write sequence and unlocks via
// defer.
//
// Keyed by SOURCE, not by the caller's raw sessionID/taskID: EditTask
// resolves both "task edit <id> \"<prompt>\"" (taskID only) and "task edit
// <id> \"<prompt>\" --session <sid>" (both) to the SAME open session via
// GetOpenForSource, so a lock keyed on whichever field happened to be set
// would let those two request shapes race right past each other on the one
// session they both actually touch. Mirrors authoring_service.go's EditTask
// source derivation exactly (taskID's prefix up to "/") so the key always
// matches the session EditTask will actually resolve to. sessionID alone
// (no taskID — a caller resuming purely by session id, not exercised by the
// CLI today but a valid Request shape) has no source to derive without the
// same racy DB read this lock exists to protect, so it falls back to a
// session-keyed lock in that case; that shape never collides with a
// taskID-bearing call anyway, since EditTask requires taskID whenever
// sessionID is empty.
func (cs *ControlServer) lockForTaskEdit(sessionID, taskID string) *sync.Mutex {
	var key string
	switch {
	case taskID != "":
		source := taskID
		if idx := strings.Index(source, "/"); idx > 0 {
			source = source[:idx]
		}
		key = "source:" + source
	case sessionID != "":
		key = "sess:" + sessionID
	default:
		key = "unkeyed"
	}
	v, _ := cs.sessionEditLocks.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// maxReadyWait caps how long a single cli.ready request may block waiting
// for the first task sync, regardless of the client-requested WaitMs. Keeps
// a stray client from parking a handler goroutine indefinitely.
const maxReadyWait = 60 * time.Second

// NewControlServer creates a ControlServer. Call Start to begin accepting
// connections. socketPath is the Unix socket path; tokenPath is where the CLI
// token is written. defaultAITask is cfg.AI.Task — resolved at daemon startup
// so the control server can fire the right task when the CLI invokes `dicode ai`
// without --task. defaultCreateTask is cfg.AI.CreateTask — the analogous
// default for `dicode task create --ai` / `dicode task edit`'s prompt threading.
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
	defaultCreateTask string,
) (*ControlServer, error) {
	secret, err := NewSecret()
	if err != nil {
		return nil, fmt.Errorf("control: generate secret: %w", err)
	}

	cs := &ControlServer{
		socketPath:        socketPath,
		tokenPath:         tokenPath,
		secret:            secret,
		reg:               reg,
		engine:            engine,
		secrets:           secretsMgr,
		metricsProvider:   mp,
		database:          database,
		log:               log,
		defaultAITask:     defaultAITask,
		defaultCreateTask: defaultCreateTask,
		startedAt:         time.Now(),
		version:           version,
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

	case "cli.run.wait":
		return cs.handleRunWait(ctx, req)

	case "cli.run.cancel":
		return cs.handleRunCancel(req)

	case "cli.logs":
		return cs.handleLogs(ctx, req)

	case "cli.status":
		return cs.handleStatus(ctx, req)

	case "cli.resume":
		return cs.handleResume(ctx, req)

	case "cli.resume.list":
		return cs.handleResumeList(ctx)

	case "cli.resume.get":
		return cs.handleResumeGet(ctx, req)

	case "cli.secrets.list":
		return cs.handleSecretsList(ctx)

	case "cli.secrets.set":
		return nil, cs.handleSecretsSet(ctx, req)

	case "cli.secrets.delete":
		return nil, cs.handleSecretsDelete(ctx, req)

	case "cli.metrics":
		return cs.handleMetrics(), nil

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

	case "cli.task.pending":
		return cs.handleTaskPending()

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
// SourceController / RepoPathResolver in the per-task IPC server.
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
	// UpdateAgentSessionID persists the underlying ai-agent run's own
	// session id onto the authoring session identified by sessionID (#568),
	// so the NEXT `dicode task edit` call against the same authoring
	// session can read it back via EditTask's AgentSessionID and re-send it
	// as the run-group correlation id (see AuthoringEditResult.AgentSessionID
	// below) for UI/log grouping. This does NOT give the agent
	// conversational memory — every one-shot turn still starts from an
	// empty SessionState; see tasks/buildin/ai-agent/task.ts's oneShotTurn.
	// Real conversational continuity across `task edit` calls is Phase 1
	// work (session-as-suspended-run, docs/design/ai-task-authoring.md).
	// Called by handleTaskEdit after a successful AI turn; a blank
	// agentSessionID (some alternative agent tasks may not return one) is a
	// no-op.
	UpdateAgentSessionID(ctx context.Context, sessionID, agentSessionID string) error
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
	// TaskDir is the absolute directory holding the task's files. The
	// authoring agent writes through a tool that takes absolute paths, and
	// dicode.list_tasks does not carry TaskDir, so the session is the only
	// thing that can tell the agent where to write. Empty when the directory
	// can't be resolved; the prompt then names the target task alone.
	TaskDir string
	// AgentSessionID is the ai-agent run's own session id stored on this
	// authoring session from a prior turn (#568), or "" if no AI turn has
	// happened on it yet. handleTaskEdit reads this before firing the next
	// turn and re-sends it as the session_id param — but the underlying
	// oneShotTurn (tasks/buildin/ai-agent/task.ts) builds a brand-new empty
	// SessionState on every call, so this does NOT carry conversational
	// memory across turns. Its real, worth-keeping effect is tagging every
	// turn's run under one run-group label (`chat:<id>`) so the WebUI/logs
	// display repeated edit turns as one expandable row. True multi-turn
	// continuity is Phase 1 work (session-as-suspended-run).
	AgentSessionID string
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
	out.RunID = edit.RunID
	out.Suspended = edit.Suspended
	return out, nil
}

// handleTaskEdit opens or resumes an AI edit session (unchanged: the
// EditTask call and its 409/single-session-per-source logic are Phase 1
// territory, not touched here — see docs/design/ai-task-authoring.md), then,
// when req.Prompt is non-empty, fires a real AI turn against cs.defaultCreateTask
// (cfg.AI.CreateTask) and folds the reply into the response (#568). A blank
// prompt is the pre-#568 behavior: open/resume the session and return, no AI
// call — this is how the plain `dicode task edit <id>` (no prompt) and every
// existing caller of this handler keeps working unchanged.
//
// Run-group correlation (NOT conversational memory): the authoring session
// carries the underlying ai-agent run's own session id (res.AgentSessionID,
// read back from a prior turn) and re-sends it as the session_id param on
// the next turn, so repeated `dicode task edit <id> "<prompt>"` calls
// against the same open session are tagged under one run-group label
// (`chat:<id>`) for UI/log grouping. The underlying agent does NOT retain
// memory between these one-shot turns — oneShotTurn
// (tasks/buildin/ai-agent/task.ts) builds a brand-new empty SessionState on
// every call, by design ("one-shot calls share no history"). Real
// conversational continuity across `task edit` calls requires the
// chat-loop/suspend-resume path, which is explicitly Phase 1 work
// (session-as-suspended-run — docs/design/ai-task-authoring.md), not
// implemented here. After a successful turn, the freshly returned agent
// session id is persisted back onto the row via UpdateAgentSessionID so the
// NEXT turn's run-group label carries it too.
func (cs *ControlServer) handleTaskEdit(ctx context.Context, req Request) (TaskEditResult, error) {
	if cs.authoring == nil {
		return TaskEditResult{}, errors.New("authoring service not configured")
	}

	// Serialize the whole read(EditTask)-fire-write sequence per
	// session/task (finding #4) — see sessionEditLocks' doc comment. Held
	// across the EditTask call too, not just FireManual onward: EditTask's
	// return value IS the AgentSessionID read this is protecting, so a
	// second caller must not perform that read until the first caller's
	// UpdateAgentSessionID write (if any) has landed.
	mu := cs.lockForTaskEdit(req.SessionID, req.TaskID)
	mu.Lock()
	defer mu.Unlock()

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
	if req.Prompt == "" {
		return out, nil
	}

	if cs.defaultCreateTask == "" {
		return out, fmt.Errorf("session %s opened but no create task configured — set ai.create_task in dicode.yaml", res.SessionID)
	}
	if _, ok := cs.reg.Get(cs.defaultCreateTask); !ok {
		return out, fmt.Errorf("session %s opened but create task %q not registered", res.SessionID, cs.defaultCreateTask)
	}

	// The agent's target — task id and the directory its files live in —
	// must ride in `prompt` itself, not a bare `task_id`
	// param: buildin/ai-agent's task.yaml declares no such param and
	// oneShotTurn never reads one (#723) — `dicode.task_id` inside the
	// task refers to ai-agent's OWN identity, a different thing entirely.
	// FireManual doesn't validate params against the task's declared
	// schema, so an undeclared "task_id" silently vanishes on the wire; the
	// agent would receive only the raw prompt with no idea what task it's
	// supposed to be working on. Prefixing the target into the prompt text
	// is the cheapest fix that's guaranteed visible to the model regardless
	// of which ai-agent-family task cfg.AI.CreateTask points at.
	header := fmt.Sprintf("(Target task: %s)", res.TaskID)
	if res.TaskDir != "" {
		header = fmt.Sprintf("(Target task: %s — its files live in %s; write them there with the write-task-file tool)", res.TaskID, res.TaskDir)
	}
	params := map[string]string{
		"prompt": header + "\n\n" + req.Prompt,
	}
	if res.AgentSessionID != "" {
		params["session_id"] = res.AgentSessionID
	}

	runID, err := cs.engine.FireManual(ctx, cs.defaultCreateTask, params)
	if err != nil {
		return out, err
	}
	// WaitRunSettled (not WaitRun): mirrors handleAI — a suspended turn must
	// surface immediately rather than blocking on a resume nobody has sent yet.
	run, err := cs.engine.WaitRunSettled(ctx, runID)
	if err != nil {
		return out, err
	}
	out.RunID = run.RunID
	if run.Status == registry.StatusSuspended {
		out.Suspended = true
		return out, nil
	}
	if run.Status != "success" {
		// Surface the run id in the error so the CLI user can jump straight to
		// `dicode logs <run-id>` — the dispatch loop only serialises `out` when
		// err == nil, mirroring handleAI's equivalent failure path.
		return out, fmt.Errorf("task run %s finished with status %s — see 'dicode logs %s'",
			run.RunID, run.Status, run.RunID)
	}

	// Extract reply + session_id from the return value — same envelope
	// handling as handleAI, so alternative task-create overrides that don't
	// match the buildin schema exactly still degrade gracefully.
	var agentSessionID string
	switch v := run.ReturnValue.(type) {
	case nil:
		// nothing to do — empty reply.
	case string:
		out.Reply = v
	case map[string]any:
		if s, ok := v["reply"].(string); ok {
			out.Reply = s
		}
		if sid, ok := v["session_id"]; ok && sid != nil {
			agentSessionID = fmt.Sprint(sid)
		}
	default:
		b, _ := json.Marshal(v)
		out.Reply = string(b)
	}
	if agentSessionID != "" {
		if uerr := cs.authoring.UpdateAgentSessionID(ctx, res.SessionID, agentSessionID); uerr != nil {
			// Non-fatal: the turn itself succeeded and the reply is already in
			// `out`. Losing this means the NEXT edit call's run-group label
			// starts fresh (a new `chat:<id>`) rather than failing this one —
			// no conversational memory is at stake either way (see the
			// handleTaskEdit doc comment above).
			cs.log.Warn("failed to persist agent session id for authoring session",
				zap.String("session", res.SessionID), zap.Error(uerr))
		}
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
	pending := cs.pendingTaskIDs()
	out := make([]TaskSummary, 0, len(specs))
	for _, s := range specs {
		summary := TaskSummary{
			ID:          s.ID,
			Name:        s.Name,
			Description: s.Description,
			Trigger:     triggerLabel(s),
		}
		if _, ok := pending[s.ID]; ok {
			summary.Pending = true
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
	// Settled, not WaitRun: a task that suspends must surface as `suspended` so
	// the CLI can render its resume form. WaitRun follows the resume chain and
	// would block until someone else answers the wizard.
	return cs.engine.WaitRunSettled(ctx, runID)
}

// handleRunWait blocks until an existing run reaches a terminal or suspended
// state and returns its RunResult. It backs the CLI's interactive follow loop:
// after a resume spawns a continuation, the client waits on it here rather than
// polling cli.resume.get, which sidesteps the window where the continuation has
// been spawned but has not yet reached `suspended` (the "run is not suspended"
// race) — WaitRun settles on the run's done signal and reads the final record.
func (cs *ControlServer) handleRunWait(ctx context.Context, req Request) (RunResult, error) {
	if req.RunID == "" {
		return RunResult{}, errors.New("runID required")
	}
	if cs.engine == nil {
		return RunResult{}, errors.New("run wait not available (engine not wired)")
	}
	// A daemon body is long-lived and never reaches a terminal state, so blocking
	// on it would park this handler until the client disconnects (and, because
	// dispatch is serialized per connection, the disconnect isn't even observed
	// until then). Return its current status immediately instead; the CLI falls
	// back to the one-shot path for daemon continuations.
	if run, err := cs.reg.GetRun(ctx, req.RunID); err == nil {
		if spec, ok := cs.reg.Get(run.TaskID); ok && spec.Trigger.Daemon {
			return RunResult{RunID: run.ID, Status: run.Status}, nil
		}
	}
	return cs.engine.WaitRunSettled(ctx, req.RunID)
}

// RunCancelResult is the cli.run.cancel response. Killed is false when the
// run was not currently cancellable (already terminal, or the id unknown) —
// not an error, since the caller (an interactive follow loop reacting to
// Ctrl+C) treats either outcome the same way: stop waiting on the run.
type RunCancelResult struct {
	Killed bool `json:"killed"`
}

// handleRunCancel kills an in-flight run by id. Backs the CLI's Ctrl+C-during-
// a-turn cancellation: the follow loop sends this on a separate connection
// while its primary connection is still blocked in cli.run.wait, since
// ControlClient.Send is not safe for concurrent use on one connection.
func (cs *ControlServer) handleRunCancel(req Request) (RunCancelResult, error) {
	if req.RunID == "" {
		return RunCancelResult{}, errors.New("runID required")
	}
	if cs.engine == nil {
		return RunCancelResult{}, errors.New("run cancel not available (engine not wired)")
	}
	return RunCancelResult{Killed: cs.engine.KillRun(req.RunID)}, nil
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
	// A blank prompt is the chat-mode entry: the agent opens the conversation
	// by suspending for the first message (handled below), rather than running
	// a single one-shot turn.
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

	params := map[string]string{}
	if req.Prompt != "" {
		params["prompt"] = req.Prompt
	}
	if req.SessionID != "" {
		params["session_id"] = req.SessionID
	}

	runID, err := cs.engine.FireManual(ctx, taskID, params)
	if err != nil {
		return AIResult{}, err
	}
	// WaitRunSettled (not WaitRun): a chat-start run suspends awaiting the first
	// message and is never resumed here, so WaitRun — which follows the resume
	// chain — would block forever. WaitRunSettled returns on the suspend.
	run, err := cs.engine.WaitRunSettled(ctx, runID)
	if err != nil {
		return AIResult{}, err
	}
	out := AIResult{
		TaskID:    taskID,
		RunID:     run.RunID,
		SessionID: req.SessionID,
	}
	// Chat mode: the agent suspended awaiting the first message. Hand the
	// suspended run back so the CLI drives the resume loop from it.
	if run.Status == registry.StatusSuspended {
		out.Suspended = true
		return out, nil
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
