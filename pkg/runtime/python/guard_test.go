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
		{"omitted is unrestricted", nil, "unrestricted", nil},
		{"wildcard is unrestricted", []string{"*"}, "unrestricted", nil},
		{"hosts form an allowlist", []string{"api.github.com", "hooks.slack.com:443"}, "allowlist", []string{"api.github.com", "hooks.slack.com:443"}},
		{"explicit empty denies all", []string{}, "deny", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pol := buildGuardPolicy(specWithPerms(task.Permissions{Net: tc.net}), "/tmp/dicode.sock")
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
			pol := buildGuardPolicy(specWithPerms(task.Permissions{Run: tc.run}), "/tmp/dicode.sock")
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
	pol := buildGuardPolicy(spec, "/tmp/dicode.sock")

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
	}), "/tmp/dicode.sock")

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

func TestBuildWrapper_GuardPlacement(t *testing.T) {
	script := "# /// script\n# dependencies = [\"requests\"]\n# ///\nresult = 1\n"
	pol := buildGuardPolicy(specWithPerms(task.Permissions{}), "/tmp/dicode.sock")

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
