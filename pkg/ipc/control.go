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
	"go.uber.org/zap"
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
	database        db.DB           // for broker pubkey trust pinning; nil in tests
	log             *zap.Logger

	startedAt time.Time
	version   string

	// relayHooks provides optional accessors for the daemon's relay identity
	// and claim actions. Wired by the daemon after the relay client is built;
	// nil when the relay feature is disabled.
	relayHooks RelayHooks
}

// RelayHooks is the ControlServer's interface to the relay subsystem. Passing
// these in via SetRelayHooks keeps ControlServer ignorant of pkg/relay, which
// avoids an import cycle and keeps IPC tests side-effect-free.
type RelayHooks struct {
	// Status returns the current linkage state for cli.status. Must be safe to
	// call with nil receiver state and must never return an error that blocks
	// the status response — return (RelayStatus{}, nil) on best effort.
	Status func(ctx context.Context) (RelayStatus, error)

	// Login performs the daemon claim flow. Returns the resulting user login
	// and UUID on success, or a descriptive error otherwise. Must never log
	// the claim token.
	Login func(ctx context.Context, claimToken, label, baseURLOverride string) (RelayLoginResult, error)
}

// SetRelayHooks wires the relay accessors into the control server. Call this
// after NewControlServer if the daemon has initialised its relay identity.
func (cs *ControlServer) SetRelayHooks(h RelayHooks) {
	cs.relayHooks = h
}

// NewControlServer creates a ControlServer. Call Start to begin accepting
// connections. socketPath is the Unix socket path; tokenPath is where the CLI
// token is written.
func NewControlServer(
	socketPath, tokenPath string,
	reg *registry.Registry,
	engine EngineRunner,
	secretsMgr secrets.Manager,
	mp MetricsProvider,
	version string,
	log *zap.Logger,
	database db.DB,
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
	if err := writeTokenFile(tokenPath, tok); err != nil {
		return nil, fmt.Errorf("control: write token file: %w", err)
	}
	return cs, nil
}

// writeTokenFile writes the token to path atomically (tmp + rename, mode 0600).
func writeTokenFile(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
	claims, err := VerifyToken(cs.secret, hs.Token)
	if err != nil || claims.Identity != "cli" {
		_ = writeMsg(conn, handshakeErr{Error: "invalid token"})
		return
	}
	_ = writeMsg(conn, handshakeResp{Proto: 1, Caps: claims.Caps})
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

	case "cli.relay.login":
		return cs.handleRelayLogin(ctx, req)

	default:
		return nil, fmt.Errorf("unknown method: %s", req.Method)
	}
}

func (cs *ControlServer) handlePing() DaemonStatus {
	all := cs.reg.All()
	status := DaemonStatus{
		Version:   cs.version,
		UptimeSec: int64(time.Since(cs.startedAt).Seconds()),
		TaskCount: len(all),
	}
	if cs.relayHooks.Status != nil {
		// Best effort — never block ping on a relay status failure.
		if rs, err := cs.relayHooks.Status(context.Background()); err == nil {
			status.Relay = rs
		}
	}
	return status
}

func (cs *ControlServer) handleRelayLogin(ctx context.Context, req Request) (any, error) {
	if cs.relayHooks.Login == nil {
		return nil, errors.New("relay is not enabled on this daemon")
	}
	if req.ClaimToken == "" {
		return nil, errors.New("claimToken required")
	}
	return cs.relayHooks.Login(ctx, req.ClaimToken, req.Label, req.BaseURL)
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
