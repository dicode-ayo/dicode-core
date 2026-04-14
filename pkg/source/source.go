// Package source defines the Source interface and event types consumed by the
// reconciler. Concrete implementations (git, local) live in sub-packages.
package source

import (
	"context"

	"github.com/dicode/dicode/pkg/task"
)

// EventKind classifies a task change detected by a source.
type EventKind string

const (
	EventAdded   EventKind = "added"
	EventRemoved EventKind = "removed"
	EventUpdated EventKind = "updated"
)

// Event is emitted by a Source when a task changes.
type Event struct {
	Kind    EventKind
	TaskID  string // namespaced task ID, e.g. "infra/backend/deploy"
	TaskDir string // absolute path to the task directory (used by reconciler for LoadDir)
	Source  string // source identifier (URL or path) for logging
	// Spec, when non-nil, is a fully resolved task spec (overrides already applied).
	// The reconciler uses it directly instead of calling task.LoadDir(TaskDir).
	// Set by taskset sources; nil for plain git/local sources.
	Spec *task.Spec
	// ExtraVars carries per-source template variables injected into
	// task.yaml's ${VAR} expansion at load time. Sources populate this with
	// e.g. SOURCE_ROOT (the source's root path) so tasks can reference
	// shared directories without hardcoding a path. nil = no extras.
	ExtraVars map[string]string
}

// Source is anything that can produce task change events.
// The reconciler subscribes to one or more sources and merges their streams.
type Source interface {
	// ID returns a human-readable identifier for this source (git URL or local path).
	ID() string

	// Start begins watching for changes and sends events on the returned channel.
	// The channel is closed when ctx is cancelled.
	// Start must be called only once per Source.
	Start(ctx context.Context) (<-chan Event, error)

	// Sync triggers an immediate reconciliation (pull for git, rescan for local).
	// Safe to call concurrently.
	Sync(ctx context.Context) error
}
