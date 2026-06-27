package python

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dicode/dicode/pkg/task"
)

//go:embed sdk/guard.py
var guardContent string

// guardPolicy is the JSON document embedded into the generated wrapper and
// consumed by the PEP 578 audit hook in sdk/guard.py.
//
// Enforcement scope (a guardrail on declared intent, not a security
// boundary — the task author is trusted):
//   - net, run, and fs writes are enforced.
//   - fs reads are NOT enforced: the interpreter, stdlib, and site-packages
//     read files constantly, so an in-interpreter read allowlist would break
//     normal execution. Known divergence from the Deno runtime.
//   - sys is NOT enforced: Deno's sys permission names (hostname, osRelease,
//     ...) have no usable Python equivalent.
type guardPolicy struct {
	Net     guardNet `json:"net"`
	Run     guardRun `json:"run"`
	FSWrite []string `json:"fs_write"`
	// FSDeny lists files (dicode.lock, dicode.yaml — the approval-gate state)
	// that no task may write, even when a broad "w"/"rw" grant covers their
	// directory. The audit hook checks this before the write allowlist so the
	// deny always wins.
	FSDeny []string `json:"fs_deny,omitempty"`
	// EnvAllowed lists env var names the task may read; nil means no filtering
	// (env_read_exposed=true). When set, guard.py replaces os.environ with a
	// filtered view restricted to these names.
	EnvAllowed []string `json:"env_allowed,omitempty"`
}

type guardNet struct {
	Mode  string   `json:"mode"` // "unrestricted" | "allowlist" | "deny"
	Hosts []string `json:"hosts,omitempty"`
}

type guardRun struct {
	Mode     string   `json:"mode"` // "allow" | "allowlist" | "deny"
	Commands []string `json:"commands,omitempty"`
}

// buildGuardPolicy maps spec.Permissions onto the audit-hook policy with the
// same semantics as the Deno runtime's buildDenoArgs:
//
//	net: deny by default — omit or [] → deny all; ["*"] → unrestricted;
//	     [h, ...] → allowlist. A task gets network access only by declaring
//	     it explicitly.
//	run: ["*"] → allow all; [c, ...] → allowlist; empty/omit → deny all.
//	fs:  "w"/"rw" entries form the write allowlist. "~" expands to the user
//	     home; relative paths resolve against the task dir. "r" entries are
//	     ignored (reads are unenforced, see guardPolicy).
//
// The IPC socket path is always writable so SDK traffic is never governed.
// protectedPaths (dicode.lock, dicode.yaml) become the deny list so no broad
// write grant can reach the approval-gate state.
func buildGuardPolicy(spec *task.Spec, socketPath string, protectedPaths []string) guardPolicy {
	var pol guardPolicy

	net := spec.Permissions.Net
	switch {
	case len(net) == 1 && net[0] == "*":
		pol.Net.Mode = "unrestricted"
	case len(net) > 0:
		pol.Net.Mode = "allowlist"
		pol.Net.Hosts = net
	default:
		pol.Net.Mode = "deny"
	}

	run := spec.Permissions.Run
	switch {
	case len(run) == 1 && run[0] == "*":
		pol.Run.Mode = "allow"
	case len(run) > 0:
		pol.Run.Mode = "allowlist"
		pol.Run.Commands = run
	default:
		pol.Run.Mode = "deny"
	}

	if socketPath != "" {
		pol.FSWrite = append(pol.FSWrite, socketPath)
	}
	for _, entry := range spec.Permissions.FS {
		if entry.Permission != "w" && entry.Permission != "rw" {
			continue
		}
		p := expandHome(entry.Path)
		if !filepath.IsAbs(p) {
			p = filepath.Join(spec.TaskDir, p)
		}
		pol.FSWrite = append(pol.FSWrite, p)
	}
	for _, p := range protectedPaths {
		if p == "" {
			continue
		}
		pol.FSDeny = append(pol.FSDeny, filepath.Clean(p))
	}

	// Env-read guardrail: when env_read_exposed is false (the default), restrict
	// os.environ reads to the declared env var names plus a runtime-essential set.
	// When env_read_exposed is true, leave EnvAllowed nil (no filter).
	if !spec.Permissions.EnvReadExposed {
		essential := []string{
			// Process basics.
			"PATH", "HOME", "USER", "LOGNAME", "TMPDIR",
			// Locale / timezone.
			"LANG", "LC_ALL", "LC_CTYPE", "TZ",
			// Cache, data, and config roots.
			"XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME",
			// TLS trust store overrides.
			"SSL_CERT_FILE", "SSL_CERT_DIR",
			// Proxies, honored by uv/requests.
			"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
			"http_proxy", "https_proxy", "no_proxy",
			// Deno cache location (kept for consistency with subprocenv.go).
			"DENO_DIR",
			// uv interpreter and cache locations.
			"UV_CACHE_DIR", "UV_PYTHON", "UV_PYTHON_INSTALL_DIR", "VIRTUAL_ENV",
			// IPC handshake coordinates.
			"DICODE_SOCKET", "DICODE_TOKEN",
		}
		seen := make(map[string]bool, len(essential)+len(spec.Permissions.Env))
		allowed := make([]string, 0, len(essential)+len(spec.Permissions.Env))
		for _, name := range essential {
			if !seen[name] {
				seen[name] = true
				allowed = append(allowed, name)
			}
		}
		for _, e := range spec.Permissions.Env {
			if e.Name == "" || seen[e.Name] {
				continue
			}
			seen[e.Name] = true
			allowed = append(allowed, e.Name)
		}
		pol.EnvAllowed = allowed
	}

	return pol
}

// buildGuard renders the audit-hook source with the policy embedded. The
// policy JSON is double-encoded so it lands as a quoted string literal that
// is valid Python source (JSON string escapes are a subset of Python's).
func buildGuard(pol guardPolicy) (string, error) {
	raw, err := json.Marshal(pol)
	if err != nil {
		return "", fmt.Errorf("marshal guard policy: %w", err)
	}
	lit, err := json.Marshal(string(raw))
	if err != nil {
		return "", fmt.Errorf("encode guard policy literal: %w", err)
	}
	out := strings.Replace(guardContent, `"__DICODE_POLICY__"`, string(lit), 1)
	if out == guardContent {
		return "", fmt.Errorf("guard template missing policy placeholder")
	}
	return out, nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") || p == "~" {
		home, err := os.UserHomeDir()
		if err == nil {
			return home + p[1:]
		}
	}
	return p
}
