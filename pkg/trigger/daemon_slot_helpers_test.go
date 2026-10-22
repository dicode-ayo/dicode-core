package trigger

// seedSlot rebinds taskID's slot to runID, replacing any existing claim.
func seedSlot(e *Engine, taskID, runID string) {
	e.slots.evict(taskID)
	slot, _ := e.slots.claim(taskID)
	slot.attach(runID)
}
