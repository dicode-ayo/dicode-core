package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// Regression for #177: `watch: false` and `mcp: false` in YAML must survive
// applyDefaults. Previously both fields were `bool` with a default-flip
// (`if !x { x = true }`) that made explicit false a no-op.
// With the new spec.entries shape, watch lives on spec.entries.<name>.ref.watch.
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
