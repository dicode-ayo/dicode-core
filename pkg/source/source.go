// Package source defines the Source interface and event types consumed by the
// reconciler. Concrete implementations (git, local) live in sub-packages.
package source

import "context"

// EventKind classifies a task change detected by a source.
type EventKind string

const (
	EventAdded   EventKind = "added"
	EventRemoved EventKind = "removed"
	EventUpdated EventKind = "updated"
)

// Event is emitted by a Source when a task directory changes.
type Event struct {
	Kind    EventKind
	TaskID  string // directory name, e.g. "morning-email-check"
	TaskDir string // absolute path to the task directory
	Source  string // source identifier (URL or path) for logging
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
