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
	"sync"
	"time"

	"github.com/dicode/dicode/pkg/registry"
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

	reg     *registry.Registry
	engine  EngineRunner
	secrets secrets.Manager // nil if no local provider configured
	log     *zap.Logger

	startedAt time.Time
	version   string

	listener net.Listener
	mu       sync.Mutex
	done     chan struct{}
}

// NewControlServer creates a ControlServer. Call Start to begin accepting
// connections. socketPath is the Unix socket path; tokenPath is where the CLI
// token is written.
func NewControlServer(
	socketPath, tokenPath string,
	reg *registry.Registry,
	engine EngineRunner,
	secretsMgr secrets.Manager,
	version string,
	log *zap.Logger,
) (*ControlServer, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("control: generate secret: %w", err)
	}

	cs := &ControlServer{
		socketPath: socketPath,
		tokenPath:  tokenPath,
		secret:     secret,
		reg:        reg,
		engine:     engine,
		secrets:    secretsMgr,
		log:        log,
		startedAt:  time.Now(),
		version:    version,
		done:       make(chan struct{}),
	}

	// Issue the CLI token and write it atomically.
	tok, err := IssueToken(secret, "cli", "cli", cliCaps())
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
	cs.mu.Lock()
	cs.listener = ln
	cs.mu.Unlock()

	cs.log.Info("control socket ready", zap.String("path", cs.socketPath))

	go func() {
		<-ctx.Done()
		ln.Close()
		close(cs.done)
		_ = os.Remove(cs.socketPath)
		_ = os.Remove(cs.tokenPath)
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-cs.done:
				return nil
			default:
				cs.log.Warn("control: accept error", zap.Error(err))
				continue
			}
		}
		go cs.handleConn(conn)
	}
}

// handleConn runs the handshake and then the request loop for one CLI client.
func (cs *ControlServer) handleConn(conn net.Conn) {
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

	// Request loop.
	for {
		var req Request
		if err := readMsg(conn, &req); err != nil {
			return
		}
		if req.ID == "" {
			continue // fire-and-forget not used on control socket
		}
		result, rerr := cs.dispatch(req)
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

func (cs *ControlServer) dispatch(req Request) (any, error) {
	ctx := context.Background()
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
	specs := cs.reg.All()
	out := make([]TaskSummary, 0, len(specs))
	for _, s := range specs {
		summary := TaskSummary{
			ID:          s.ID,
			Name:        s.Name,
			Description: s.Description,
			Trigger:     triggerLabel(s),
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
