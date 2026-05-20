package task

// Kind discriminator values for the top-level `kind:` field in a task's
// task.yaml. pkg/taskset carries a parallel typed Kind constant with the
// same string value ("Task"); the two are intentionally independent to
// avoid a circular import (pkg/taskset imports pkg/task, not vice versa).
const (
	KindTask         = "Task"
	KindPipelineTask = "PipelineTask"
)

// Kinded is the common surface the transport layer (source events, registry,
// reconciler, engine registration) needs to handle any task kind uniformly.
// Both *Spec (kind: Task) and *PipelineTask (kind: PipelineTask) implement it.
//
// Method names deliberately avoid clashing with the underlying struct fields
// (ID, Enabled, Warnings) — Go forbids a method and field of the same name.
type Kinded interface {
	KindOf() string
	TaskID() string
	SetTaskID(id string)
	IsEnabled() bool
	SetEnabled(b bool)
	LoadWarnings() []string
	Validate() error
}

// --- *Spec implements Kinded ---

func (s *Spec) KindOf() string         { return KindTask }
func (s *Spec) TaskID() string         { return s.ID }
func (s *Spec) SetTaskID(id string)    { s.ID = id }
func (s *Spec) IsEnabled() bool        { return s.Enabled }
func (s *Spec) SetEnabled(b bool)      { s.Enabled = b }
func (s *Spec) LoadWarnings() []string { return s.Warnings }
