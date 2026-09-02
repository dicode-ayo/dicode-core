package audit

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Redacted is the placeholder substituted for any sanitized value.
// Audit rows must NEVER contain secret values — only the fact that a
// (redacted) value was passed.
const Redacted = "[REDACTED]"

// denyExact is the case-insensitive set of param names whose values are
// always redacted. pkg/registry/inputredact.go keeps a separate list for the
// persisted run input; the two share the credential names but not the header
// names, and a credential name added to either belongs in both.
var denyExact = map[string]struct{}{
	"authorization": {},
	"cookie":        {},
	"password":      {},
	"passphrase":    {},
	"api_key":       {},
	"apikey":        {},
	"api-key":       {},
	"secret":        {},
	"token":         {},
	"bearer":        {},
	"credential":    {},
	"credentials":   {},
	// The URL embeds a single-use approval token, so the value is a bearer
	// credential despite the innocuous name.
	"approve_url": {},
}

// denySubstrings is matched case-insensitively as a substring of the param
// name. Over-redaction (e.g. "tokens_per_minute") is the safe failure mode.
var denySubstrings = []string{
	"signature",
	"token",
	"secret",
	"password",
	"passphrase",
	"credential",
	"key",
}

// refPrefixes are value prefixes that reference daemon-resolved material —
// `env:` fields and `secret:`/`secrets:` references from task.yaml. The
// reference itself names the secret, and the value the daemon resolves it
// to must never reach the audit log, so the whole value is redacted.
var refPrefixes = []string{"env:", "secret:", "secrets:"}

// shouldRedactName reports whether a param name matches the deny-list.
func shouldRedactName(name string) bool {
	lower := strings.ToLower(name)
	if _, ok := denyExact[lower]; ok {
		return true
	}
	for _, sub := range denySubstrings {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}

// isSecretRef reports whether a string value is an env:/secret(s): reference.
func isSecretRef(v string) bool {
	lower := strings.ToLower(strings.TrimSpace(v))
	for _, p := range refPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// SanitizeParams sanitizes a flat string param map (the shape used by task
// triggers) and returns it as a deterministic JSON object string. Values
// are replaced with Redacted when either the key name matches the
// deny-list or the value is an env:/secret(s): reference. Returns "" for
// an empty/nil map so callers can store NULL-ish params cheaply.
func SanitizeParams(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}
	out := make(map[string]string, len(params))
	for k, v := range params {
		if shouldRedactName(k) || isSecretRef(v) {
			out[k] = Redacted
			continue
		}
		out[k] = v
	}
	raw, err := json.Marshal(out) // map keys are sorted by encoding/json
	if err != nil {
		return ""
	}
	return string(raw)
}

// maxSanitizeDepth caps recursion on adversarially deep structures.
const maxSanitizeDepth = 64

// SanitizeAny sanitizes an arbitrary JSON-shaped value (used for MCP tool
// arguments) and returns its JSON encoding. Same redaction rules as
// SanitizeParams, applied recursively. Returns "" for nil.
func SanitizeAny(v any) string {
	if v == nil {
		return ""
	}
	raw, err := json.Marshal(sanitizeValue(v, 0))
	if err != nil {
		return ""
	}
	return string(raw)
}

func sanitizeValue(v any, depth int) any {
	if depth >= maxSanitizeDepth {
		return Redacted
	}
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, child := range x {
			if shouldRedactName(k) {
				out[k] = Redacted
				continue
			}
			out[k] = sanitizeValue(child, depth+1)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(x))
		for k, child := range x {
			if shouldRedactName(k) || isSecretRef(child) {
				out[k] = Redacted
				continue
			}
			out[k] = child
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, child := range x {
			out[i] = sanitizeValue(child, depth+1)
		}
		return out
	case string:
		if isSecretRef(x) {
			return Redacted
		}
		return x
	case nil, bool, float64, float32, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, json.Number:
		return x
	default:
		// Unknown Go type — fall back to its string form, redacting refs.
		s := fmt.Sprintf("%v", x)
		if isSecretRef(s) {
			return Redacted
		}
		return s
	}
}
