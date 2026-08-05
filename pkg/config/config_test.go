package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExpandVars(t *testing.T) {
	vars := map[string]string{
		"HOME":      "/home/testuser",
		"CONFIGDIR": "/etc/dicode",
		"DATADIR":   "/var/lib/dicode",
	}

	tests := []struct {
		input string
		want  string
	}{
		{"${HOME}/tasks", "/home/testuser/tasks"},
		{"${CONFIGDIR}/certs", "/etc/dicode/certs"},
		{"${DATADIR}/data.db", "/var/lib/dicode/data.db"},
		{"/absolute/path", "/absolute/path"},
		{"${HOME}/${DATADIR}/nested", "/home/testuser//var/lib/dicode/nested"},
		{"no-vars", "no-vars"},
		{"", ""},
	}

	for _, tt := range tests {
		got := expandVars(tt.input, vars)
		if got != tt.want {
			t.Errorf("expandVars(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestLoadWithVars(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := `
spec:
  entries:
    my-tasks:
      ref:
        path: ${HOME}/my-tasks
    tasks:
      ref:
        path: ${CONFIGDIR}/tasks
database:
  type: sqlite
  path: ${DATADIR}/test.db
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	home, _ := os.UserHomeDir()

	myTasksEntry := cfg.Spec.Entries["my-tasks"]
	if myTasksEntry == nil || myTasksEntry.Ref == nil {
		t.Fatal("spec.entries[my-tasks] not found")
	}
	if myTasksEntry.Ref.Path != home+"/my-tasks" {
		t.Errorf("spec.entries[my-tasks].ref.path = %q, want %q", myTasksEntry.Ref.Path, home+"/my-tasks")
	}

	tasksEntry := cfg.Spec.Entries["tasks"]
	if tasksEntry == nil || tasksEntry.Ref == nil {
		t.Fatal("spec.entries[tasks] not found")
	}
	if tasksEntry.Ref.Path != dir+"/tasks" {
		t.Errorf("spec.entries[tasks].ref.path = %q, want %q", tasksEntry.Ref.Path, dir+"/tasks")
	}

	wantDB := home + "/.dicode/test.db"
	if cfg.Database.Path != wantDB {
		t.Errorf("database.path = %q, want %q", cfg.Database.Path, wantDB)
	}
}

// TestLoad_RejectsLegacySources ensures the old `sources:` array is rejected
// at load time with a clear error pointing at the migration guide.
func TestLoad_RejectsLegacySources(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := `
sources:
  - type: local
    path: /tmp/tasks
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("Load accepted legacy sources array; want error")
	}
	if !strings.Contains(err.Error(), "spec.entries") {
		t.Errorf("error = %v; want mention of spec.entries", err)
	}
}

// TestLoad_SourceSecurityAllowlist parses a valid source_security block into
// the runtime allowlist, honouring hostnames on the literal-host layer and
// CIDRs on the resolved-IP layer.
func TestLoad_SourceSecurityAllowlist(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")
	content := `
source_security:
  allow_internal_hosts:
    - git.corp.internal
    - 10.0.0.0/8
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	al, err := cfg.SourceSecurity.Allowlist()
	if err != nil {
		t.Fatalf("Allowlist() = %v, want nil", err)
	}
	if !al.AllowsHost("git.corp.internal") {
		t.Error("AllowsHost(git.corp.internal) = false, want true")
	}
	if al.AllowsIP(nil) {
		t.Error("AllowsIP(nil) = true, want false")
	}
}

// TestLoad_SourceSecurityRejectsBadEntry proves a malformed entry fails at
// load time rather than at first clone.
func TestLoad_SourceSecurityRejectsBadEntry(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")
	content := `
source_security:
  allow_internal_hosts:
    - 10.0.0.0/999
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("Load accepted invalid CIDR; want error")
	}
	if !strings.Contains(err.Error(), "allow_internal_hosts") {
		t.Errorf("error = %v; want mention of allow_internal_hosts", err)
	}
}

// TestLoad_RejectsLegacyNotificationsBlock ensures the removed
// `notifications:` block is rejected at load time. yaml.v3 would otherwise
// drop it silently and operators would lose alerts without warning.
func TestLoad_RejectsLegacyNotificationsBlock(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := `
notifications:
  on_failure: true
  provider:
    type: ntfy
    url: https://ntfy.sh
    topic: test
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("Load accepted legacy notifications block; want error")
	}
	if !strings.Contains(err.Error(), "on_failure_chain") {
		t.Errorf("error = %v; want mention of on_failure_chain migration", err)
	}
}

// TestLoad_IgnoresLegacyAIBlock ensures a legacy top-level `ai:` key from an
// older dicode.yaml parses cleanly after AIConfig was removed. yaml.v3 silently
// drops unknown keys when unmarshalling into a typed struct, so this should
// not return an error.
func TestLoad_IgnoresLegacyAIBlock(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := `
ai:
  api_key_env: OPENAI_API_KEY
  base_url: ""
  model: gpt-4o
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(cfgPath); err != nil {
		t.Fatalf("legacy ai: block should be silently ignored, got %v", err)
	}
}

// TestLoad_AITaskDefault ensures an empty ai: block falls back to the
// buildin/dicodai default so zero-config installs keep the WebUI chat panel
// and `dicode ai` wired up without edits to dicode.yaml.
func TestLoad_AITaskDefault(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := `
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AI.Task != "buildin/dicodai" {
		t.Errorf("AI.Task default = %q, want %q", cfg.AI.Task, "buildin/dicodai")
	}
}

// TestLoad_AITaskOverride ensures a user-supplied ai.task survives the YAML
// round-trip without being clobbered by applyDefaults.
func TestLoad_AITaskOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := `
ai:
  task: examples/ai-agent-ollama
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AI.Task != "examples/ai-agent-ollama" {
		t.Errorf("AI.Task = %q, want %q", cfg.AI.Task, "examples/ai-agent-ollama")
	}
}

// TestLoad_DeviceBindingDefault ensures server.device_binding defaults to "off"
// when omitted, preserving the pre-#132 trusted-device behaviour.
func TestLoad_DeviceBindingDefault(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := `
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.DeviceBinding != "off" {
		t.Errorf("DeviceBinding default = %q, want %q", cfg.Server.DeviceBinding, "off")
	}
}

// TestLoad_DeviceBindingOverride checks that warn/strict survive the round-trip.
func TestLoad_DeviceBindingOverride(t *testing.T) {
	for _, mode := range []string{"warn", "strict", "off"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "dicode.yaml")
			content := fmt.Sprintf(`
server:
  device_binding: %s
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
`, mode)
			if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Server.DeviceBinding != mode {
				t.Errorf("DeviceBinding = %q, want %q", cfg.Server.DeviceBinding, mode)
			}
		})
	}
}

// TestLoad_DeviceBindingInvalid rejects unknown modes at Load() time.
func TestLoad_DeviceBindingInvalid(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")
	content := `
server:
  device_binding: paranoid
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(cfgPath); err == nil {
		t.Fatal("expected error for invalid device_binding, got nil")
	} else if !strings.Contains(err.Error(), "device_binding") {
		t.Errorf("error = %v, want mention of device_binding", err)
	}
}

// TestResolvedBrokerURL_Override exercises the explicit RelayConfig.BrokerURL
// path: when set it wins over any derivation from ServerURL.
func TestResolvedBrokerURL_Override(t *testing.T) {
	r := RelayConfig{
		ServerURL: "wss://relay.dicode.app",
		BrokerURL: "https://oauth.dicode.app",
	}
	if got := r.ResolvedBrokerURL(); got != "https://oauth.dicode.app" {
		t.Errorf("ResolvedBrokerURL() = %q, want override https://oauth.dicode.app", got)
	}
}

// TestResolvedBrokerURL_StripsTrailingSlash ensures callers can safely
// concatenate "/auth/..." onto the returned URL without producing a "//"
// double-slash, regardless of whether the operator put a trailing slash
// in broker_url: or the derivation path happens to introduce one.
func TestResolvedBrokerURL_StripsTrailingSlash(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    RelayConfig
		want string
	}{
		{"override with trailing slash", RelayConfig{BrokerURL: "https://broker.dicode.app/"}, "https://broker.dicode.app"},
		{"override with multiple trailing slashes", RelayConfig{BrokerURL: "https://broker.dicode.app///"}, "https://broker.dicode.app"},
		{"derived: wss with trailing slash", RelayConfig{ServerURL: "wss://relay.dicode.app/"}, "https://relay.dicode.app"},
		{"override without trailing slash unchanged", RelayConfig{BrokerURL: "https://broker.dicode.app"}, "https://broker.dicode.app"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.ResolvedBrokerURL(); got != tc.want {
				t.Errorf("ResolvedBrokerURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolvedBrokerURL_DerivesFromServerURL covers the default path:
// BrokerURL empty → swap ws[s] → http[s] on the ServerURL host.
func TestResolvedBrokerURL_DerivesFromServerURL(t *testing.T) {
	for _, tc := range []struct {
		name      string
		serverURL string
		want      string
	}{
		{"wss → https", "wss://relay.dicode.app", "https://relay.dicode.app"},
		{"ws → http", "ws://localhost:5553", "http://localhost:5553"},
		{"empty server_url → empty", "", ""},
		{"http scheme rejected at derivation", "http://oops", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := RelayConfig{ServerURL: tc.serverURL}
			if got := r.ResolvedBrokerURL(); got != tc.want {
				t.Errorf("ResolvedBrokerURL(%q) = %q, want %q", tc.serverURL, got, tc.want)
			}
		})
	}
}

// TestLoad_RelayBrokerURL_Roundtrip ensures the BrokerURL field parses from
// YAML and survives through Load() validation.
func TestLoad_RelayBrokerURL_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := `
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
relay:
  enabled: true
  server_url: wss://relay.example.com
  broker_url: https://broker.example.com
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Relay.BrokerURL != "https://broker.example.com" {
		t.Errorf("Relay.BrokerURL = %q, want https://broker.example.com", cfg.Relay.BrokerURL)
	}
	if got := cfg.Relay.ResolvedBrokerURL(); got != "https://broker.example.com" {
		t.Errorf("ResolvedBrokerURL() = %q, want the explicit broker_url", got)
	}
}

// TestLoad_RelayBrokerURL_RejectsMalformed covers the validator: anything
// that's not http:// or https://, or missing a host, fails at Load() time
// with a clear error.
func TestLoad_RelayBrokerURL_RejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		"broker.dicode.app",       // no scheme
		"ftp://broker.dicode.app", // non-http scheme
		"wss://broker.dicode.app", // WSS — user probably meant server_url
		"https://",                // missing host
		"http://",                 // missing host
	} {
		t.Run(bad, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "dicode.yaml")
			content := fmt.Sprintf(`
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
relay:
  enabled: true
  broker_url: %q
`, bad)
			if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(cfgPath); err == nil {
				t.Errorf("Load(broker_url=%q): expected error, got nil", bad)
			}
		})
	}
}

// TestLoad_RelayServerURL_RequiresWSS covers the mTLS invariant: when the
// relay is enabled, server_url must be wss://. A ws:// or any other scheme
// fails at Load() with a clear message rather than looping on connection
// errors at runtime.
func TestLoad_RelayServerURL_RequiresWSS(t *testing.T) {
	for _, badURL := range []string{
		"ws://127.0.0.1:5554/",   // plaintext ws
		"http://127.0.0.1:5554",  // wrong scheme entirely (not caught by a literal ws:// prefix check)
		"https://127.0.0.1:5554", // https, still not wss
	} {
		t.Run(badURL, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "dicode.yaml")
			content := fmt.Sprintf(`
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
relay:
  enabled: true
  server_url: %q
  broker_url: http://127.0.0.1:5553
`, badURL)
			if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(cfgPath)
			if err == nil {
				t.Fatalf("Load(server_url=%q): expected error, got nil", badURL)
			}
			if !strings.Contains(err.Error(), "wss://") {
				t.Errorf("error should mention wss://, got: %v", err)
			}
		})
	}
}

// TestLoad_RelayServerURL_DisabledRelayAllowsPlaintext ensures the wss:// guard
// is gated on relay.enabled: a disabled relay with a stale ws:// server_url
// (e.g. left over from local dev) must not block the whole daemon from booting.
func TestLoad_RelayServerURL_DisabledRelayAllowsPlaintext(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")
	content := `
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
relay:
  enabled: false
  server_url: ws://127.0.0.1:5554/
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(cfgPath); err != nil {
		t.Errorf("disabled relay with ws:// server_url should load, got: %v", err)
	}
}

// TestResolvedBrokerURL_DerivesFromFirstServerURL covers the HA-list path:
// with server_urls set, the OAuth broker origin derives from the FIRST entry
// (all instances of one deployment share one public origin).
func TestResolvedBrokerURL_DerivesFromFirstServerURL(t *testing.T) {
	r := RelayConfig{ServerURLs: []string{"wss://a.example:5554", "wss://b.example:5554"}}
	if got := r.ResolvedBrokerURL(); got != "https://a.example:5554" {
		t.Errorf("ResolvedBrokerURL() = %q, want derivation from the first entry", got)
	}
	// Explicit broker_url still wins over the derived origin.
	r.BrokerURL = "https://broker.example.com"
	if got := r.ResolvedBrokerURL(); got != "https://broker.example.com" {
		t.Errorf("ResolvedBrokerURL() = %q, want the explicit broker_url", got)
	}
}

// TestPrimaryServerURL covers the XOR collapse: ServerURL when the shorthand
// is used, else the first list entry, else "".
func TestPrimaryServerURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    RelayConfig
		want string
	}{
		{"shorthand", RelayConfig{ServerURL: "wss://a.example"}, "wss://a.example"},
		{"list first", RelayConfig{ServerURLs: []string{"wss://a.example", "wss://b.example"}}, "wss://a.example"},
		{"neither", RelayConfig{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.PrimaryServerURL(); got != tc.want {
				t.Errorf("PrimaryServerURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLoad_RelayServerURLs covers the server_urls list validator: both-set is
// rejected, an enabled relay needs at least one URL, every list entry must be
// wss://, and duplicate/empty entries are rejected at load time.
func TestLoad_RelayServerURLs(t *testing.T) {
	write := func(t *testing.T, relay string) (*Config, error) {
		t.Helper()
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "dicode.yaml")
		content := `
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
relay:
` + relay
		if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return Load(cfgPath)
	}

	t.Run("both server_url and server_urls rejected", func(t *testing.T) {
		_, err := write(t, `  enabled: true
  server_url: wss://a.example:5554
  server_urls:
    - wss://b.example:5554
  broker_url: https://a.example
`)
		if err == nil || !strings.Contains(err.Error(), "not both") {
			t.Fatalf("expected both-set rejection, got: %v", err)
		}
	})

	t.Run("enabled requires at least one URL", func(t *testing.T) {
		_, err := write(t, `  enabled: true
`)
		if err == nil || !strings.Contains(err.Error(), "no control-channel URL") {
			t.Fatalf("expected at-least-one requirement, got: %v", err)
		}
	})

	t.Run("non-wss list entry rejected", func(t *testing.T) {
		_, err := write(t, `  enabled: true
  server_urls:
    - wss://a.example:5554
    - ws://b.example:5554
  broker_url: https://a.example
`)
		if err == nil || !strings.Contains(err.Error(), "wss://") {
			t.Fatalf("expected non-wss rejection, got: %v", err)
		}
		if err != nil && !strings.Contains(err.Error(), "server_urls[1]") {
			t.Errorf("error should point at the offending index, got: %v", err)
		}
	})

	t.Run("duplicate list entry rejected", func(t *testing.T) {
		_, err := write(t, `  enabled: true
  server_urls:
    - wss://a.example:5554
    - wss://a.example:5554
  broker_url: https://a.example
`)
		if err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("expected duplicate rejection, got: %v", err)
		}
	})

	t.Run("valid list loads and derives broker from first", func(t *testing.T) {
		cfg, err := write(t, `  enabled: true
  server_urls:
    - wss://a.example:5554
    - wss://b.example:5554
  broker_url: https://a.example
`)
		if err != nil {
			t.Fatalf("valid server_urls should load, got: %v", err)
		}
		if len(cfg.Relay.ServerURLs) != 2 {
			t.Errorf("ServerURLs = %v, want 2 entries", cfg.Relay.ServerURLs)
		}
		if got := cfg.Relay.PrimaryServerURL(); got != "wss://a.example:5554" {
			t.Errorf("PrimaryServerURL() = %q, want the first entry", got)
		}
	})
}

func TestLoadExecutionMaxConcurrentTasks(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := `
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
execution:
  max_concurrent_tasks: 8
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Execution.MaxConcurrentTasks != 8 {
		t.Errorf("Execution.MaxConcurrentTasks = %d, want 8", cfg.Execution.MaxConcurrentTasks)
	}
}

// Regression: `watch: false` and `mcp: false` in YAML must survive
// applyDefaults. Using a bare `bool` with a default-flip
// (`if !x { x = true }`) makes explicit false a no-op; the pointer-based
// fields avoid that. With the new spec.entries shape, watch lives on
// spec.entries.<name>.ref.watch.
func TestLoadWatchAndMCPRespectExplicitFalse(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := `
server:
  port: 8080
  mcp: false
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
        watch: false
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.MCP == nil || *cfg.Server.MCP {
		t.Errorf("Server.MCP = %v, want explicit false", cfg.Server.MCP)
	}
	localEntry := cfg.Spec.Entries["local"]
	if localEntry == nil || localEntry.Ref == nil {
		t.Fatal("spec.entries[local] not found")
	}
	if localEntry.Ref.Watch == nil || *localEntry.Ref.Watch {
		t.Errorf("Spec.Entries[local].Ref.Watch = %v, want explicit false", localEntry.Ref.Watch)
	}
}

func TestLoadWatchAndMCPDefaultsToTrueWhenUnset(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := `
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.MCP == nil || !*cfg.Server.MCP {
		t.Errorf("Server.MCP = %v, want default true when unset", cfg.Server.MCP)
	}
	localEntry := cfg.Spec.Entries["local"]
	if localEntry == nil || localEntry.Ref == nil {
		t.Fatal("spec.entries[local] not found")
	}
	if localEntry.Ref.Watch == nil || !*localEntry.Ref.Watch {
		t.Errorf("Spec.Entries[local].Ref.Watch = %v, want default true when unset", localEntry.Ref.Watch)
	}
}

// ── server.bcrypt_cost (#209) ─────────────────────────────────────────────────

// Default cost (12) when omitted. The webui passphrase store reads this value
// directly, so an absent YAML entry must produce a usable default rather than
// silently falling back to bcrypt's package-default of 10.
func TestLoad_BcryptCost_DefaultIs12WhenUnset(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")
	content := `
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.BcryptCost != 12 {
		t.Errorf("Server.BcryptCost = %d, want default 12", cfg.Server.BcryptCost)
	}
}

func TestLoad_BcryptCost_AcceptsValidOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")
	content := `
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
server:
  bcrypt_cost: 10
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.BcryptCost != 10 {
		t.Errorf("Server.BcryptCost = %d, want 10 (operator override)", cfg.Server.BcryptCost)
	}
}

// Out-of-range values must be rejected at Load time so the operator finds out
// at startup rather than discovering at first login that bcrypt is rejecting
// the cost. We cap at 14 to prevent operators from accidentally locking themselves out.
func TestLoad_BcryptCost_RejectsOutOfRange(t *testing.T) {
	for _, bad := range []int{1, 2, 3, 15, 20, 31} {
		t.Run(fmt.Sprintf("cost=%d", bad), func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "dicode.yaml")
			content := fmt.Sprintf(`
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
server:
  bcrypt_cost: %d
`, bad)
			if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(cfgPath); err == nil {
				t.Errorf("Load(bcrypt_cost=%d): expected error, got nil", bad)
			}
		})
	}
}

func TestConfigLoad_RejectsAutonomousAtDefaults(t *testing.T) {
	yaml := `
defaults:
  on_failure_chain:
    task: auto-fix
    params:
      mode: autonomous
`
	tmp := t.TempDir()
	path := filepath.Join(tmp, "dicode.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load accepted autonomous-at-defaults; want error")
	}
	if !strings.Contains(err.Error(), "autonomous") {
		t.Errorf("error = %v; want mention of autonomous", err)
	}
}

func TestConfigLoad_RejectsReservedKeyCollision(t *testing.T) {
	yaml := `
defaults:
  on_failure_chain:
    task: auto-fix
    params:
      taskID: somehow
`
	tmp := t.TempDir()
	path := filepath.Join(tmp, "dicode.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load accepted reserved-key collision; want error")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("error = %v; want mention of reserved", err)
	}
}

// TestLoad_AICreateTaskDefault ensures an empty ai: block falls back to the
// buildin/task-create default so zero-config installs get the AI-first task
// authoring agent wired up without edits to dicode.yaml.
func TestLoad_AICreateTaskDefault(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := `
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AI.CreateTask != "buildin/task-create" {
		t.Errorf("AI.CreateTask default = %q, want %q", cfg.AI.CreateTask, "buildin/task-create")
	}
}

// TestLoad_AICreateTaskOverride ensures a user-supplied ai.create_task survives
// the YAML round-trip without being clobbered by applyDefaults — operators
// who point at a custom override (e.g. claude-cli wrapper) must keep their
// choice across daemon restarts.
func TestLoad_AICreateTaskOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := `
ai:
  create_task: examples/task-create-claude
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AI.CreateTask != "examples/task-create-claude" {
		t.Errorf("AI.CreateTask = %q, want %q", cfg.AI.CreateTask, "examples/task-create-claude")
	}
}

// TestLoad_AICreateSessionTTLDefault ensures an empty ai.create_session_ttl
// falls back to 24h so stale authoring sessions are auto-cancelled without
// requiring explicit operator config.
func TestLoad_AICreateSessionTTLDefault(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := `
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	want := 24 * time.Hour
	if cfg.AI.CreateSessionTTL != want {
		t.Errorf("AI.CreateSessionTTL default = %v, want %v", cfg.AI.CreateSessionTTL, want)
	}
}

// TestLoad_AIScratchSourceSynthesized ensures that when no source named
// "ai-scratch" is defined in dicode.yaml, applyDefaults synthesizes one
// pointing at ${DATADIR}/ai-tasks. This is what makes `dicode task create`
// zero-config: the operator runs it, the boilerplate writes to the
// synthesized source, and the reconciler picks it up. Without this, every
// install would need to hand-author an entry in dicode.yaml first.
//
// Deliberately does NOT pre-create the ai-tasks directory — applyDefaults
// must create it itself (#568: a fresh install with no directory used to
// silently skip synthesis, so a never-before-run `task create` 404'd with
// "source not found").
func TestLoad_AIScratchSourceSynthesized(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := fmt.Sprintf(`
data_dir: %s
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
`, dir)
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := cfg.Spec.Entries["ai-scratch"]
	if !ok || entry == nil {
		t.Fatalf("Spec.Entries[ai-scratch] missing; want synthesized entry")
	}
	if entry.Ref == nil {
		t.Fatalf("Spec.Entries[ai-scratch].Ref = nil; want non-nil local ref")
	}
	// Ref.Path must be the taskset.yaml FILE inside ai-tasks/, not the bare
	// directory: taskset.Source computes its fsnotify watch root (and thus
	// CreateTask's RepoPath()) as filepath.Dir(ref.Path) for local refs, which
	// assumes ref.Path is a file — a bare-directory ref.Path would silently
	// resolve the watch root to DataDir itself (one level up), so scaffolded
	// task files would land beside ai-tasks/ instead of inside it.
	wantPath := filepath.Join(dir, "ai-tasks", "taskset.yaml")
	if entry.Ref.Path != wantPath {
		t.Errorf("Spec.Entries[ai-scratch].Ref.Path = %q, want %q", entry.Ref.Path, wantPath)
	}
	if entry.Ref.URL != "" {
		t.Errorf("Spec.Entries[ai-scratch].Ref.URL = %q, want empty (local source)", entry.Ref.URL)
	}
	// Watch defaults to true for local sources so the agent's writes are
	// reflected in the registry via fsnotify, matching every other local
	// source under the same applyDefaults branch.
	if entry.Ref.Watch == nil || !*entry.Ref.Watch {
		t.Errorf("Spec.Entries[ai-scratch].Ref.Watch = %v, want explicit true", entry.Ref.Watch)
	}
	// The directory AND the minimal taskset.yaml inside it must now exist on
	// disk — this is the #568 fix: applyDefaults creates them rather than
	// merely stat-checking for the directory, so a fresh install (this test
	// never called MkdirAll) still gets a genuinely resolvable ai-scratch
	// source instead of a 404 (or a silently-misplaced-files bug) on first
	// `task create`.
	wantDir := filepath.Join(dir, "ai-tasks")
	if fi, err := os.Stat(wantDir); err != nil || !fi.IsDir() {
		t.Errorf("ai-tasks dir %q not created by applyDefaults: %v", wantDir, err)
	}
	tsBytes, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("ai-tasks/taskset.yaml not created by applyDefaults: %v", err)
	}
	if !strings.Contains(string(tsBytes), "kind: TaskSet") {
		t.Errorf("ai-tasks/taskset.yaml content = %q, want a kind: TaskSet document", tsBytes)
	}
}

// TestLoad_AIScratchSynthesizedOnFreshInstall is the regression test for
// #568: before the fix, applyDefaults only synthesized "ai-scratch" when
// ${DATADIR}/ai-tasks already existed on disk, which is never true on a
// genuinely fresh install (no prior `task create` ever run) — so synthesis
// silently never fired and CreateTask's default-source lookup 404'd with
// "source not found" for every new user's first attempt. This test asserts
// both halves of the fix: the directory gets created, and the source
// synthesizes, in the same Load call that used to do neither.
func TestLoad_AIScratchSynthesizedOnFreshInstall(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := fmt.Sprintf(`
data_dir: %s
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
`, dir)
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	aiTasksDir := filepath.Join(dir, "ai-tasks")
	if _, err := os.Stat(aiTasksDir); !os.IsNotExist(err) {
		t.Fatalf("precondition failed: %q must not exist before Load", aiTasksDir)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	if fi, err := os.Stat(aiTasksDir); err != nil || !fi.IsDir() {
		t.Fatalf("ai-tasks dir %q not created by Load/applyDefaults: %v", aiTasksDir, err)
	}
	wantTasksetPath := filepath.Join(aiTasksDir, "taskset.yaml")
	if _, err := os.Stat(wantTasksetPath); err != nil {
		t.Fatalf("ai-tasks/taskset.yaml not created by Load/applyDefaults: %v", err)
	}
	entry, ok := cfg.Spec.Entries["ai-scratch"]
	if !ok || entry == nil {
		t.Fatalf("Spec.Entries[ai-scratch] missing; want synthesized entry even on a fresh install with no pre-existing dir")
	}
	if entry.Ref == nil || entry.Ref.Path != wantTasksetPath {
		t.Errorf("Spec.Entries[ai-scratch].Ref = %v, want path %q", entry.Ref, wantTasksetPath)
	}
}

// TestLoad_AIScratchSynthesizedFromEmptySpec ensures the synthesis works
// even when dicode.yaml has no `spec:` block at all (the bare zero-config
// install). A refactor that drops the `cfg.Spec.Entries == nil` map-init
// guard would pass every other test in this package because they all
// pre-populate at least one entry — this is the only test that exercises
// the nil-map allocation path.
//
// No pre-existing ai-tasks dir here either (see #568 fix): applyDefaults
// creates it itself, so this test's original manual `os.MkdirAll` step is
// redundant and has been removed — confirmed by TestLoad_AIScratchSynthesizedOnFreshInstall
// above, which covers the mkdir behavior directly.
func TestLoad_AIScratchSynthesizedFromEmptySpec(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := fmt.Sprintf(`
data_dir: %s
`, dir)
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Spec.Entries == nil {
		t.Fatal("Spec.Entries = nil after Load; want allocated map containing ai-scratch")
	}
	entry, ok := cfg.Spec.Entries["ai-scratch"]
	if !ok || entry == nil {
		t.Fatalf("Spec.Entries[ai-scratch] missing; want synthesized entry on empty spec")
	}
	wantPath := filepath.Join(dir, "ai-tasks", "taskset.yaml")
	if entry.Ref == nil || entry.Ref.Path != wantPath {
		t.Errorf("Spec.Entries[ai-scratch].Ref.Path = %v, want %q", entry.Ref, wantPath)
	}
}

// TestLoad_AIScratchUserDefinedWins ensures a user who explicitly defines
// an `ai-scratch` source in dicode.yaml is not overwritten by the synthesis.
// The synthesis is a default — like every other applyDefaults branch, a
// user-supplied value takes precedence.
func TestLoad_AIScratchUserDefinedWins(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")
	userPath := filepath.Join(dir, "my-scratch")

	content := fmt.Sprintf(`
data_dir: %s
spec:
  entries:
    ai-scratch:
      ref:
        path: %s
`, dir, userPath)
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := cfg.Spec.Entries["ai-scratch"]
	if !ok || entry == nil {
		t.Fatalf("Spec.Entries[ai-scratch] missing")
	}
	if entry.Ref.Path != userPath {
		t.Errorf("Spec.Entries[ai-scratch].Ref.Path = %q, want user-supplied %q (synthesis must not overwrite)", entry.Ref.Path, userPath)
	}
}

// TestLoad_AICreateSessionTTLOverride ensures a user-supplied duration string
// parses into time.Duration so operators can tune the auto-cancel window
// (short TTLs for shared dev boxes, long for personal installs).
func TestLoad_AICreateSessionTTLOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := `
ai:
  create_session_ttl: 1h
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AI.CreateSessionTTL != time.Hour {
		t.Errorf("AI.CreateSessionTTL = %v, want %v", cfg.AI.CreateSessionTTL, time.Hour)
	}
}

// TestLoad_ContainerSecurity covers the issue #380 operator opt-out block:
// default deny when absent, parsed values when present, and validation of
// allowed_volume_roots / allowed_cap_add entries.
func TestLoad_ContainerSecurity(t *testing.T) {
	writeCfg := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "dicode.yaml")
		content := `
spec:
  entries:
    local:
      ref:
        path: ${CONFIGDIR}/tasks
` + body
		if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return cfgPath
	}

	t.Run("absent block is default deny", func(t *testing.T) {
		cfg, err := Load(writeCfg(t, ""))
		if err != nil {
			t.Fatal(err)
		}
		p := cfg.ContainerSecurity.Policy()
		if p.AllowHostNetwork || p.AllowInsecureSecurityOpt || len(p.AllowedCapAdd) != 0 || len(p.AllowedVolumeRoots) != 0 {
			t.Errorf("absent container_security must yield the strict zero policy, got %+v", p)
		}
	})

	t.Run("explicit opt-ins parse into the policy", func(t *testing.T) {
		cfg, err := Load(writeCfg(t, `
container_security:
  allow_host_network: true
  allow_insecure_security_opt: true
  allowed_cap_add: [SYS_PTRACE, CAP_NET_ADMIN]
  allowed_volume_roots: [/srv/dicode-data, /opt/shared]
`))
		if err != nil {
			t.Fatal(err)
		}
		p := cfg.ContainerSecurity.Policy()
		if !p.AllowHostNetwork || !p.AllowInsecureSecurityOpt {
			t.Errorf("booleans not propagated: %+v", p)
		}
		if len(p.AllowedCapAdd) != 2 || p.AllowedCapAdd[0] != "SYS_PTRACE" {
			t.Errorf("AllowedCapAdd = %v", p.AllowedCapAdd)
		}
		if len(p.AllowedVolumeRoots) != 2 || p.AllowedVolumeRoots[0] != "/srv/dicode-data" {
			t.Errorf("AllowedVolumeRoots = %v", p.AllowedVolumeRoots)
		}
	})

	t.Run("relative allowed_volume_roots rejected", func(t *testing.T) {
		_, err := Load(writeCfg(t, `
container_security:
  allowed_volume_roots: [relative/path]
`))
		if err == nil || !strings.Contains(err.Error(), "allowed_volume_roots") {
			t.Errorf("expected allowed_volume_roots validation error, got %v", err)
		}
	})

	t.Run("empty allowed_cap_add entry rejected", func(t *testing.T) {
		_, err := Load(writeCfg(t, `
container_security:
  allowed_cap_add: ["", SYS_PTRACE]
`))
		if err == nil || !strings.Contains(err.Error(), "allowed_cap_add") {
			t.Errorf("expected allowed_cap_add validation error, got %v", err)
		}
	})
}
