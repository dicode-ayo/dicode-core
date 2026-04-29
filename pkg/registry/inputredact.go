package registry

import (
	"encoding/json"
	"fmt"
	"strings"
)

// WebhookFields is the subset of HTTP context the redaction layer needs.
// Mirrors pkgruntime.WebhookContext to avoid a runtime → registry import edge.
// The trigger engine maps its WebhookContext into this struct before calling
// BuildPersistedInputFromRunOpts.
type WebhookFields struct {
	Method          string
	Path            string
	Headers         map[string][]string
	Query           map[string][]string
	RawBody         []byte
	ContentType     string
	BodyFullTextual bool
}

// BuildPersistedInputFromRunOpts is the public helper trigger.Engine uses to
// build a PersistedInput from a run's source + params + input. Marshalling +
// redaction happen here so callers don't need direct access to redactParams.
//
// web must be non-nil for webhook-triggered runs; it carries the raw HTTP
// context so content-type-aware body redaction (Task 7) and header/query
// redaction (Task 6) are actually invoked. For non-webhook sources (cron,
// chain, manual, daemon) pass nil and the prior parsed-input fallback applies.
func BuildPersistedInputFromRunOpts(source string, params map[string]string, input any, web *WebhookFields) PersistedInput {
	in := PersistedInput{Source: source}
	redacted := []string{}

	// params is map[string]string — wrap into a redactable map.
	if len(params) > 0 {
		p := make(map[string]any, len(params))
		for k, v := range params {
			p[k] = v
		}
		in.Params = redactParams(p, "params", &redacted).(map[string]any)
	}

	if web != nil {
		// Webhook-triggered run: populate HTTP-level fields with full redaction.
		in.Method = web.Method
		in.Path = web.Path
		if len(web.Headers) > 0 {
			in.Headers = redactHeaders(web.Headers, &redacted)
		}
		if len(web.Query) > 0 {
			in.Query = redactQuery(web.Query, &redacted)
		}
		if len(web.RawBody) > 0 && web.ContentType != "" {
			body := redactBody(web.RawBody, web.ContentType, web.BodyFullTextual, &redacted)
			in.BodyKind = body.BodyKind
			in.Body = body.Body
			in.BodyHash = body.BodyHash
			in.BodyParts = body.BodyParts
		}
	} else if input != nil {
		// Non-webhook trigger (chain, manual, cron, daemon): preserve the prior
		// behaviour of storing the parsed input as redacted JSON. No Method/
		// Path/Headers/Query because there is no HTTP context here.
		walked := redactParams(input, "input", &redacted)
		if raw, err := json.Marshal(walked); err == nil {
			in.Body = raw
			in.BodyKind = "json"
		}
	}

	in.RedactedFields = redacted
	return in
}

// PersistedInput is the structured shape of a run input as it lives encrypted
// at rest. Fields cover the union of webhook (HTTP), manual (params), cron
// (none), chain (params + parent context), replay (carries persisted input
// forward), and daemon trigger sources.
//
// All values reaching this struct are post-redaction: header/query/param
// names matching shouldRedactName have had their values replaced with
// redactPlaceholder. RedactedFields lists the dotted paths that were
// redacted, surfaced to the auto-fix agent prompt so it can reason about
// what's missing without seeing secret values.
type PersistedInput struct {
	Source         string              `json:"source"`                    // webhook | cron | manual | chain | daemon | replay
	Method         string              `json:"method,omitempty"`          // webhook only
	Path           string              `json:"path,omitempty"`            // webhook only
	Headers        map[string][]string `json:"headers,omitempty"`         // webhook; multi-valued for HTTP fidelity; post-redaction
	Query          map[string][]string `json:"query,omitempty"`           // webhook; same shape; post-redaction
	Body           json.RawMessage     `json:"body,omitempty"`            // see body policy in inputredact_body.go (Task 7)
	BodyKind       string              `json:"body_kind,omitempty"`       // "json" | "form" | "multipart" | "binary" | "text" | "omitted"
	BodyHash       string              `json:"body_hash,omitempty"`       // sha256 hex; present for omitted/binary/multipart
	BodyParts      []PartMeta          `json:"body_parts,omitempty"`      // multipart only
	Params         map[string]any      `json:"params,omitempty"`          // post-redaction (recursive)
	RedactedFields []string            `json:"redacted_fields,omitempty"` // dotted paths of redacted fields
}

// PartMeta describes a single multipart/form-data part. Values are NEVER
// stored — only structural metadata. Used by body redaction (Task 7).
type PartMeta struct {
	Name        string `json:"name"`               // form-field name (after redaction if name matched)
	Kind        string `json:"kind"`               // "field" | "file"
	Filename    string `json:"filename,omitempty"` // file parts only; redacted if name matched
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size"`
}

// redactPlaceholder is the value substituted for any redacted scalar.
const redactPlaceholder = "<redacted>"

// denyListExact is the case-insensitive set of header/key names that are
// always redacted. Compared lowercased against the lowercased input name.
var denyListExact = map[string]struct{}{
	"authorization":       {},
	"cookie":              {},
	"set-cookie":          {},
	"x-hub-signature":     {},
	"x-hub-signature-256": {},
	"x-dicode-signature":  {},
	"x-dicode-timestamp":  {},
	"x-slack-signature":   {},
	"x-line-signature":    {},
	"password":            {},
	"passphrase":          {},
	"api_key":             {},
	"apikey":              {},
	"api-key":             {},
	"secret":              {},
	"token":               {},
	"bearer":              {},
}

// denyListSubstrings is matched as a case-insensitive substring against the
// lowercased input name. Catches custom names like MY_SLACK_TOKEN and
// gh-secret-XYZ that don't appear in denyListExact verbatim. Over-redaction
// (e.g. legitimate field "tokens_per_minute") is the safe failure mode.
var denyListSubstrings = []string{
	"signature",
	"token",
	"secret",
	"password",
	"key",
}

// shouldRedactName returns true if the lowercased name matches any deny-list
// rule (exact or substring). Used by header/query/param redaction (Task 6)
// and body redaction (Task 7).
func shouldRedactName(name string) bool {
	lower := strings.ToLower(name)
	if _, ok := denyListExact[lower]; ok {
		return true
	}
	for _, substr := range denyListSubstrings {
		if strings.Contains(lower, substr) {
			return true
		}
	}
	return false
}

// redactHeaders walks an HTTP-style map[string][]string. For each key whose
// name matches the deny-list, every value in the slice is replaced with the
// redactPlaceholder. Names of redacted keys are recorded as "headers.<name>"
// in `redacted`.
//
// Returns a NEW map; does not mutate the input.
func redactHeaders(in map[string][]string, redacted *[]string) map[string][]string {
	return redactStringSliceMap(in, "headers", redacted)
}

// redactQuery is the same as redactHeaders but with the "query." path prefix.
func redactQuery(in map[string][]string, redacted *[]string) map[string][]string {
	return redactStringSliceMap(in, "query", redacted)
}

// redactStringSliceMap is the shared implementation for header/query
// redaction. Single-valued substitution preserves length information without
// leaking content.
func redactStringSliceMap(in map[string][]string, prefix string, redacted *[]string) map[string][]string {
	if len(in) == 0 {
		return in
	}
	out := make(map[string][]string, len(in))
	for name, vals := range in {
		if shouldRedactName(name) {
			redactedVals := make([]string, len(vals))
			for i := range redactedVals {
				redactedVals[i] = redactPlaceholder
			}
			out[name] = redactedVals
			*redacted = append(*redacted, prefix+"."+name)
		} else {
			// Defensive copy: never share the input slice.
			out[name] = append([]string(nil), vals...)
		}
	}
	return out
}

// maxRedactionDepth caps recursion in redactParams to avoid stack overflows
// on adversarially deep JSON structures.
const maxRedactionDepth = 64

// redactParams recursively walks a generic value (typically map[string]any
// from JSON) replacing values for keys whose names match the deny-list.
// Lists are walked positionally with [N] in the path. Returns a new value;
// does NOT mutate the input.
func redactParams(v any, path string, redacted *[]string) any {
	return redactParamsDepth(v, path, redacted, 0)
}

func redactParamsDepth(v any, path string, redacted *[]string, depth int) any {
	if depth >= maxRedactionDepth {
		return "<redacted-too-deep>"
	}
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, child := range x {
			childPath := path + "." + k
			if shouldRedactName(k) {
				out[k] = redactPlaceholder
				*redacted = append(*redacted, childPath)
				continue
			}
			out[k] = redactParamsDepth(child, childPath, redacted, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, child := range x {
			out[i] = redactParamsDepth(child, fmt.Sprintf("%s[%d]", path, i), redacted, depth+1)
		}
		return out
	default:
		return v
	}
}
