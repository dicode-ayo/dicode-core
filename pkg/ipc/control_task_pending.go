package ipc

// PendingTask is one row in the cli.task.pending response: a task the approval
// gate is holding, plus the short content hash observed when it was held — enough
// to eyeball what changed without leaking the full digest.
type PendingTask struct {
	TaskID string `json:"taskID"`
	Hash   string `json:"hash"`
	// Enabled is the resolved enabled flag observed when the task was held
	// pending. False means the task is disabled (task.yaml or a taskset
	// override) — the trigger engine registers it with zero triggers
	// regardless of approval state (#822), so an operator reading this
	// listing can tell "held, and nothing would arm anyway" apart from a
	// real hold blocking something that would otherwise run.
	Enabled bool `json:"enabled"`
}

// SetPendingApprovals wires the approval gate's pending set for cli.task.pending
// and cli.list annotation. The callback returns each held task's id and full
// content hash; the handler shortens the hash for display. Nil leaves
// cli.task.pending returning an empty list and cli.list unannotated (tests /
// gate disabled).
func (cs *ControlServer) SetPendingApprovals(f func() []PendingTask) { cs.pendingApprovals = f }

// handleTaskPending lists the tasks the approval gate is holding, each with its
// short content hash, so a headless operator can discover an id to feed
// `dicode task approve`. A nil gate (disabled / not wired) yields an empty list,
// not an error — nothing pending and no gate are the same clear empty state.
func (cs *ControlServer) handleTaskPending() ([]PendingTask, error) {
	if cs.pendingApprovals == nil {
		return []PendingTask{}, nil
	}
	pending := cs.pendingApprovals()
	out := make([]PendingTask, 0, len(pending))
	for _, p := range pending {
		out = append(out, PendingTask{TaskID: p.TaskID, Hash: shortContentHash(p.Hash), Enabled: p.Enabled})
	}
	return out, nil
}

// pendingTaskIDs returns the set of task ids the approval gate is holding, for
// annotating cli.list. Empty when the gate is disabled or nothing is pending.
func (cs *ControlServer) pendingTaskIDs() map[string]struct{} {
	if cs.pendingApprovals == nil {
		return nil
	}
	pending := cs.pendingApprovals()
	if len(pending) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(pending))
	for _, p := range pending {
		set[p.TaskID] = struct{}{}
	}
	return set
}

// shortContentHash trims a content hash for display, matching the Web UI's
// convention so the full digest never crosses the wire for a discovery listing.
func shortContentHash(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}
