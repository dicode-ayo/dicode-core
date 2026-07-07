package runtime

import (
	"os"
	"sort"
	"strings"

	"github.com/dicode/dicode/pkg/task"
)

// envWildcardSuffix marks a pattern env entry. A bare permissions.env name
// ending in "*" forwards every host var sharing the literal prefix before it
// (e.g. "GITHUB_*" matches GITHUB_TOKEN, GITHUB_SHA, …).
const envWildcardSuffix = "*"

// IsWildcardEnvEntry reports whether e is a bare, name-only pattern entry
// (e.g. "GITHUB_*"). Only bare entries can be patterns: an entry carrying
// from/secret/value is a concrete binding whose Name is the literal target,
// not a host-env glob. A lone "*" is not a pattern here — it is rejected at
// spec validation in favour of env_read_exposed.
func IsWildcardEnvEntry(e task.EnvEntry) bool {
	return e.Value == "" && e.Secret == "" && e.From == "" &&
		len(e.Name) > 1 && strings.HasSuffix(e.Name, envWildcardSuffix)
}

// wildcardBlocked reports whether name must never be reached by a wildcard
// match. It covers the daemon-only credential denylist and the per-run IPC
// handshake vars: a pattern like "DICODE_*" prefix-matches all of them, but
// none may ever leak into a task.
func wildcardBlocked(name string) bool {
	if neverForwardEnv[name] {
		return true
	}
	switch name {
	case "DICODE_SOCKET", "DICODE_TOKEN":
		return true
	}
	return false
}

// WildcardEnvNames expands the bare wildcard entries in spec.Permissions.Env
// against the current host environment and returns the matching host var
// names, de-duplicated and sorted. Denylisted daemon credentials and the
// internal IPC vars are always excluded, even when a pattern would match them.
//
// All three call sites share this one expansion so the set of vars forwarded
// (SubprocessEnv) exactly matches the set made readable by the sandbox (Deno
// --allow-env, the Python env-read guardrail).
func WildcardEnvNames(spec *task.Spec) []string {
	if spec == nil {
		return nil
	}
	var prefixes []string
	for _, e := range spec.Permissions.Env {
		if IsWildcardEnvEntry(e) {
			prefixes = append(prefixes, strings.TrimSuffix(e.Name, envWildcardSuffix))
		}
	}
	if len(prefixes) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		name := kv[:i]
		if seen[name] || wildcardBlocked(name) {
			continue
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				seen[name] = true
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}
