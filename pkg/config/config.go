package config

import (
	"fmt"
	"os"
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

type Config struct {
	Sources       []SourceConfig           `yaml:"sources"`
	Database      DatabaseConfig           `yaml:"database"`
	Secrets       SecretsConfig            `yaml:"secrets"`
	Notifications NotificationsConfig      `yaml:"notifications"`
	Relay         RelayConfig              `yaml:"relay"`
	Server        ServerConfig             `yaml:"server"`
	AI            AIConfig                 `yaml:"ai"`
	Runtimes      map[string]RuntimeConfig `yaml:"runtimes,omitempty"`
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

// RelayConfig controls the dicode.app WebSocket tunnel for public webhook URLs.
type RelayConfig struct {
	Enabled    bool   `yaml:"enabled"`     // default: true if AccountEnv is set
	AccountEnv string `yaml:"account_env"` // env var holding dicode.app token
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
	Path  string `yaml:"path,omitempty"`  // absolute path to taskset.yaml (local) or tasks dir (legacy)
	Watch bool   `yaml:"watch,omitempty"` // enable fsnotify (default true for local)

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
	Port   int    `yaml:"port"`
	Secret string `yaml:"secret" json:"-"` // optional basic-auth password; excluded from JSON API
	MCP    bool   `yaml:"mcp"`             // expose MCP endpoint at /mcp (default: true)
	Tray   *bool  `yaml:"tray"`            // system tray icon (nil = auto-detect)
}

type AIConfig struct {
	// BaseURL is the OpenAI-compatible API endpoint.
	// Leave empty for OpenAI (https://api.openai.com/v1).
	// Use "https://api.anthropic.com/v1" for Claude.
	// Use "http://localhost:11434/v1" for Ollama.
	BaseURL string `yaml:"base_url"`
	// Model name as accepted by the chosen endpoint.
	Model string `yaml:"model"`
	// APIKeyEnv is the env var that holds the API key.
	// Leave empty for Ollama (no key needed).
	APIKeyEnv string `yaml:"api_key_env"`
	// APIKey is a direct API key value. If set it takes precedence over APIKeyEnv.
	// Stored in dicode.yaml — only use for local/trusted setups.
	// json:"-" prevents it from appearing in /api/config HTTP responses.
	APIKey string `yaml:"api_key,omitempty" json:"-"`
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

	applyDefaults(&cfg)

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

func applyDefaults(cfg *Config) {
	// Expand ~ in all path fields before anything else.
	cfg.DataDir = expandHome(cfg.DataDir)
	cfg.Database.Path = expandHome(cfg.Database.Path)
	for i := range cfg.Sources {
		cfg.Sources[i].Path = expandHome(cfg.Sources[i].Path)
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
			// Watch defaults to true for local sources
			if !s.Watch {
				s.Watch = true
			}
		}
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	// MCP defaults to enabled
	if !cfg.Server.MCP {
		cfg.Server.MCP = true
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
	// Enable relay if account token env is set
	if cfg.Relay.AccountEnv == "" {
		cfg.Relay.AccountEnv = "DICODE_TOKEN"
	}
	// AI defaults — OpenAI-compatible, works with OpenAI / Claude / Ollama.
	if cfg.AI.Model == "" {
		cfg.AI.Model = "gpt-4o"
	}
	if cfg.AI.APIKeyEnv == "" {
		cfg.AI.APIKeyEnv = "OPENAI_API_KEY"
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.DataDir == "" {
		home, _ := os.UserHomeDir()
		cfg.DataDir = home + "/.dicode"
	}
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
	return nil
}
