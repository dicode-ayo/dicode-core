package webui

import (
	"strings"
	"sync"
)

// LogBroadcaster is an io.Writer (used by zap) that fans out log lines
// to a registered hook (e.g. the WSHub). It also keeps a ring buffer
// so the log bar can show recent history when first opened.
type LogBroadcaster struct {
	mu     sync.Mutex
	hook   func(line string)
	recent []string
}

func NewLogBroadcaster() *LogBroadcaster {
	return &LogBroadcaster{}
}

// SetHook registers a function called for each new log line.
func (b *LogBroadcaster) SetHook(fn func(line string)) {
	b.mu.Lock()
	b.hook = fn
	b.mu.Unlock()
}

// Recent returns the last N buffered log lines.
func (b *LogBroadcaster) Recent() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.recent))
	copy(out, b.recent)
	return out
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
	hook := b.hook
	b.mu.Unlock()
	if hook != nil {
		hook(line)
	}
	return len(p), nil
}
