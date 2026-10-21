package trigger

import (
	"errors"
	"sync"
)

// daemonSlots owns the "at most one body in flight" invariant for standalone
// daemons: a key is present exactly while a start is in flight or a body is
// running or parked. Daemon task IDs and pipeline IDs live in separate
// keyspaces so equal IDs never coalesce with each other.
type daemonSlots struct {
	mu sync.Mutex
	m  map[slotKey]*slotEntry
}

type slotKey struct {
	pipeline bool
	id       string
}

type slotEntry struct {
	runID  string
	parked bool
}

// daemonSlot is a handle on one claimed entry. Every operation re-checks under
// the lock that the entry is still the one in the map, so a handle outlived by
// an evict or a newer claim is inert.
type daemonSlot struct {
	s   *daemonSlots
	k   slotKey
	e   *slotEntry
	run string // run the entry was bound to when the handle was obtained
}

func newDaemonSlots() *daemonSlots {
	return &daemonSlots{m: make(map[slotKey]*slotEntry)}
}

func (s *daemonSlots) claimKey(k slotKey) (daemonSlot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.m[k]; busy {
		return daemonSlot{}, false
	}
	e := &slotEntry{}
	s.m[k] = e
	return daemonSlot{s: s, k: k, e: e}, true
}

// claim takes the daemon slot for taskID; false means a body is already in
// flight (or being started).
func (s *daemonSlots) claim(taskID string) (daemonSlot, bool) {
	return s.claimKey(slotKey{id: taskID})
}

// claimPipeline is claim in the pipeline keyspace.
func (s *daemonSlots) claimPipeline(pipelineID string) (daemonSlot, bool) {
	return s.claimKey(slotKey{pipeline: true, id: pipelineID})
}

// holding returns a handle on taskID's slot if it is currently bound to runID.
// An empty runID never matches: a fresh claim has no run yet.
func (s *daemonSlots) holding(taskID, runID string) (daemonSlot, bool) {
	if runID == "" {
		return daemonSlot{}, false
	}
	k := slotKey{id: taskID}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[k]
	if !ok || e.runID != runID {
		return daemonSlot{}, false
	}
	return daemonSlot{s: s, k: k, e: e, run: runID}, true
}

// releaseRun frees taskID's slot if it is bound to runID; an empty runID never
// matches.
func (s *daemonSlots) releaseRun(taskID, runID string) bool {
	h, ok := s.holding(taskID, runID)
	return ok && h.release(runID)
}

// evict unconditionally frees taskID's slot and returns the run it was bound
// to ("" if none). Handles on the evicted entry become inert.
func (s *daemonSlots) evict(taskID string) string {
	k := slotKey{id: taskID}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[k]
	if !ok {
		return ""
	}
	delete(s.m, k)
	return e.runID
}

// runID returns the run taskID's slot is bound to, or "".
func (s *daemonSlots) runID(taskID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[slotKey{id: taskID}]; ok {
		return e.runID
	}
	return ""
}

// runs snapshots the daemon task ID → bound run ID of every bound slot.
func (s *daemonSlots) runs() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.m))
	for k, e := range s.m {
		if !k.pipeline && e.runID != "" {
			out[k.id] = e.runID
		}
	}
	return out
}

// owned reports whether the handle's entry is still the one in the map. The
// caller holds h.s.mu.
func (h daemonSlot) owned() bool {
	return h.s.m[h.k] == h.e
}

// bound reports whether the slot is still owned by the handle and bound to
// runID.
func (h daemonSlot) bound(runID string) bool {
	if h.s == nil {
		return false
	}
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	return h.owned() && h.e.runID == runID
}

// attach binds the claimed slot to runID. False if the slot was evicted.
func (h daemonSlot) attach(runID string) bool {
	if h.s == nil {
		return false
	}
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if !h.owned() {
		return false
	}
	h.e.runID = runID
	h.e.parked = false
	return true
}

// park marks the slot as held by a suspended body awaiting resume. False if
// the slot is no longer bound to the run the handle was obtained for.
func (h daemonSlot) park() bool {
	if h.s == nil {
		return false
	}
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if !h.owned() || h.e.runID != h.run {
		return false
	}
	h.e.parked = true
	return true
}

var errSlotSuperseded = errors.New("slot no longer bound to the expected run")

// adopt rebinds the slot from the run the handle was obtained for to
// newRunID, failing if the slot was evicted or taken over meanwhile, or if
// either run ID is empty.
func (h daemonSlot) adopt(newRunID string) error {
	if h.s == nil || h.run == "" || newRunID == "" {
		return errSlotSuperseded
	}
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if !h.owned() || h.e.runID != h.run {
		return errSlotSuperseded
	}
	h.e.runID = newRunID
	h.e.parked = false
	return nil
}

// release frees the slot if it is still bound to runID; a no-op (false) when
// it was evicted or rebound.
func (h daemonSlot) release(runID string) bool {
	if h.s == nil {
		return false
	}
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if !h.owned() || h.e.runID != runID {
		return false
	}
	delete(h.s.m, h.k)
	return true
}
