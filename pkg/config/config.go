package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"gopkg.in/yaml.v3"
)

// ErrLegacySourcesFormat is returned by Load when the config file contains the
// legacy `sources:` array. Operators must migrate to the new `spec.entries`
// shape — see https://github.com/dicode-ayo/dicode-core/issues/261 and
// docs/concepts/sources.md for a side-by-side migration guide.
var ErrLegacySourcesFormat = errors.New(
	"dicode.yaml: legacy `sources` array detected. The format changed: declare\n" +
		"sources as `spec.entries` instead. See\n" +
		"https://github.com/dicode-ayo/dicode-core/issues/261\n" +
		"or docs/concepts/sources.md for the migration. The translation is\n" +
		"mechanical — each `sources[]` entry becomes a `spec.entries.<name>`\n" +
		"with the source's fields nested under `ref`.",
)

// RuntimeConfig configures a managed runtime executor.
// The Version field pins the interpreter version that dicode downloads;
// leave it empty to use the default version bundled with this release.
// Set Disabled: true to prevent the runtime from being registered at startup.
type RuntimeConfig struct {
	// Version pins the interpreter version (e.g. "2.3.3" for Deno,
	// "0.7.3" for uv/Python). If empty the runtime's built-in default is used.
	Version string `yaml:"version,omitempty"`
	// Disabled prevents this runtime from being registered at startup.
	Disabled bool `yaml:"disabled,omitempty"`
}

// DefaultsConfig holds task-level defaults that apply globally unless overridden per-task.
type DefaultsConfig struct {
	// OnFailureChain is the chain target to fire whenever any task fails.
	// Accepts a bare task ID or a structured `{task, params}` form.
	// Per-task on_failure_chain field can override or disable this.
	OnFailureChain task.OnFailureChainSpec `yaml:"on_failure_chain,omitempty"`

	// RunInputs configures run-input persistence. See spec § 4.1.
	RunInputs RunInputsConfig `yaml:"run_inputs,omitempty"`
}

// RunInputsConfig is the global default for run-input persistence (#233).
type RunInputsConfig struct {
	// Enabled toggles persistence globally. Default (nil → true) applied in
	// applyDefaults. Set to false to disable for the entire daemon.
	Enabled *bool `yaml:"enabled,omitempty"`

	// Retention is the maximum age of a persisted input. Older blobs are
	// swept by the run-inputs-cleanup buildin. Default 30d applied in
	// applyDefaults.
	Retention time.Duration `yaml:"retention,omitempty"`

	// StorageTask is the task ID of the configured storage backend.
	// Default "buildin/local-storage" applied in applyDefaults.
	StorageTask string `yaml:"storage_task,omitempty"`

	// BodyFullTextual opts in to persisting non-JSON textual bodies
	// verbatim (XML, plain-text). Footgun: name-based redaction can't
	// reach unstructured content; persisted bodies may contain credentials.
	// Default false.
	BodyFullTextual bool `yaml:"body_full_textual,omitempty"`
}

// IsEnabled returns the effective Enabled value (nil → true).
func (c RunInputsConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// ExecutionConfig tunes how task runs are dispatched by the trigger engine.
type ExecutionConfig struct {
	// MaxConcurrentTasks caps how many task goroutines run in parallel.
	// 0 (default) = unlimited. Extra invocations queue inside the daemon
	// and run as slots free. Daemon-trigger tasks bypass the limit so
	// long-runners can't starve webhook/cron tasks.
	// The DICODE_MAX_CONCURRENT_TASKS env var overrides this value when set.
	MaxConcurrentTasks int `yaml:"max_concurrent_tasks,omitempty"`
}

// RelayConfig configures the WebSocket relay client.
// The relay allows a local dicode instance to receive webhooks from external
// services (GitHub, Slack, etc.) without port forwarding.
type RelayConfig struct {
	Enabled   bool   `yaml:"enabled"`
	ServerURL string `yaml:"server_url"` // wss://relay.dicode.app
	// BrokerURL overrides the OAuth broker base URL. When empty, the daemon
	// derives it from ServerURL by swapping the scheme (wss://host →
	// https://host). Set this when the broker runs on a different host than
	// the WSS relay endpoint, or during local development to point at a
	// broker on a non-TLS port (e.g. http://localhost:5553). Must be http://
	// or https:// when set.
	BrokerURL string `yaml:"broker_url,omitempty"`
}

// ResolvedBrokerURL returns the HTTP(S) OAuth broker base URL that the
// daemon should use for signing /auth URLs and receiving ECIES-encrypted
// token deliveries. If BrokerURL is set it wins; otherwise the URL is
// derived from ServerURL. Returns empty when neither yields a usable URL —
// the daemon treats that as "OAuth broker disabled".
//
// The returned URL never has a trailing slash so callers can safely
// concatenate `+ "/auth/" + provider` without producing a "//" double-
// slash. Operators writing broker_url: https://host/ in dicode.yaml get
// the slash stripped here.
func (r RelayConfig) ResolvedBrokerURL() string {
	var raw string
	switch {
	case r.BrokerURL != "":
		raw = r.BrokerURL
	case strings.HasPrefix(r.ServerURL, "wss://"):
		raw = "https://" + strings.TrimPrefix(r.ServerURL, "wss://")
	case strings.HasPrefix(r.ServerURL, "ws://"):
		raw = "http://" + strings.TrimPrefix(r.ServerURL, "ws://")
	default:
		return ""
	}
	return strings.TrimRight(raw, "/")
}

// AIConfig points the WebUI and CLI at a single task for AI operations.
// The task must have a webhook trigger — /api/ai/chat forwards requests to it,
// and `dicode ai` fires it through the engine.
type AIConfig struct {
	// Task is the task id invoked for AI operations in the WebUI and CLI.
	// Defaults to "buildin/dicodai" — a preset of buildin/ai-agent preloaded
	// with the dicode-task-dev skill. Point it at any ai-agent override to
	// swap providers, skills, or model without changing code.
	Task string `yaml:"task,omitempty"`
}

type Config struct {
	// Spec is the root TaskSet defined inline in dicode.yaml. Its entries
	// declare every source the daemon loads. Overrides on these entries
	// propagate down to the referenced TaskSet using the standard
	// parent.overrides.entries mechanism — see pkg/taskset/resolver.go.
	//
	// Example:
	//   spec:
	//     entries:
	//       buildin:
	//         ref:
	//           path: ${CONFIGDIR}/tasks/buildin/taskset.yaml
	//       examples:
	//         ref:
	//           url: https://github.com/org/examples
	//           branch: main
	//           poll_interval: 5m
	//           auth: { token_env: GITHUB_TOKEN }
	Spec          taskset.TaskSetBody      `yaml:"spec"`
	Database      DatabaseConfig           `yaml:"database"`
	Secrets       SecretsConfig            `yaml:"secrets"`
	Notifications NotificationsConfig      `yaml:"notifications"`
	Server        ServerConfig             `yaml:"server"`
	Defaults      DefaultsConfig           `yaml:"defaults"`
	Runtimes      map[string]RuntimeConfig `yaml:"runtimes,omitempty"`
	Execution     ExecutionConfig          `yaml:"execution,omitempty"`
	Relay         RelayConfig              `yaml:"relay,omitempty"`
	AI            AIConfig                 `yaml:"ai,omitempty"`
	LogLevel      string                   `yaml:"log_level"`
	DataDir       string                   `yaml:"data_dir"`
}

// DatabaseConfig selects the storage backend.
// SQLite is the default (free). Postgres/MySQL are for paid/enterprise use.
type DatabaseConfig struct {
	Type   string `yaml:"type"`    // "sqlite" (default) | "postgres" | "mysql"
	Path   string `yaml:"path"`    // sqlite: path to .db file
	URLEnv string `yaml:"url_env"` // postgres/mysql: env var holding DSN
}

type SecretsConfig struct {
	Providers []SecretProviderConfig `yaml:"providers"`
}

type SecretProviderConfig struct {
	Type     string `yaml:"type"`      // "local" | "env" | "vault" | ...
	Address  string `yaml:"address"`   // vault address
	TokenEnv string `yaml:"token_env"` // env var holding token
}

type NotificationsConfig struct {
	// OnFailure sends a notification when a task run fails. Defaults to true.
	OnFailure *bool `yaml:"on_failure,omitempty"`
	// OnSuccess sends a notification when a task run succeeds. Defaults to false.
	OnSuccess *bool                 `yaml:"on_success,omitempty"`
	Provider  *NotifyProviderConfig `yaml:"provider,omitempty"`
}

// NotifyOnFailure returns the effective on_failure value (defaults to true).
func (n *NotificationsConfig) NotifyOnFailure() bool {
	if n.OnFailure == nil {
		return true
	}
	return *n.OnFailure
}

// NotifyOnSuccess returns the effective on_success value (defaults to false).
func (n *NotificationsConfig) NotifyOnSuccess() bool {
	if n.OnSuccess == nil {
		return false
	}
	return *n.OnSuccess
}

type NotifyProviderConfig struct {
	Type     string `yaml:"type"`      // "ntfy" | "gotify" | "pushover" | "telegram"
	URL      string `yaml:"url"`       // provider base URL
	Topic    string `yaml:"topic"`     // ntfy topic / gotify app token / etc.
	TokenEnv string `yaml:"token_env"` // env var holding auth token
}

type ServerConfig struct {
	Port           int      `yaml:"port"`
	Secret         string   `yaml:"secret" json:"-"`           // optional passphrase; excluded from JSON API
	Auth           bool     `yaml:"auth"`                      // enable global auth wall (default: false)
	AllowedOrigins []string `yaml:"allowed_origins,omitempty"` // CORS allowlist; empty = same-origin only
	TrustProxy     bool     `yaml:"trust_proxy,omitempty"`     // trust X-Forwarded-For from upstream proxy
	MCP            *bool    `yaml:"mcp,omitempty"`             // expose MCP endpoint at /mcp; nil → default true, explicit false opts out
	TLSCertFile    string   `yaml:"tls_cert,omitempty"`        // path to TLS certificate (PEM); enables HTTPS when set with tls_key
	TLSKeyFile     string   `yaml:"tls_key,omitempty"`         // path to TLS private key (PEM)
	// BcryptCost is the work factor used when hashing the stored auth
	// passphrase. Valid range 4–14. 0 means "unset" → defaults to 12 in
	// applyDefaults. Higher = slower login but stronger against offline attacks
	// if the SQLite DB ever leaks; lower can be useful on very small devices
	// (e.g. Raspberry Pi Zero) where the default ~300ms login is too slow.
	// Values outside 4–14 are rejected by validate.
	BcryptCost int `yaml:"bcrypt_cost,omitempty"`
}

// Load reads and parses the config file at path, then applies defaults.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}

	// Probe for the old `sources:` array before decoding into Config.
	// Fail fast with a clear migration error instead of silently dropping sources.
	var probe struct {
		Sources any `yaml:"sources"`
	}
	if err := yaml.Unmarshal(data, &probe); err == nil && probe.Sources != nil {
		return nil, ErrLegacySourcesFormat
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	configDir, _ := filepath.Abs(filepath.Dir(path))
	applyDefaults(&cfg, configDir)

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

// expandHome replaces a leading ~/ with the actual home directory.
func expandHome(path string) string {
	if len(path) >= 2 && path[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err == nil {
			return home + path[1:]
		}
	}
	return path
}

// expandVars replaces ${VAR} placeholders in path strings.
// Supported variables:
//   - ${HOME}      — user home directory
//   - ${CONFIGDIR} — directory containing dicode.yaml
//   - ${DATADIR}   — resolved data_dir value
func expandVars(path string, vars map[string]string) string {
	for k, v := range vars {
		path = strings.ReplaceAll(path, "${"+k+"}", v)
	}
	return path
}

func applyDefaults(cfg *Config, configDir string) {
	// Build template variables for path expansion.
	home, _ := os.UserHomeDir()
	vars := map[string]string{
		"HOME":      home,
		"CONFIGDIR": configDir,
	}

	// Expand ~ and ${VAR} in all path fields before anything else.
	expand := func(path string) string {
		return expandVars(expandHome(path), vars)
	}
	cfg.DataDir = expand(cfg.DataDir)

	// DataDir must be resolved first so ${DATADIR} is available for other paths.
	if cfg.DataDir == "" {
		cfg.DataDir = home + "/.dicode"
	}
	vars["DATADIR"] = cfg.DataDir

	cfg.Database.Path = expand(cfg.Database.Path)

	// Expand ${VAR} in ref paths for spec.entries.
	for _, entry := range cfg.Spec.Entries {
		if entry == nil || entry.Ref == nil {
			continue
		}
		entry.Ref.Path = expand(entry.Ref.Path)
		// Apply defaults for git refs.
		if entry.Ref.IsGit() {
			if entry.Ref.Branch == "" {
				entry.Ref.Branch = "main"
			}
			if entry.Ref.PollInterval == 0 {
				entry.Ref.PollInterval = 30 * time.Second
			}
		}
		// Watch defaults to true for local refs.
		if !entry.Ref.IsGit() && entry.Ref.Watch == nil {
			t := true
			entry.Ref.Watch = &t
		}
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	// MCP defaults to enabled. Pointer lets us distinguish unset (nil → true)
	// from explicit false (opt-out).
	if cfg.Server.MCP == nil {
		t := true
		cfg.Server.MCP = &t
	}
	// BcryptCost defaults to 12 — ~300ms per hash on a 2024 server CPU.
	// Operators can override via server.bcrypt_cost in dicode.yaml; validate()
	// enforces the 4–14 range. We keep the unset → default mapping in
	// applyDefaults rather than at the call site so every consumer sees the
	// resolved value.
	if cfg.Server.BcryptCost == 0 {
		cfg.Server.BcryptCost = 12
	}
	// Default secret providers if none configured
	if len(cfg.Secrets.Providers) == 0 {
		cfg.Secrets.Providers = []SecretProviderConfig{
			{Type: "local"},
			{Type: "env"},
		}
	}
	// Default database to sqlite
	if cfg.Database.Type == "" {
		cfg.Database.Type = "sqlite"
	}
	if cfg.Database.Type == "sqlite" && cfg.Database.Path == "" {
		cfg.Database.Path = cfg.DataDir + "/data.db"
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	// AI.Task defaults to the buildin/dicodai preset so out-of-the-box
	// WebUI + CLI AI flows work without configuration. Empty-string and
	// absent both resolve to the default — there is no "explicitly
	// disabled" state today. If a future version needs one, switch to
	// *string and test for nil instead of empty.
	if cfg.AI.Task == "" {
		cfg.AI.Task = "buildin/dicodai"
	}
	// RunInputs defaults: 30-day retention, local-storage backend.
	if cfg.Defaults.RunInputs.Retention == 0 {
		cfg.Defaults.RunInputs.Retention = 30 * 24 * time.Hour
	}
	if cfg.Defaults.RunInputs.StorageTask == "" {
		cfg.Defaults.RunInputs.StorageTask = "buildin/local-storage"
	}
	// DataDir default is set earlier during variable expansion.
}

func (cfg *Config) validate() error {
	for name, entry := range cfg.Spec.Entries {
		if entry == nil {
			return fmt.Errorf("spec.entries[%q]: entry must not be null", name)
		}
		if entry.Ref == nil && entry.Inline == nil {
			return fmt.Errorf("spec.entries[%q]: either ref or inline must be set", name)
		}
		if entry.Ref != nil {
			if entry.Ref.IsGit() {
				// URL is present (checked by IsGit) — nothing more to validate here.
			} else if entry.Ref.Path == "" {
				return fmt.Errorf("spec.entries[%q]: ref.path is required for local entries", name)
			}
		}
	}
	// Lift the top-level `enabled` shortcut into overrides.enabled so that all
	// downstream code (resolver, override merging) sees one canonical path.
	// The same lift is applied by validateTaskSet for TaskSet files — both call
	// the shared helper to stay DRY.
	if err := taskset.LiftEntryEnabled(cfg.Spec.Entries); err != nil {
		return fmt.Errorf("spec.entries: %w", err)
	}
	if cfg.Relay.BrokerURL != "" {
		u, err := url.Parse(cfg.Relay.BrokerURL)
		if err != nil {
			return fmt.Errorf("relay.broker_url: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("relay.broker_url: must use http:// or https://, got scheme %q", u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("relay.broker_url: missing host in %q", cfg.Relay.BrokerURL)
		}
	}
	// bcrypt cost: x/crypto/bcrypt's MinCost = 4, MaxCost = 31, but anything
	// above ~14 is multi-second per login on commodity hardware and serves no
	// practical purpose for a single-user passphrase. Cap at 14 to prevent
	// operators from accidentally locking themselves out (or causing a
	// mid-attempt timeout) by setting 20 "to be safe".
	if cfg.Server.BcryptCost != 0 && (cfg.Server.BcryptCost < 4 || cfg.Server.BcryptCost > 14) {
		return fmt.Errorf("server.bcrypt_cost: must be between 4 and 14, got %d", cfg.Server.BcryptCost)
	}
	if err := cfg.Defaults.OnFailureChain.ValidateAtDefaults(); err != nil {
		return fmt.Errorf("defaults.on_failure_chain: %w", err)
	}
	return nil
}
