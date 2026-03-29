package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/coder/websocket"
	"go.uber.org/zap"
)

// WSMsg is a message sent over the WebSocket connection.
type WSMsg struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
}

// RunStartedData is the payload for "run:started".
type RunStartedData struct {
	RunID         string `json:"runID"`
	TaskID        string `json:"taskID"`
	TaskName      string `json:"taskName"`
	TriggerSource string `json:"triggerSource"`
}

// RunLogData is the payload for "run:log".
type RunLogData struct {
	RunID   string `json:"runID"`
	Level   string `json:"level"`
	Message string `json:"message"`
	Ts      int64  `json:"ts"`
}

// RunFinishedData is the payload for "run:finished".
type RunFinishedData struct {
	RunID             string `json:"runID"`
	TaskID            string `json:"taskID"`
	TaskName          string `json:"taskName"`
	Status            string `json:"status"`
	DurationMs        int64  `json:"durationMs"`
	TriggerSource     string `json:"triggerSource"`
	OutputContentType string `json:"outputContentType,omitempty"`
	ReturnValue       string `json:"returnValue,omitempty"`
	NotifyOnSuccess   bool   `json:"notifyOnSuccess"`
	NotifyOnFailure   bool   `json:"notifyOnFailure"`
}

// WSHub manages all WebSocket clients.
type WSHub struct {
	mu         sync.Mutex
	clients    map[*wsClient]struct{}
	log        *zap.Logger
	recentLogs func() []string // returns buffered log lines for replay on subscribe
}

type wsClient struct {
	hub    *WSHub
	conn   *websocket.Conn
	send   chan []byte
	logSub bool
	mu     sync.Mutex
}

func NewWSHub(log *zap.Logger) *WSHub {
	return &WSHub{clients: make(map[*wsClient]struct{}), log: log}
}

func (h *WSHub) add(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *WSHub) remove(c *wsClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// Broadcast sends a message to all connected clients.
func (h *WSHub) Broadcast(msg WSMsg) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.send <- b:
		default: // drop if slow
		}
	}
}

// BroadcastLog sends a server log line only to clients that have subscribed.
func (h *WSHub) BroadcastLog(line string) {
	b, _ := json.Marshal(WSMsg{Type: "log:line", Data: map[string]string{"line": line}})
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		c.mu.Lock()
		sub := c.logSub
		c.mu.Unlock()
		if sub {
			select {
			case c.send <- b:
			default:
			}
		}
	}
}

// ServeHTTP upgrades the connection to WebSocket and manages the client.
func (h *WSHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // local dev — allow any origin
	})
	if err != nil {
		h.log.Debug("ws accept failed", zap.Error(err))
		return
	}

	c := &wsClient{
		hub:  h,
		conn: conn,
		send: make(chan []byte, 128),
	}
	h.add(c)
	defer h.remove(c)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Writer goroutine
	go func() {
		defer cancel()
		for {
			select {
			case b, ok := <-c.send:
				if !ok {
					return
				}
				if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Reader loop — handles subscription control messages from the client
	for {
		_, b, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg WSMsg
		if err := json.Unmarshal(b, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "sub:logs":
			c.mu.Lock()
			c.logSub = true
			c.mu.Unlock()
			// Replay buffered history so the log bar isn't empty on first open.
			if h.recentLogs != nil {
				for _, line := range h.recentLogs() {
					b, _ := json.Marshal(WSMsg{Type: "log:line", Data: map[string]string{"line": line}})
					select {
					case c.send <- b:
					default:
					}
				}
			}
		case "unsub:logs":
			c.mu.Lock()
			c.logSub = false
			c.mu.Unlock()
		}
	}
}
