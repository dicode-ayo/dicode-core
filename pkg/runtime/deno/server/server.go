// Package server implements the per-run Unix socket server that bridges
// the Deno subprocess and the Go host.
//
// Protocol: newline-delimited JSON over a single persistent connection.
//
// Task → Go (request):
//
//	{"id":"1","method":"kv.get","key":"x"}          — needs response
//	{"method":"log","level":"info","message":"hi"}   — fire-and-forget (no id)
//
// Go → Task (response, only when request had an id):
//
//	{"id":"1","result":{...}}
//	{"id":"1","error":"something went wrong"}
//
// Fire-and-forget methods (no id, no response): log, kv.set, kv.delete, output.
// Request/response methods (require id):        params, input, kv.get, kv.list, return.
package server

import (
	"bufio"
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

// OutputResult is a structured output from a task.
type OutputResult struct {
	ContentType string      `json:"contentType"`
	Content     string      `json:"content"`
	Data        interface{} `json:"data,omitempty"`
}

// IsSet reports whether output was set by the task.
func (o *OutputResult) IsSet() bool { return o.ContentType != "" }

// Server is a per-run Unix socket server.
type Server struct {
	runID    string
	taskID   string
	registry *registry.Registry
	db       db.DB
	params   map[string]string
	input    interface{}
	log      *zap.Logger

	socketPath   string
	listener     net.Listener
	returnCh     chan interface{}
	mu           sync.Mutex
	outputResult *OutputResult
	spec         *task.Spec

	engine    EngineRunner
	aiBaseURL string
	aiModel   string
	aiAPIKey  string
}

// request is an inbound message from the Deno subprocess.
type request struct {
	ID string `json:"id,omitempty"` // absent → fire-and-forget

	Method string `json:"method"`

	// log
	Level   string `json:"level,omitempty"`
	Message string `json:"message,omitempty"`

	// kv.*
	Key    string          `json:"key,omitempty"`
	Value  json.RawMessage `json:"value,omitempty"`
	Prefix string          `json:"prefix,omitempty"`

	// output
	ContentType string          `json:"contentType,omitempty"`
	Content     string          `json:"content,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`

	// dicode.*
	TaskID  string          `json:"taskID,omitempty"`
	Limit   int             `json:"limit,omitempty"`
	Section string          `json:"section,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`

	// mcp.*
	MCPName string          `json:"mcpName,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
}

// response is an outbound message to the Deno subprocess.
type response struct {
	ID     string      `json:"id"`
	Result interface{} `json:"result,omitempty"`
	Error  string      `json:"error,omitempty"`
}

// New creates a Server (not yet started).
func New(
	runID, taskID string,
	reg *registry.Registry,
	database db.DB,
	params map[string]string,
	input interface{},
	log *zap.Logger,
	spec *task.Spec,
	engine EngineRunner,
	aiBaseURL, aiModel, aiAPIKey string,
) *Server {
	return &Server{
		runID:     runID,
		taskID:    taskID,
		registry:  reg,
		db:        database,
		params:    params,
		input:     input,
		log:       log,
		spec:      spec,
		engine:    engine,
		aiBaseURL: aiBaseURL,
		aiModel:   aiModel,
		aiAPIKey:  aiAPIKey,
		returnCh:  make(chan interface{}, 1),
	}
}

// Start creates the Unix socket and begins accepting connections.
// Returns the socket path for the Deno subprocess.
func (s *Server) Start(_ context.Context) (string, error) {
	socketPath := fmt.Sprintf("/tmp/dicode-%s.sock", s.runID)
	_ = os.Remove(socketPath)

	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return "", fmt.Errorf("listen unix %s: %w", socketPath, err)
	}
	s.socketPath = socketPath
	s.listener = l

	go s.accept()
	return socketPath, nil
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

// ReturnCh receives the task return value once the subprocess posts "return".
func (s *Server) ReturnCh() <-chan interface{} { return s.returnCh }

// Output returns the captured output, or nil if none was set.
func (s *Server) Output() *OutputResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outputResult
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

// handleConn reads newline-delimited JSON requests and dispatches them
// sequentially. Responses (for request/response methods) are written back
// on the same connection.
func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	enc := json.NewEncoder(conn)

	// reply writes a response only when the request included an id.
	reply := func(id string, result interface{}, errMsg string) {
		if id == "" {
			return
		}
		r := response{ID: id, Result: result}
		if errMsg != "" {
			r.Error = errMsg
			r.Result = nil
		}
		_ = enc.Encode(r)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		var req request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}

		switch req.Method {

		// --- fire-and-forget ---

		case "log":
			level := req.Level
			if level == "" {
				level = "info"
			}
			_ = s.registry.AppendLog(context.Background(), s.runID, level, req.Message)

		case "kv.set":
			var val interface{}
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
				s.log.Error("kv.set", zap.String("key", req.Key), zap.Error(err))
			}

		case "kv.delete":
			ns := s.taskID + ":" + req.Key
			if err := s.db.Exec(context.Background(),
				`DELETE FROM kv WHERE key = ?`, ns,
			); err != nil {
				s.log.Error("kv.delete", zap.String("key", req.Key), zap.Error(err))
			}

		case "output":
			var data interface{}
			if len(req.Data) > 0 {
				_ = json.Unmarshal(req.Data, &data)
			}
			s.mu.Lock()
			s.outputResult = &OutputResult{
				ContentType: req.ContentType,
				Content:     req.Content,
				Data:        data,
			}
			s.mu.Unlock()

		// --- request / response ---

		case "params":
			reply(req.ID, s.params, "")

		case "input":
			reply(req.ID, s.input, "")

		case "kv.get":
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
			var out interface{}
			_ = json.Unmarshal([]byte(raw), &out)
			reply(req.ID, out, "")

		case "kv.list":
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
			var val interface{}
			if len(req.Value) > 0 {
				_ = json.Unmarshal(req.Value, &val)
			}
			// Signal returnCh BEFORE replying to Deno. This guarantees the
			// runtime's select sees returnCh before doneCh (which only becomes
			// ready after Deno receives the reply and exits).
			select {
			case s.returnCh <- val:
			default:
			}
			reply(req.ID, true, "")

		// --- dicode.* ---

		case "dicode.run_task":
			if s.engine == nil {
				reply(req.ID, nil, "dicode.run_task: engine not available")
				continue
			}
			if !s.taskAllowed(req.TaskID) {
				reply(req.ID, nil, fmt.Sprintf("dicode.run_task: task %q not in security.allowed_tasks", req.TaskID))
				continue
			}
			var callParams map[string]string
			if len(req.Params) > 0 {
				_ = json.Unmarshal(req.Params, &callParams)
			}
			runID, err := s.engine.FireManual(context.Background(), req.TaskID, callParams)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			result, err := s.engine.WaitRun(context.Background(), runID)
			if err != nil {
				reply(req.ID, nil, err.Error())
				continue
			}
			reply(req.ID, result, "")

		case "dicode.list_tasks":
			type taskSummary struct {
				ID          string      `json:"id"`
				Name        string      `json:"name"`
				Description string      `json:"description"`
				Params      interface{} `json:"params,omitempty"`
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
			if req.Section != "ai" {
				reply(req.ID, nil, fmt.Sprintf("dicode.get_config: unsupported section %q (only \"ai\" is supported)", req.Section))
				continue
			}
			reply(req.ID, map[string]string{
				"baseURL": s.aiBaseURL,
				"model":   s.aiModel,
				"apiKey":  s.aiAPIKey,
			}, "")

		// --- mcp.* ---

		case "mcp.list_tools":
			if !s.mcpAllowed(req.MCPName) {
				reply(req.ID, nil, fmt.Sprintf("mcp.list_tools: %q not in security.allowed_mcp", req.MCPName))
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
			if !s.mcpAllowed(req.MCPName) {
				reply(req.ID, nil, fmt.Sprintf("mcp.call: %q not in security.allowed_mcp", req.MCPName))
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
		}
	}
}

// taskAllowed reports whether the running task's security config permits calling taskID.
func (s *Server) taskAllowed(taskID string) bool {
	if s.spec == nil || s.spec.Security == nil {
		return false
	}
	for _, allowed := range s.spec.Security.AllowedTasks {
		if allowed == "*" || allowed == taskID {
			return true
		}
	}
	return false
}

// mcpAllowed reports whether the running task's security config permits accessing the named MCP server.
func (s *Server) mcpAllowed(name string) bool {
	if s.spec == nil || s.spec.Security == nil {
		return false
	}
	for _, allowed := range s.spec.Security.AllowedMCP {
		if allowed == "*" || allowed == name {
			return true
		}
	}
	return false
}

// getMCPPort looks up the mcp_port of a daemon task by its task ID.
func (s *Server) getMCPPort(taskID string) (int, error) {
	spec, ok := s.registry.Get(taskID)
	if !ok {
		return 0, fmt.Errorf("mcp: task %q not found", taskID)
	}
	if spec.MCPPort == 0 {
		return 0, fmt.Errorf("mcp: task %q does not declare mcp_port", taskID)
	}
	return spec.MCPPort, nil
}
