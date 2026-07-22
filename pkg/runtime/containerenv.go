package runtime

import (
	"sort"
)

// BuildContainerEnv assembles the environment for a container run as a
// deterministic, sorted slice of "KEY=VALUE" entries.
//
// envVars are the task's literal docker.env_vars; resolved is the task's
// declared permissions.env after the resolver produced values (including
// secret-store values). A resolved value wins on a key collision — the
// declared secret is the intended value for that name. Only these two maps
// contribute: a container never inherits the daemon's ambient host env.
func BuildContainerEnv(envVars map[string]string, resolved map[string]string) []string {
	merged := make(map[string]string, len(envVars)+len(resolved))
	for k, v := range envVars {
		merged[k] = v
	}
	for k, v := range resolved {
		merged[k] = v
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+merged[k])
	}
	return out
}
