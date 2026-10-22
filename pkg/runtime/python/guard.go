package python

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/runtime/perms"
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
// A Unix-socket IPC path is always writable so SDK traffic is never governed.
// protectedPaths (dicode.lock, dicode.yaml) become the deny list so no broad
// write grant can reach the approval-gate state.
func buildGuardPolicy(spec *task.Spec, ipcAddr string, protectedPaths []string) guardPolicy {
	var pol guardPolicy

	eff := perms.Resolve(spec)

	switch eff.NetMode {
	case perms.NetUnrestricted:
		pol.Net.Mode = "unrestricted"
	case perms.NetAllowlist:
		pol.Net.Mode = "allowlist"
		pol.Net.Hosts = eff.NetHosts
	default:
		pol.Net.Mode = "deny"
	}

	switch eff.RunMode {
	case perms.RunAllow:
		pol.Run.Mode = "allow"
	case perms.RunAllowlist:
		pol.Run.Mode = "allowlist"
		pol.Run.Commands = eff.RunCommands
	default:
		pol.Run.Mode = "deny"
	}

	// A loopback IPC endpoint is not a path and needs no grant of either kind:
	// the SDK opens the connection before this guard installs, and no later
	// SDK call re-connects.
	if ipcAddr != "" && !ipc.IsLoopbackAddr(ipcAddr) {
		pol.FSWrite = append(pol.FSWrite, ipcAddr)
	}
	pol.FSWrite = append(pol.FSWrite, eff.WritePaths...)
	for _, p := range protectedPaths {
		if p == "" {
			continue
		}
		pol.FSDeny = append(pol.FSDeny, filepath.Clean(p))
	}

	// Env-read guardrail: when env_read_exposed is false (the default), restrict
	// os.environ reads to the declared env var names plus a runtime-essential set.
	// When env_read_exposed is true, leave EnvAllowed nil (no filter).
	if !eff.EnvReadExposed {
		// Larger than the Deno sandbox's baseline (see package perms): the
		// interpreter and uv read these themselves.
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
			// uv interpreter and cache locations.
			"UV_CACHE_DIR", "UV_PYTHON", "UV_PYTHON_INSTALL_DIR", "VIRTUAL_ENV",
			// IPC handshake coordinates.
			"DICODE_SOCKET", "DICODE_TOKEN",
		}
		seen := make(map[string]bool, len(essential)+len(eff.EnvNames))
		allowed := make([]string, 0, len(essential)+len(eff.EnvNames))
		for _, name := range essential {
			if !seen[name] {
				seen[name] = true
				allowed = append(allowed, name)
			}
		}
		for _, name := range eff.EnvNames {
			if seen[name] {
				continue
			}
			seen[name] = true
			allowed = append(allowed, name)
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
