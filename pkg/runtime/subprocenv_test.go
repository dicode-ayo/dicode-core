package runtime

import (
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

func envMap(env []string) map[string]string {
	// Last occurrence wins, matching os/exec dedupe semantics.
	out := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

func TestSubprocessEnv_ExcludesDaemonEnv(t *testing.T) {
	t.Setenv("DICODE_MASTER_KEY", "super-secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "leaky")
	t.Setenv("PATH", "/usr/bin")

	env := envMap(SubprocessEnv(&task.Spec{}, nil, "/tmp/sock", "tok"))

	if _, ok := env["DICODE_MASTER_KEY"]; ok {
		t.Error("DICODE_MASTER_KEY leaked into subprocess env")
	}
	if _, ok := env["AWS_SECRET_ACCESS_KEY"]; ok {
		t.Error("arbitrary daemon env var leaked into subprocess env")
	}
	if env["PATH"] != "/usr/bin" {
		t.Errorf("PATH not passed through, got %q", env["PATH"])
	}
	if env["DICODE_SOCKET"] != "/tmp/sock" || env["DICODE_TOKEN"] != "tok" {
		t.Errorf("IPC vars missing: socket=%q token=%q", env["DICODE_SOCKET"], env["DICODE_TOKEN"])
	}
}

func TestSubprocessEnv_PassthroughOnlyWhenSet(t *testing.T) {
	t.Setenv("DENO_DIR", "/custom/deno")
	// UV_CACHE_DIR deliberately not set.
	env := SubprocessEnv(nil, nil, "/tmp/sock", "tok")
	m := envMap(env)
	if m["DENO_DIR"] != "/custom/deno" {
		t.Errorf("DENO_DIR not passed through, got %q", m["DENO_DIR"])
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "UV_CACHE_DIR=") {
			t.Error("unset allowlist var injected with empty value")
		}
	}
}

func TestSubprocessEnv_ResolvedVarsIncluded(t *testing.T) {
	env := envMap(SubprocessEnv(&task.Spec{}, map[string]string{"API_KEY": "abc123"}, "/tmp/sock", "tok"))
	if env["API_KEY"] != "abc123" {
		t.Errorf("resolved var missing, got %q", env["API_KEY"])
	}
}

func TestSubprocessEnv_BareAllowlistEntryForwardsHostValue(t *testing.T) {
	t.Setenv("UPSTREAM_URL", "http://example.test")
	t.Setenv("OTHER_HOST_VAR", "nope")
	t.Setenv("DICODE_MASTER_KEY", "root-key")
	t.Setenv("DICODE_API_KEY", "admin-key")
	t.Setenv("DICODE_MCP_API_KEY", "mcp-key")

	spec := &task.Spec{}
	spec.Permissions.Env = []task.EnvEntry{
		{Name: "UPSTREAM_URL"},                     // bare → forward host value
		{Name: "FROM_VAR", From: "env:SOMEWHERE"},  // resolver-handled, not forwarded here
		{Name: "SECRET_VAR", Secret: "secret-key"}, // resolver-handled
		{Name: "DICODE_MASTER_KEY"},                // daemon-only → never forwarded
		{Name: "DICODE_API_KEY"},                   // daemon-only → never forwarded
		{Name: "DICODE_MCP_API_KEY"},               // daemon-only → never forwarded
		{Name: "UNSET_BARE"},                       // not set on host → omitted
	}

	env := SubprocessEnv(spec, nil, "/tmp/sock", "tok")
	m := envMap(env)

	if m["UPSTREAM_URL"] != "http://example.test" {
		t.Errorf("bare allowlist host value not forwarded, got %q", m["UPSTREAM_URL"])
	}
	if _, ok := m["OTHER_HOST_VAR"]; ok {
		t.Error("undeclared host var forwarded")
	}
	for _, name := range []string{"FROM_VAR", "SECRET_VAR", "DICODE_MASTER_KEY", "DICODE_API_KEY", "DICODE_MCP_API_KEY", "UNSET_BARE"} {
		for _, kv := range env {
			if strings.HasPrefix(kv, name+"=") {
				t.Errorf("%s should not be present in subprocess env", name)
			}
		}
	}
}

// TestSubprocessEnv_WildcardEntryIsNoOpForForwarding: the "*" --allow-env
// grant-all sentinel must not be forwarded as a host var named "*", while
// named entries listed alongside it still forward their host values.
func TestSubprocessEnv_WildcardEntryIsNoOpForForwarding(t *testing.T) {
	t.Setenv("DICODE_DATADIR", "/var/lib/dicode")

	spec := &task.Spec{}
	spec.Permissions.Env = []task.EnvEntry{
		{Name: "*"},              // grant-all sentinel → no forwarding
		{Name: "DICODE_DATADIR"}, // named → forward host value
	}

	env := SubprocessEnv(spec, nil, "/tmp/sock", "tok")
	m := envMap(env)

	if _, ok := m["*"]; ok {
		t.Error(`"*" was forwarded as a host var named "*"`)
	}
	if m["DICODE_DATADIR"] != "/var/lib/dicode" {
		t.Errorf("named entry alongside wildcard not forwarded, got %q", m["DICODE_DATADIR"])
	}
}

func TestSubprocessEnv_ResolvedWinsOverBareHostValue(t *testing.T) {
	t.Setenv("DUAL_VAR", "host-value")
	spec := &task.Spec{}
	spec.Permissions.Env = []task.EnvEntry{{Name: "DUAL_VAR"}}

	env := envMap(SubprocessEnv(spec, map[string]string{"DUAL_VAR": "resolved-value"}, "/tmp/sock", "tok"))
	if env["DUAL_VAR"] != "resolved-value" {
		t.Errorf("resolved value should win over host value, got %q", env["DUAL_VAR"])
	}
}
