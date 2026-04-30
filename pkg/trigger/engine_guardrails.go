// pkg/trigger/engine_guardrails.go
package trigger

import (
	"sync"
	"time"
)

// chainGuards holds the in-memory state for engine-level on_failure_chain
// guardrails: cooldown timestamps, per-task and global concurrency counters,
// and per-source storm counters. Survives daemon process lifetime only —
// resets on restart (documented trade-off for v1).
type chainGuards struct {
	mu sync.Mutex

	// lastFire[taskID] = time.Time of the most recent on_failure_chain fire
	// originating from a failure of taskID.
	lastFire map[string]time.Time

	// inFlightPerTask[taskID] = count of currently-running on_failure_chain
	// runs whose parent failed-task is taskID.
	inFlightPerTask map[string]int

	// inFlightGlobal = total currently-running on_failure_chain runs.
	inFlightGlobal int

	// stormFires[sourceNs] = sliding window of fire timestamps. Trimmed on
	// each query to drop entries older than the configured window.
	stormFires map[string][]time.Time

	// stormSuppressedUntil[sourceNs] = time after which fires from this source
	// resume.
	stormSuppressedUntil map[string]time.Time
}

func newChainGuards() *chainGuards {
	return &chainGuards{
		lastFire:             make(map[string]time.Time),
		inFlightPerTask:      make(map[string]int),
		stormFires:           make(map[string][]time.Time),
		stormSuppressedUntil: make(map[string]time.Time),
	}
}

func (g *chainGuards) recordChainFire(taskID string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastFire[taskID] = now
}

func (g *chainGuards) cooldownActive(taskID string, cooldown time.Duration, now time.Time) bool {
	if cooldown <= 0 {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	last, ok := g.lastFire[taskID]
	if !ok {
		return false
	}
	return now.Sub(last) < cooldown
}

// acquireSlot atomically reserves a chain-fire slot for taskID if both the
// per-task and global caps have headroom. Returns false (and acquires
// nothing) on contention.
func (g *chainGuards) acquireSlot(taskID string, perTaskCap, globalCap int) bool {
	if perTaskCap <= 0 {
		perTaskCap = 1
	}
	if globalCap <= 0 {
		globalCap = 3
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inFlightPerTask[taskID] >= perTaskCap {
		return false
	}
	if g.inFlightGlobal >= globalCap {
		return false
	}
	g.inFlightPerTask[taskID]++
	g.inFlightGlobal++
	return true
}

// releaseSlot decrements counters for a chain-fire slot. Idempotent at zero
// (clamped) so a double-release does not underflow.
func (g *chainGuards) releaseSlot(taskID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inFlightPerTask[taskID] > 0 {
		g.inFlightPerTask[taskID]--
	}
	if g.inFlightGlobal > 0 {
		g.inFlightGlobal--
	}
}

// observeChainFire records a fire for the given scope and trips the breaker
// when the count within the window exceeds rate.
func (g *chainGuards) observeChainFire(scope string, rate int, window, suppress time.Duration, now time.Time) {
	if rate <= 0 || window <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	cutoff := now.Add(-window)
	fires := g.stormFires[scope]
	pruned := fires[:0]
	for _, t := range fires {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	pruned = append(pruned, now)
	g.stormFires[scope] = pruned
	if len(pruned) > rate {
		g.stormSuppressedUntil[scope] = now.Add(suppress)
	}
}

// stormSuppressed reports whether scope is in a tripped-breaker suppression
// window. The suppress duration was applied when the breaker tripped (in
// observeChainFire); this query just reads the cached deadline.
func (g *chainGuards) stormSuppressed(scope string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	until, ok := g.stormSuppressedUntil[scope]
	if !ok {
		return false
	}
	if now.After(until) {
		delete(g.stormSuppressedUntil, scope)
		return false
	}
	return true
}
