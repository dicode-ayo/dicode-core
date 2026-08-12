package task

import "time"

// LoadFailure describes a task/entry that failed to parse, load, or validate
// instead of being silently dropped. Historically, a source event whose
// task.yaml failed to load (or a taskset entry that failed to resolve) was
// just logged and discarded — which made the task vanish from the registry
// and the webui with no trace beyond daemon.log (#649). Both
// pkg/registry.Registry (for plain git/local sources, which load task.yaml
// via task.LoadKindedDir in the reconciler) and pkg/taskset.Source (for
// taskset sources, which resolve entries themselves) record failures of this
// shape into their own side-channel so the webui can merge them into
// GET /api/tasks and GET /api/sources instead of the entry disappearing.
type LoadFailure struct {
	// ID is the task/entry ID that failed to load (namespaced where
	// applicable, matching the ID it would have registered under).
	ID string `json:"id"`
	// Source is the identifier of the source the entry came from (e.g. the
	// taskset source's name/ID, or the reconciler event's source string).
	Source string `json:"source,omitempty"`
	// Error is the load/parse/validate failure message.
	Error string `json:"error"`
	// At is when this failure was last (re)recorded.
	At time.Time `json:"at"`
}
