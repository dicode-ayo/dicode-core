package relay

import (
	"sync"
	"time"
)

// Status is the JSON-serializable snapshot of the relay client's current
// state. Consumed by the webui (/api/relay/status) and MCP, so field
// names are stable and additive.
type Status struct {
	Enabled           bool      `json:"enabled"`                 // true for any constructed Client; false when no relay is configured
	Connected         bool      `json:"connected"`               // WebSocket is currently up
	RemoteURL         string    `json:"remote_url,omitempty"`    // configured relay endpoint
	HookBaseURL       string    `json:"hook_base_url,omitempty"` // populated after handshake
	Since             time.Time `json:"since"`                   // when the current (dis)connected state began
	LastError         string    `json:"last_error,omitempty"`    // last transport error, cleared on connect
	ReconnectAttempts int       `json:"reconnect_attempts"`      // consecutive failures; zeroed on connect
}

// statusState isolates the mutable connection health under a single
// mutex so Status() is a consistent snapshot.
type statusState struct {
	mu                sync.RWMutex
	connected         bool
	since             time.Time
	lastError         string
	reconnectAttempts int
}

// Status returns a copy of the client's current state. Safe for
// concurrent use while the client's Run loop is active.
func (c *Client) Status() Status {
	c.status.mu.RLock()
	defer c.status.mu.RUnlock()
	return Status{
		Enabled:           true,
		Connected:         c.status.connected,
		RemoteURL:         c.serverURL,
		HookBaseURL:       c.HookBaseURL(),
		Since:             c.status.since,
		LastError:         c.status.lastError,
		ReconnectAttempts: c.status.reconnectAttempts,
	}
}

// markConnected records a successful handshake. Clears the last error
// and resets the reconnect counter (exponential backoff's own state is
// owned by Run).
func (c *Client) markConnected() {
	c.status.mu.Lock()
	defer c.status.mu.Unlock()
	c.status.connected = true
	c.status.since = time.Now()
	c.status.lastError = ""
	c.status.reconnectAttempts = 0
}

// markDisconnected records a connection-loop transition. When err is
// non-nil, the error is stored and the reconnect counter bumps (this
// is the normal "runOnce returned an error" case). When err is nil,
// the transition reflects a clean shutdown — the Connected flag flips
// but the previous error and retry counter are preserved so the UI
// doesn't lie about the final state.
func (c *Client) markDisconnected(err error) {
	c.status.mu.Lock()
	defer c.status.mu.Unlock()
	c.status.connected = false
	c.status.since = time.Now()
	if err == nil {
		return
	}
	c.status.lastError = sanitizeErrorString(err.Error())
	c.status.reconnectAttempts++
}
