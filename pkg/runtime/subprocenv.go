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

// neverForwardEnv are daemon-only credentials that a bare permissions.env
// entry must never forward to a task subprocess, regardless of what a
// task.yaml declares.
var neverForwardEnv = map[string]bool{
	"DICODE_MASTER_KEY":  true, // root key deriving every secret
	"DICODE_API_KEY":     true, // daemon admin control-plane credential
	"DICODE_MCP_API_KEY": true, // daemon MCP control-plane credential
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
			// "*" is the Deno --allow-env grant-all sentinel (see
			// envWildcard in pkg/runtime/deno), not a host var named "*" to
			// forward. It widens read permission only; the script still sees
			// just the allowlisted/resolved env assembled here.
			if e.Name == "*" {
				continue
			}
			// Daemon-only credentials must never reach a task, even if a
			// task.yaml names them: the master key derives every secret, and
			// the API keys authenticate to the daemon's own admin/MCP control
			// plane — a task has no legitimate need for any of them.
			if neverForwardEnv[e.Name] {
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
