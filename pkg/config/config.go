package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
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
	// OnFailureChain is the task ID to fire whenever any task fails.
	// Per-task on_failure_chain field can override or disable this.
	OnFailureChain string `yaml:"on_failure_chain,omitempty"`
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
	Sources       []SourceConfig           `yaml:"sources"`
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

// SourceType identifies the kind of task source.
type SourceType string

const (
	SourceTypeGit   SourceType = "git"
	SourceTypeLocal SourceType = "local"
)

// SourceConfig describes one task source — either a remote git repo or a
// local path pointing to a taskset.yaml (or kind: Task yaml).
type SourceConfig struct {
	Type SourceType `yaml:"type"` // "git" | "local"

	// Git source fields
	URL          string        `yaml:"url,omitempty"`
	Branch       string        `yaml:"branch,omitempty"`
	PollInterval time.Duration `yaml:"poll_interval,omitempty"`
	Auth         SourceAuth    `yaml:"auth,omitempty"`

	// Local source fields
	Path string `yaml:"path,omitempty"` // absolute path to taskset.yaml (local) or tasks dir (legacy)
	// Watch enables fsnotify on the local source. Nil means "unset — apply
	// default"; an explicit `watch: false` in YAML preserves false so the
	// user can opt out. Default (nil → true) is applied in applyDefaults.
	Watch *bool `yaml:"watch,omitempty"`

	// TaskSet fields (new model)
	// Name is the root namespace segment for all tasks from this source.
	// Defaults to the last segment of URL or Path.
	Name string `yaml:"name,omitempty"`
	// EntryPath is the path within the git repo (or absolute path for local)
	// to the entry yaml file. Defaults to "taskset.yaml".
	EntryPath string `yaml:"entry_path,omitempty"`
	// ConfigPath is the path to an optional kind:Config yaml file.
	// For git sources: path within the repo. For local: absolute path.
	// Defaults to "dicode-config.yaml" alongside the entry file.
	ConfigPath string `yaml:"config_path,omitempty"`

	// Shared / future
	Tags []string `yaml:"tags,omitempty"`
}

// SourceAuth holds credentials for a git source.
type SourceAuth struct {
	Type     string `yaml:"type"`      // "token" | "ssh"
	TokenEnv string `yaml:"token_env"` // env var name holding the token
	SSHKey   string `yaml:"ssh_key"`   // path to SSH private key file
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
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	var cfg Config
	if err := yaml.NewDecoder(f).Decode(&cfg); err != nil {
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
	for i := range cfg.Sources {
		cfg.Sources[i].Path = expand(cfg.Sources[i].Path)
	}

	for i := range cfg.Sources {
		s := &cfg.Sources[i]
		if s.Type == SourceTypeGit {
			if s.Branch == "" {
				s.Branch = "main"
			}
			if s.PollInterval == 0 {
				s.PollInterval = 30 * time.Second
			}
		}
		if s.Type == SourceTypeLocal {
			// Watch defaults to true for local sources. A pointer lets us
			// distinguish "unset" (nil → apply default true) from "explicitly
			// false" (user opted out) — the previous non-pointer form made
			// `watch: false` a no-op.
			if s.Watch == nil {
				t := true
				s.Watch = &t
			}
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
	// DataDir default is set earlier during variable expansion.
}

func (cfg *Config) validate() error {
	for i, s := range cfg.Sources {
		switch s.Type {
		case SourceTypeGit:
			if s.URL == "" {
				return fmt.Errorf("sources[%d]: url is required for git source", i)
			}
		case SourceTypeLocal:
			if s.Path == "" {
				return fmt.Errorf("sources[%d]: path is required for local source", i)
			}
		case "":
			return fmt.Errorf("sources[%d]: type is required (git or local)", i)
		default:
			return fmt.Errorf("sources[%d]: unknown type %q (valid: git, local)", i, s.Type)
		}
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
	return nil
}
