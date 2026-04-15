package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/dicode/dicode/pkg/db"
	mcpclient "github.com/dicode/dicode/pkg/mcp/client"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/relay"
	"github.com/dicode/dicode/pkg/secrets"
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
	secrets      secrets.Manager         // optional; enables dicode.secrets_set / dicode.secrets_delete
	oauthID      *relay.Identity         // optional; enables dicode.oauth.* for the auth built-ins
	oauthURL     string                  // broker base URL, e.g. "https://relay.dicode.app"
	oauthPending *relay.PendingSessions  // tracks outstanding /auth/:provider flows by session id
	log          *zap.Logger

	aiBaseURL string
	aiModel   string
	aiAPIKey  string

	gateway *Gateway // optional; enables http.register for daemon tasks

	ctx        context.Context
	socketPath string
	listener   net.Listener

	mu     sync.Mutex
	output *OutputResult
	retCh  chan any
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
	aiBaseURL, aiModel, aiAPIKey string,
) *Server {
	if runID == "" {
		panic("ipc.New: runID must not be empty")
	}
	if taskID == "" {
		panic("ipc.New: taskID must not be empty")
	}
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

// SetSecrets attaches the secrets manager so tasks with permissions.dicode.secrets_write
// can call dicode.secrets_set() and dicode.secrets_delete().
func (s *Server) SetSecrets(m secrets.Manager) { s.secrets = m }

// SetOAuthBroker wires the daemon's relay identity (used to sign /auth URLs
// and decrypt token deliveries) plus the broker base URL and the
// daemon-wide PendingSessions store. The task-side dicode.oauth.* API is
// inert until this is set, so the auth-relay built-in task must gracefully
// degrade when the relay client is not enabled in dicode.yaml.
//
// pending is shared across all per-run ipc.Server instances so that an
// auth-start run and the subsequent auth-complete webhook run can correlate
// on session id.
func (s *Server) SetOAuthBroker(id *relay.Identity, baseURL string, pending *relay.PendingSessions) {
	s.oauthID = id
	s.oauthURL = baseURL
	s.oauthPending = pending
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
		if dp.GetConfig {
			caps = append(caps, CapConfigRead)
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

// SetGateway attaches the HTTP gateway so daemon tasks can call http.register.
// Must be called before Start.
func (s *Server) SetGateway(g *Gateway) { s.gateway = g }

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
			// apiKey is never sent to task scripts — it must be fetched
			// from the environment or secrets store by the task itself.
			// Returning it here would expose the credential to any task
			// regardless of whether it needs AI access.
			reply(req.ID, map[string]string{
				"baseURL": s.aiBaseURL,
				"model":   s.aiModel,
			}, "")

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
				s.log.Info("oauth token delivered",
					zap.String("task", s.taskID),
					zap.String("run", s.runID),
					zap.String("provider", authReq.Provider),
					zap.String("session", authReq.SessionID),
					zap.Strings("secrets", written),
				)
			}
			reply(req.ID, map[string]any{
				"provider": authReq.Provider,
				"secrets":  written,
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
