// Package notify defines the Notifier interface and provider implementations
// for push notifications (ntfy, gotify, pushover, telegram).
//
// The Notifier is used in two ways:
//  1. System-level: dicode calls it automatically on task failure/success
//  2. Task-level: injected as the `notify` JS global so tasks can send
//     notifications from script code
package notify

import "context"

// Priority maps to notification urgency levels.
type Priority string

const (
	PriorityMin     Priority = "min"
	PriorityLow     Priority = "low"
	PriorityDefault Priority = "default"
	PriorityHigh    Priority = "high"
	PriorityUrgent  Priority = "urgent"
)

// Message is a notification to be delivered.
type Message struct {
	Title    string
	Body     string
	Priority Priority
	Tags     []string // emoji shortcodes, e.g. ["warning", "skull"]
	Actions  []Action // optional action buttons (approval gates)
}

// Action is a button the user can tap in the notification.
type Action struct {
	Label string
	ID    string // returned via callback when tapped
}

// Notifier delivers notifications to the user.
type Notifier interface {
	// Name returns the provider name (ntfy, gotify, etc.)
	Name() string

	// Send delivers a notification. Non-blocking — returns after delivery attempt.
	Send(ctx context.Context, msg Message) error
}

// NoopNotifier silently drops all notifications. Used when no provider is configured.
type NoopNotifier struct{}

func (n *NoopNotifier) Name() string                            { return "noop" }
func (n *NoopNotifier) Send(_ context.Context, _ Message) error { return nil }
