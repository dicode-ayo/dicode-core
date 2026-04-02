package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/dicode/dicode/pkg/db"
	mcpclient "github.com/dicode/dicode/pkg/mcp/client"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
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
	log      *zap.Logger

	aiBaseURL string
	aiModel   string
	aiAPIKey  string

	ctx        context.Context
	socketPath string
	listener   net.Listener

	mu     sync.Mutex
	output *OutputResult
	retCh  chan any
}

// New creates a Server (not yet started).
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
	aiBaseURL, aiModel, aiAPIKey string,
) *Server {
	return &Server{
		runID:     runID,
		taskID:    taskID,
		secret:    secret,
		registry:  reg,
		db:        database,
		params:    params,
		input:     input,
		spec:      spec,
		engine:    engine,
		log:       log,
		aiBaseURL: aiBaseURL,
		aiModel:   aiModel,
		aiAPIKey:  aiAPIKey,
		retCh:     make(chan any, 1),
	}
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
	caps := defaultTaskCaps()
	if s.spec != nil && s.spec.Security != nil && len(s.spec.Security.AllowedTasks) > 0 {
		caps = append(caps, CapTaskTrigger)
	}
	if s.spec != nil && s.spec.Security != nil && len(s.spec.Security.AllowedMCP) > 0 {
		caps = append(caps, CapMCPCall)
	}

	token, err = IssueToken(s.secret, "task:"+s.taskID, s.runID, caps)
	if err != nil {
		_ = l.Close()
		_ = os.Remove(socketPath)
		return "", "", fmt.Errorf("ipc: issue token: %w", err)
	}

	go s.accept()
	return socketPath, token, nil
}

// Stop closes the listener and removes the socket file.
func (s *Server) Stop() {
	if s.listener != nil {
		_ = s.listener.Close()
	}
	if s.socketPath != "" {
		_ = os.Remove(s.socketPath)
	}
}

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

	// ── handshake ────────────────────────────────────────────────────────────
	var hs handshakeReq
	if err := readMsg(conn, &hs); err != nil {
		s.log.Warn("ipc: handshake read failed", zap.String("run", s.runID), zap.Error(err))
		return
	}
	claims, err := VerifyToken(s.secret, hs.Token)
	if err != nil {
		_ = writeMsg(conn, handshakeErr{Error: err.Error()})
		return
	}
	if claims.RunID != s.runID {
		_ = writeMsg(conn, handshakeErr{Error: "ipc: token run ID mismatch"})
		return
	}
	if err := writeMsg(conn, handshakeResp{Proto: 1, Caps: claims.Caps}); err != nil {
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
			_ = s.registry.AppendLog(context.Background(), s.runID, level, req.Message)

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

		case "dicode.get_config":
			if !hasCap(caps, CapConfigRead) {
				reply(req.ID, nil, "ipc: permission denied (config.read)")
				continue
			}
			if req.Section != "ai" {
				reply(req.ID, nil, fmt.Sprintf("ipc: unsupported config section %q", req.Section))
				continue
			}
			reply(req.ID, map[string]string{
				"baseURL": s.aiBaseURL,
				"model":   s.aiModel,
				"apiKey":  s.aiAPIKey,
			}, "")

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

		default:
			if req.ID != "" {
				reply(req.ID, nil, fmt.Sprintf("ipc: unknown method %q", req.Method))
			}
		}
	}
}

func (s *Server) taskAllowed(taskID string) bool {
	if s.spec == nil || s.spec.Security == nil {
		return false
	}
	for _, a := range s.spec.Security.AllowedTasks {
		if a == "*" || a == taskID {
			return true
		}
	}
	return false
}

func (s *Server) mcpAllowed(name string) bool {
	if s.spec == nil || s.spec.Security == nil {
		return false
	}
	for _, a := range s.spec.Security.AllowedMCP {
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
