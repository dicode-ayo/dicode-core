package taskset

import (
	"sync"
	"time"
)

// PullStatus is the UI-facing snapshot of a Source's most recent
// git-pull attempt. Used to render a health indicator in the webui.
type PullStatus struct {
	LastPullAt time.Time `json:"last_pull_at,omitempty"`
	OK         bool      `json:"ok"`
	Error      string    `json:"error,omitempty"`
}

// pullStatusState guards the mutable health fields under its own mutex
// so PullStatus() never blocks on the Source's main mutex.
type pullStatusState struct {
	mu sync.RWMutex
	ps PullStatus
}

// PullStatus returns a copy of the Source's most recent pull outcome.
// A zero-value result (empty LastPullAt) means no pull has been
// attempted yet, which the frontend treats as "pending / N/A".
func (s *Source) PullStatus() PullStatus {
	s.pullStatus.mu.RLock()
	defer s.pullStatus.mu.RUnlock()
	return s.pullStatus.ps
}

// recordPull writes the result of a Pull attempt. err nil marks
// success; a non-nil err captures the error message and flips OK off.
// On success the previous error is cleared.
func (s *Source) recordPull(err error) {
	s.pullStatus.mu.Lock()
	defer s.pullStatus.mu.Unlock()
	s.pullStatus.ps.LastPullAt = time.Now()
	if err != nil {
		s.pullStatus.ps.OK = false
		s.pullStatus.ps.Error = err.Error()
		return
	}
	s.pullStatus.ps.OK = true
	s.pullStatus.ps.Error = ""
}
