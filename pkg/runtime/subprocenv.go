package runtime

import (
	"os"

	"github.com/dicode/dicode/pkg/task"
)

// hostPassthroughVars are the only daemon env vars a task subprocess
// inherits. The daemon environment can carry secrets (operator-exported
// credentials, deploy tokens), so everything not on this list is withheld.
// This list covers what the runtime binaries themselves (deno, uv, the
// provisioned Python) need to locate caches, trust stores, and proxies —
// the script-visible env is restricted separately (Deno --allow-env,
// Python SDK env.get).
var hostPassthroughVars = []string{
	// Process basics.
	"PATH", "HOME", "USER", "LOGNAME", "TMPDIR",
	// Locale / timezone.
	"LANG", "LC_ALL", "LC_CTYPE", "TZ",
	// Cache, data, and config roots (deno and uv both derive default
	// cache locations from these).
	"XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME",
	// TLS trust store overrides.
	"SSL_CERT_FILE", "SSL_CERT_DIR",
	// Proxies, honored by both deno and uv/requests.
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
	// Deno cache location.
	"DENO_DIR",
	// uv interpreter and cache locations.
	"UV_CACHE_DIR", "UV_PYTHON", "UV_PYTHON_INSTALL_DIR", "VIRTUAL_ENV",
}

// SubprocessEnv builds the full environment for a task subprocess:
// allowlisted host vars (only those actually set in the daemon env), host
// values for bare permissions.env entries (a name-only entry allowlists a
// host var, so its value must be forwarded explicitly now that the daemon
// env is not inherited wholesale), the per-run IPC coordinates, and the
// resolved task env. Later entries win on duplicate names (os/exec dedupes
// keeping the last), so resolved values override any host passthrough.
func SubprocessEnv(spec *task.Spec, resolved map[string]string, socketPath, token string) []string {
	env := make([]string, 0, len(hostPassthroughVars)+len(resolved)+8)
	for _, name := range hostPassthroughVars {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	if spec != nil {
		for _, e := range spec.Permissions.Env {
			if e.Name == "" || e.Value != "" || e.Secret != "" || e.From != "" {
				continue
			}
			// The master key must never reach a task, even if a task.yaml
			// names it.
			if e.Name == "DICODE_MASTER_KEY" {
				continue
			}
			if _, ok := resolved[e.Name]; ok {
				continue
			}
			if v, ok := os.LookupEnv(e.Name); ok {
				env = append(env, e.Name+"="+v)
			}
		}
	}
	env = append(env, "DICODE_SOCKET="+socketPath, "DICODE_TOKEN="+token)
	for k, v := range resolved {
		env = append(env, k+"="+v)
	}
	return env
}
