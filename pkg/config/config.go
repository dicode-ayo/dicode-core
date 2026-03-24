package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Sources       []SourceConfig      `yaml:"sources"`
	Database      DatabaseConfig      `yaml:"database"`
	Secrets       SecretsConfig       `yaml:"secrets"`
	Notifications NotificationsConfig `yaml:"notifications"`
	Relay         RelayConfig         `yaml:"relay"`
	Server        ServerConfig        `yaml:"server"`
	AI            AIConfig            `yaml:"ai"`
	LogLevel      string              `yaml:"log_level"`
	DataDir       string              `yaml:"data_dir"`
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
	OnFailure bool                   `yaml:"on_failure"`
	OnSuccess bool                   `yaml:"on_success"`
	Provider  *NotifyProviderConfig  `yaml:"provider,omitempty"`
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
// local directory watched via fsnotify.
type SourceConfig struct {
	Type SourceType `yaml:"type"` // "git" | "local"

	// Git source fields
	URL          string        `yaml:"url,omitempty"`
	Branch       string        `yaml:"branch,omitempty"`
	PollInterval time.Duration `yaml:"poll_interval,omitempty"`
	Auth         SourceAuth    `yaml:"auth,omitempty"`

	// Local source fields
	Path  string `yaml:"path,omitempty"`  // local directory to watch
	Watch bool   `yaml:"watch,omitempty"` // enable fsnotify (default true for local)

	// Shared / future
	// Tags filters which tasks are loaded from this source (north star).
	// Empty = load all tasks.
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
	Secret string `yaml:"secret"` // optional basic-auth password
	MCP    bool   `yaml:"mcp"`    // expose MCP endpoint at /mcp (default: true)
	Tray   *bool  `yaml:"tray"`   // system tray icon (nil = auto-detect)
}

type AIConfig struct {
	// BaseURL is the OpenAI-compatible API endpoint.
	// Leave empty for OpenAI (https://api.openai.com/v1).
	// Use "https://api.anthropic.com/v1" for Claude.
	// Use "http://localhost:11434/v1" for Ollama.
	BaseURL   string `yaml:"base_url"`
	// Model name as accepted by the chosen endpoint.
	Model     string `yaml:"model"`
	// APIKeyEnv is the env var that holds the API key.
	// Leave empty for Ollama (no key needed).
	APIKeyEnv string `yaml:"api_key_env"`
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
	// Notifications default to alerting on failure only
	// (OnFailure zero value is false, so only set if provider is configured)

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
