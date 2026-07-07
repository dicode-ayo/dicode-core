package python

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

func specWithPerms(perms task.Permissions) *task.Spec {
	return &task.Spec{ID: "t/perm", TaskDir: "/srv/tasks/perm", Permissions: perms}
}

func TestBuildGuardPolicy_NetModes(t *testing.T) {
	cases := []struct {
		name      string
		net       []string
		wantMode  string
		wantHosts []string
	}{
		{"omitted denies all", nil, "deny", nil},
		{"wildcard is unrestricted", []string{"*"}, "unrestricted", nil},
		{"hosts form an allowlist", []string{"api.github.com", "hooks.slack.com:443"}, "allowlist", []string{"api.github.com", "hooks.slack.com:443"}},
		{"explicit empty denies all", []string{}, "deny", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pol := buildGuardPolicy(specWithPerms(task.Permissions{Net: tc.net}), "/tmp/dicode.sock", nil)
			if pol.Net.Mode != tc.wantMode {
				t.Errorf("net mode = %q, want %q", pol.Net.Mode, tc.wantMode)
			}
			if len(pol.Net.Hosts) != len(tc.wantHosts) {
				t.Fatalf("net hosts = %v, want %v", pol.Net.Hosts, tc.wantHosts)
			}
			for i := range tc.wantHosts {
				if pol.Net.Hosts[i] != tc.wantHosts[i] {
					t.Errorf("net hosts = %v, want %v", pol.Net.Hosts, tc.wantHosts)
				}
			}
		})
	}
}

func TestBuildGuardPolicy_RunModes(t *testing.T) {
	cases := []struct {
		name     string
		run      []string
		wantMode string
		wantCmds []string
	}{
		{"omitted denies all", nil, "deny", nil},
		{"wildcard allows all", []string{"*"}, "allow", nil},
		{"commands form an allowlist", []string{"git", "curl"}, "allowlist", []string{"git", "curl"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pol := buildGuardPolicy(specWithPerms(task.Permissions{Run: tc.run}), "/tmp/dicode.sock", nil)
			if pol.Run.Mode != tc.wantMode {
				t.Errorf("run mode = %q, want %q", pol.Run.Mode, tc.wantMode)
			}
			if len(pol.Run.Commands) != len(tc.wantCmds) {
				t.Fatalf("run commands = %v, want %v", pol.Run.Commands, tc.wantCmds)
			}
			for i := range tc.wantCmds {
				if pol.Run.Commands[i] != tc.wantCmds[i] {
					t.Errorf("run commands = %v, want %v", pol.Run.Commands, tc.wantCmds)
				}
			}
		})
	}
}

func TestBuildGuardPolicy_FSWriteResolution(t *testing.T) {
	spec := specWithPerms(task.Permissions{FS: []task.FSEntry{
		{Path: "data", Permission: "rw"},        // relative → joined to TaskDir
		{Path: "/var/log/out", Permission: "w"}, // absolute → kept
		{Path: "readonly", Permission: "r"},     // read entry → not in write allowlist
	}})
	pol := buildGuardPolicy(spec, "/tmp/dicode.sock", nil)

	want := []string{
		"/tmp/dicode.sock", // IPC socket always writable
		filepath.Join(spec.TaskDir, "data"),
		"/var/log/out",
	}
	if len(pol.FSWrite) != len(want) {
		t.Fatalf("fs_write = %v, want %v", pol.FSWrite, want)
	}
	for i := range want {
		if pol.FSWrite[i] != want[i] {
			t.Errorf("fs_write[%d] = %q, want %q", i, pol.FSWrite[i], want[i])
		}
	}
}

func TestBuildGuard_EmbedsPolicyJSON(t *testing.T) {
	pol := buildGuardPolicy(specWithPerms(task.Permissions{
		Net: []string{"api.github.com"},
		Run: []string{"git"},
	}), "/tmp/dicode.sock", nil)

	guard, err := buildGuard(pol)
	if err != nil {
		t.Fatalf("buildGuard: %v", err)
	}
	if strings.Contains(guard, "__DICODE_POLICY__") {
		t.Error("guard still contains the policy placeholder")
	}

	// The policy is embedded as a Python string literal containing JSON;
	// the literal uses JSON string escaping, so unescape and round-trip it.
	start := strings.Index(guard, `_json.loads(`)
	if start == -1 {
		t.Fatal("guard missing _json.loads call")
	}
	rest := guard[start+len(`_json.loads(`):]
	end := strings.Index(rest, ")\n")
	if end == -1 {
		t.Fatal("guard missing _json.loads closing paren")
	}
	var inner string
	if err := json.Unmarshal([]byte(rest[:end]), &inner); err != nil {
		t.Fatalf("embedded literal is not a JSON string: %v", err)
	}
	var got guardPolicy
	if err := json.Unmarshal([]byte(inner), &got); err != nil {
		t.Fatalf("embedded policy is not valid JSON: %v", err)
	}
	if got.Net.Mode != "allowlist" || len(got.Net.Hosts) != 1 || got.Net.Hosts[0] != "api.github.com" {
		t.Errorf("round-tripped net policy = %+v", got.Net)
	}
	if got.Run.Mode != "allowlist" || len(got.Run.Commands) != 1 || got.Run.Commands[0] != "git" {
		t.Errorf("round-tripped run policy = %+v", got.Run)
	}
	if len(got.FSWrite) != 1 || got.FSWrite[0] != "/tmp/dicode.sock" {
		t.Errorf("round-tripped fs_write = %v", got.FSWrite)
	}
}

func TestBuildGuardPolicy_ProtectedPathsBecomeDenyList(t *testing.T) {
	// A broad "w" grant on the config dir would otherwise cover dicode.lock;
	// the protected paths must land in fs_deny so the hook rejects them.
	spec := specWithPerms(task.Permissions{FS: []task.FSEntry{
		{Path: "/etc/dicode", Permission: "w"},
	}})
	protected := []string{"/etc/dicode/dicode.lock", "/etc/dicode/../dicode/dicode.yaml"}
	pol := buildGuardPolicy(spec, "/tmp/dicode.sock", protected)

	want := []string{"/etc/dicode/dicode.lock", "/etc/dicode/dicode.yaml"}
	if len(pol.FSDeny) != len(want) {
		t.Fatalf("fs_deny = %v, want %v", pol.FSDeny, want)
	}
	for i := range want {
		if pol.FSDeny[i] != want[i] {
			t.Errorf("fs_deny[%d] = %q, want %q (paths must be cleaned)", i, pol.FSDeny[i], want[i])
		}
	}
}

func TestBuildGuardPolicy_EnvAllowed_WithDeclaredVars(t *testing.T) {
	// When env_read_exposed=false and env vars are declared, EnvAllowed
	// must contain the declared names and the essential set.
	spec := specWithPerms(task.Permissions{
		Env: []task.EnvEntry{
			{Name: "MY_API_KEY"},
			{Name: "ANOTHER_VAR"},
		},
	})
	pol := buildGuardPolicy(spec, "/tmp/dicode.sock", nil)

	// Must contain declared names
	names := make(map[string]bool, len(pol.EnvAllowed))
	for _, n := range pol.EnvAllowed {
		names[n] = true
	}
	for _, want := range []string{
		"MY_API_KEY", "ANOTHER_VAR",
		// Essential set samples (locale, proxy, TLS, cache, IPC).
		"PATH", "HOME", "LANG", "HTTP_PROXY", "SSL_CERT_FILE", "XDG_CACHE_HOME",
		"DICODE_SOCKET", "DICODE_TOKEN",
	} {
		if !names[want] {
			t.Errorf("EnvAllowed missing %q; got %v", want, pol.EnvAllowed)
		}
	}
	// DENO_DIR is Deno-specific and must never appear in the Python essential set.
	if names["DENO_DIR"] {
		t.Errorf("EnvAllowed must not contain DENO_DIR (Deno-specific); got %v", pol.EnvAllowed)
	}
}

func TestBuildGuardPolicy_EnvAllowed_Wildcard(t *testing.T) {
	// A pattern entry adds its expanded host matches to EnvAllowed (never the
	// literal "GITHUB_*"), and never a denylisted daemon credential it would
	// prefix-match.
	// Collision-proof prefix so match assertions hold under CI's ambient env.
	t.Setenv("WILDTEST_TOKEN", "gh")
	t.Setenv("WILDTEST_SHA", "sha")
	t.Setenv("DICODE_MASTER_KEY", "root")

	spec := specWithPerms(task.Permissions{
		Env: []task.EnvEntry{{Name: "WILDTEST_*"}},
	})
	pol := buildGuardPolicy(spec, "/tmp/dicode.sock", nil)

	names := make(map[string]bool, len(pol.EnvAllowed))
	for _, n := range pol.EnvAllowed {
		names[n] = true
	}
	for _, want := range []string{"WILDTEST_TOKEN", "WILDTEST_SHA"} {
		if !names[want] {
			t.Errorf("EnvAllowed missing wildcard match %q; got %v", want, pol.EnvAllowed)
		}
	}
	if names["WILDTEST_*"] {
		t.Errorf("literal pattern name leaked into EnvAllowed; got %v", pol.EnvAllowed)
	}
	if names["DICODE_MASTER_KEY"] {
		t.Errorf("wildcard leaked daemon credential into EnvAllowed; got %v", pol.EnvAllowed)
	}
}

func TestBuildGuardPolicy_EnvAllowed_EnvReadExposed(t *testing.T) {
	// env_read_exposed=true must set EnvAllowed=nil (no filter).
	spec := specWithPerms(task.Permissions{
		EnvReadExposed: true,
		Env:            []task.EnvEntry{{Name: "FOO"}},
	})
	pol := buildGuardPolicy(spec, "/tmp/dicode.sock", nil)
	if pol.EnvAllowed != nil {
		t.Errorf("env_read_exposed=true must set EnvAllowed=nil; got %v", pol.EnvAllowed)
	}
}

func TestBuildGuardPolicy_EnvAllowed_NoDeclarations(t *testing.T) {
	// No declared vars: EnvAllowed must still be set (to essential set only),
	// not nil, so filtering is active.
	spec := specWithPerms(task.Permissions{})
	pol := buildGuardPolicy(spec, "/tmp/dicode.sock", nil)
	if pol.EnvAllowed == nil {
		t.Error("EnvAllowed must be non-nil (essential set) even with no declared vars")
	}
	names := make(map[string]bool, len(pol.EnvAllowed))
	for _, n := range pol.EnvAllowed {
		names[n] = true
	}
	for _, want := range []string{
		"PATH", "HOME", "LANG", "HTTP_PROXY", "SSL_CERT_FILE", "XDG_CACHE_HOME",
		"DICODE_SOCKET", "DICODE_TOKEN",
	} {
		if !names[want] {
			t.Errorf("EnvAllowed (essential-only) missing %q", want)
		}
	}
	if names["DENO_DIR"] {
		t.Errorf("EnvAllowed must not contain DENO_DIR; got %v", pol.EnvAllowed)
	}
}

func TestBuildWrapper_GuardPlacement(t *testing.T) {
	script := "# /// script\n# dependencies = [\"requests\"]\n# ///\nresult = 1\n"
	pol := buildGuardPolicy(specWithPerms(task.Permissions{}), "/tmp/dicode.sock", nil)

	wrapped, err := buildWrapper([]byte(script), pol)
	if err != nil {
		t.Fatalf("buildWrapper: %v", err)
	}

	pepIdx := strings.Index(wrapped, "# /// script")
	sdkIdx := strings.Index(wrapped, "# === dicode SDK ===")
	guardIdx := strings.Index(wrapped, "# === permission guard ===")
	bodyIdx := strings.Index(wrapped, "# === task script ===")
	retIdx := strings.Index(wrapped, "# === return capture ===")
	for name, idx := range map[string]int{
		"pep723": pepIdx, "sdk": sdkIdx, "guard": guardIdx, "body": bodyIdx, "return": retIdx,
	} {
		if idx == -1 {
			t.Fatalf("wrapper missing %s section", name)
		}
	}
	// PEP 723 first (uv requirement), then SDK, then guard (so the hook
	// governs user code but not SDK setup), then task body, then epilogue.
	if !(pepIdx < sdkIdx && sdkIdx < guardIdx && guardIdx < bodyIdx && bodyIdx < retIdx) {
		t.Errorf("wrapper sections out of order: pep=%d sdk=%d guard=%d body=%d ret=%d",
			pepIdx, sdkIdx, guardIdx, bodyIdx, retIdx)
	}
}
