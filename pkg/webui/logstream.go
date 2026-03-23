package webui

import (
	"strings"
	"sync"
)

// LogBroadcaster is an io.Writer that fans out written log lines to SSE subscribers.
// It also keeps a short ring buffer so new subscribers receive recent history.
type LogBroadcaster struct {
	mu      sync.Mutex
	clients map[chan string]struct{}
	recent  []string
}

func NewLogBroadcaster() *LogBroadcaster {
	return &LogBroadcaster{clients: make(map[chan string]struct{})}
}

func (b *LogBroadcaster) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	if line == "" {
		return len(p), nil
	}
	b.mu.Lock()
	b.recent = append(b.recent, line)
	if len(b.recent) > 300 {
		b.recent = b.recent[1:]
	}
	for ch := range b.clients {
		select {
		case ch <- line:
		default: // drop if client is slow
		}
	}
	b.mu.Unlock()
	return len(p), nil
}

func (b *LogBroadcaster) subscribe() (ch chan string, snapshot []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch = make(chan string, 64)
	b.clients[ch] = struct{}{}
	snapshot = make([]string, len(b.recent))
	copy(snapshot, b.recent)
	return ch, snapshot
}

func (b *LogBroadcaster) unsubscribe(ch chan string) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
	close(ch)
}
