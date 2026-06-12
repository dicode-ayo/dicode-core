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
//	net: omit → unrestricted; ["*"] → unrestricted; [h, ...] → allowlist;
//	     [] (explicit empty) → deny all. Omit=unrestricted is intentional
//	     parity with Deno; #379 tracks changing the default to deny.
//	run: ["*"] → allow all; [c, ...] → allowlist; empty/omit → deny all.
//	fs:  "w"/"rw" entries form the write allowlist. "~" expands to the user
//	     home; relative paths resolve against the task dir. "r" entries are
//	     ignored (reads are unenforced, see guardPolicy).
//
// The IPC socket path is always writable so SDK traffic is never governed.
func buildGuardPolicy(spec *task.Spec, socketPath string) guardPolicy {
	var pol guardPolicy

	net := spec.Permissions.Net
	switch {
	case net == nil, len(net) == 1 && net[0] == "*":
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
