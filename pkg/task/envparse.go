// Package task — env-from prefix grammar.
//
// EnvEntry.From historically held a bare host-env-var name. Issue #119
// introduces an optional prefix:
//
//	env:NAME       — host OS environment variable
//	task:PROVIDER  — resolve via provider task PROVIDER
//	NAME           — bare; treated as env:NAME for backwards compatibility
package task

import "strings"

// FromKind is the discriminator for an EnvEntry.From value.
type FromKind int

const (
	// FromKindEnv resolves via os.Getenv. Default for bare values.
	FromKindEnv FromKind = iota
	// FromKindTask resolves by spawning a provider task whose ID is the
	// returned target.
	FromKindTask
)

// parseFrom splits an EnvEntry.From string into (kind, target). Whitespace
// is trimmed. An empty string yields (FromKindEnv, "") so callers can
// detect the no-from case.
//
// Grammar:
//
//	"env:NAME"        → (FromKindEnv,  "NAME")
//	"task:PROVIDER"   → (FromKindTask, "PROVIDER")
//	"NAME"            → (FromKindEnv,  "NAME")  // bare = env (legacy)
//	""                → (FromKindEnv,  "")
//
// Unknown prefixes (e.g. "foo:bar") are treated as bare names so existing
// task.yaml files containing colons in env var names continue to load.
// The reconciler is responsible for catching truly malformed names.
func parseFrom(s string) (FromKind, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return FromKindEnv, ""
	}
	if rest, ok := strings.CutPrefix(s, "task:"); ok {
		return FromKindTask, strings.TrimSpace(rest)
	}
	if rest, ok := strings.CutPrefix(s, "env:"); ok {
		return FromKindEnv, strings.TrimSpace(rest)
	}
	return FromKindEnv, s
}

// ParseFrom is the exported counterpart of parseFrom for callers outside
// pkg/task (e.g. the reconciler validates from: task:<id> references).
func ParseFrom(s string) (FromKind, string) { return parseFrom(s) }
