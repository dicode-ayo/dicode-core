package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
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
sources:
  - type: local
    path: ${HOME}/my-tasks
  - type: local
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

	if cfg.Sources[0].Path != home+"/my-tasks" {
		t.Errorf("sources[0].path = %q, want %q", cfg.Sources[0].Path, home+"/my-tasks")
	}
	if cfg.Sources[1].Path != dir+"/tasks" {
		t.Errorf("sources[1].path = %q, want %q", cfg.Sources[1].Path, dir+"/tasks")
	}
	wantDB := home + "/.dicode/test.db"
	if cfg.Database.Path != wantDB {
		t.Errorf("database.path = %q, want %q", cfg.Database.Path, wantDB)
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
sources:
  - type: local
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
sources:
  - type: local
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
sources:
  - type: local
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
sources:
  - type: local
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
sources:
  - type: local
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

func TestLoadExecutionMaxConcurrentTasks(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := `
sources:
  - type: local
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

// Regression for #177: `watch: false` and `mcp: false` in YAML must survive
// applyDefaults. Previously both fields were `bool` with a default-flip
// (`if !x { x = true }`) that made explicit false a no-op.
func TestLoadWatchAndMCPRespectExplicitFalse(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := `
server:
  port: 8080
  mcp: false
sources:
  - type: local
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
	if len(cfg.Sources) != 1 {
		t.Fatalf("want 1 source, got %d", len(cfg.Sources))
	}
	if cfg.Sources[0].Watch == nil || *cfg.Sources[0].Watch {
		t.Errorf("Sources[0].Watch = %v, want explicit false", cfg.Sources[0].Watch)
	}
}

func TestLoadWatchAndMCPDefaultsToTrueWhenUnset(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	content := `
sources:
  - type: local
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
	if len(cfg.Sources) != 1 {
		t.Fatalf("want 1 source, got %d", len(cfg.Sources))
	}
	if cfg.Sources[0].Watch == nil || !*cfg.Sources[0].Watch {
		t.Errorf("Sources[0].Watch = %v, want default true when unset", cfg.Sources[0].Watch)
	}
}
