package config

import "strings"

// SplitTaskID splits a namespaced task ID into the top-level source key
// (matches dicode.yaml spec.entries.<key>) and the sub-path used for
// overrides.entries.<sub>. Returns ok=false if id has no separator.
//
// Note: nested IDs encode as a flat sub-key (e.g. "platform/nginx") rather
// than walking nested entries — the resolver looks up parent.Entries by
// the leaf-relative key it computed during recursion, which matches the
// flat encoding for top-level overrides applied at the source root.
//
//	"buildin/temp-cleanup" → ("buildin", "temp-cleanup", true)
//	"infra/platform/nginx" → ("infra",   "platform/nginx", true)
//	"buildin"              → ("",        "",              false)
func SplitTaskID(id string) (source, sub string, ok bool) {
	// Note: not strings.Cut — Cut returns the full input as `before` when
	// the separator is absent, but callers here expect both fields empty
	// in that case so they can fail-closed without re-checking ok.
	idx := strings.IndexByte(id, '/')
	if idx < 0 {
		return "", "", false
	}
	return id[:idx], id[idx+1:], true
}
