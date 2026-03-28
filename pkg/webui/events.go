package webui

import (
	"encoding/json"
	"sync"
)

// RunEvent is broadcast to SSE clients when a run finishes.
type RunEvent struct {
	RunID         string `json:"runID"`
	TaskID        string `json:"taskID"`
	TaskName      string `json:"taskName"`
	Status        string `json:"status"`
	DurationMs    int64  `json:"durationMs"`
	TriggerSource string `json:"triggerSource"`
}

// JSON returns the event serialised as JSON (used in SSE data field).
func (e RunEvent) JSON() string {
	b, _ := json.Marshal(e)
	return string(b)
}

// RunEventBroadcaster fans out RunEvents to all subscribed SSE clients.
type RunEventBroadcaster struct {
	mu      sync.Mutex
	clients map[chan RunEvent]struct{}
}

func NewRunEventBroadcaster() *RunEventBroadcaster {
	return &RunEventBroadcaster{clients: make(map[chan RunEvent]struct{})}
}

func (b *RunEventBroadcaster) Publish(evt RunEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- evt:
		default: // drop if subscriber is slow
		}
	}
}

func (b *RunEventBroadcaster) Subscribe() chan RunEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan RunEvent, 16)
	b.clients[ch] = struct{}{}
	return ch
}

func (b *RunEventBroadcaster) Unsubscribe(ch chan RunEvent) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
	close(ch)
}
