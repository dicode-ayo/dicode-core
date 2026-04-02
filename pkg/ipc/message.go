package ipc

import (
	"context"
	"encoding/json"
)

// handshakeReq is the first message sent by the client after connecting.
type handshakeReq struct {
	Token string `json:"token"`
}

// handshakeResp is the server's reply to a successful handshake.
type handshakeResp struct {
	Proto int      `json:"proto"`
	Caps  []string `json:"caps"`
}

// handshakeErr is the server's reply when the handshake fails. The server
// closes the connection immediately after sending this.
type handshakeErr struct {
	Error string `json:"error"`
}

// Request is an inbound message from a connected client.
type Request struct {
	ID string `json:"id,omitempty"` // absent → fire-and-forget

	Method string `json:"method"`

	// log
	Level   string `json:"level,omitempty"`
	Message string `json:"message,omitempty"`

	// kv.* and return — both use "value" in the JSON payload
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

// Response is an outbound message to a connected client.
type Response struct {
	ID     string `json:"id"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// OutputResult is a structured output produced by a task via the output.* API.
type OutputResult struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
	Data        any    `json:"data,omitempty"`
}

// IsSet reports whether any output was recorded.
func (o *OutputResult) IsSet() bool { return o != nil && o.ContentType != "" }

// EngineRunner allows the IPC server to fire and await task runs.
// Implemented by the trigger engine; injected to avoid import cycles.
type EngineRunner interface {
	FireManual(ctx context.Context, taskID string, params map[string]string) (string, error)
	WaitRun(ctx context.Context, runID string) (RunResult, error)
}

// RunResult is returned by EngineRunner.WaitRun.
type RunResult struct {
	RunID       string `json:"runID"`
	Status      string `json:"status"`
	ReturnValue any    `json:"returnValue"`
}
