package secrets

import (
	"sort"
	"strings"
)

// RedactionMarker is what replaces a matched secret value in any string
// passed through a Redactor. Chosen to be obviously non-secret and to survive
// naive substring checks for "leaked the actual value" in test assertions.
const RedactionMarker = "<REDACTED>"

// Redactor replaces known secret values in log lines with RedactionMarker.
// The set of values is snapshot at construction and never refreshed — the
// intended usage is one Redactor per task run, built from the secrets
// resolved for that run's env permissions. Refreshing on every log line
// would pull a database round-trip into the hot path of task stdout.
//
// The zero value is safe to use and redacts nothing. Methods are safe for
// concurrent use (internally a *strings.Replacer, which is stateless).
type Redactor struct {
	// nil when there are no values to redact — the hot path bails early.
	replacer *strings.Replacer
}

// NewRedactor returns a Redactor that replaces every distinct non-empty
// value from `values` with RedactionMarker. Keys are ignored; only values
// matter. Empty values are dropped (a 0-length "secret" would otherwise
// match every position). Single-character values ARE accepted — if a
// caller stored them as a secret we redact them, even if the log output
// becomes unreadable; leaking the value is always worse than a noisy log.
//
// Duplicates are dropped so repeated secret registration doesn't produce
// a duplicate-key argument to strings.NewReplacer (which would panic).
func NewRedactor(values map[string]string) *Redactor {
	if len(values) == 0 {
		return &Redactor{}
	}
	seen := make(map[string]struct{}, len(values))
	uniq := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		uniq = append(uniq, v)
	}
	if len(uniq) == 0 {
		return &Redactor{}
	}
	// strings.Replacer matches pairs in argument order — whichever `old`
	// appears first wins at a given position. Sort longest-first so a
	// secret that contains another secret as a substring is replaced as
	// a whole, not piecewise ("foobar" stays intact even if "foo" is
	// also registered).
	sort.SliceStable(uniq, func(i, j int) bool { return len(uniq[i]) > len(uniq[j]) })

	pairs := make([]string, 0, len(uniq)*2)
	for _, v := range uniq {
		pairs = append(pairs, v, RedactionMarker)
	}
	return &Redactor{replacer: strings.NewReplacer(pairs...)}
}

// RedactString returns s with every occurrence of a known secret value
// replaced by RedactionMarker. Safe on a nil receiver (returns s
// unchanged), safe on the zero-value Redactor, and safe on concurrent
// callers.
func (r *Redactor) RedactString(s string) string {
	if r == nil || r.replacer == nil || s == "" {
		return s
	}
	return r.replacer.Replace(s)
}
