// Package onboarding handles the first-run experience when no dicode.yaml
// exists. It serves a wizard page in the browser that lets the user choose
// between local-only mode and git-backed mode, then generates a dicode.yaml.
//
// Flow:
//  1. main.go calls onboarding.Required(configPath) — returns true if no config
//  2. If required, start a temporary HTTP server on :8080 and open browser
//  3. User completes the wizard (local or git)
//  4. Wizard handler writes dicode.yaml and signals completion
//  5. main.go restarts with the new config
package onboarding

import (
	"os"
	"path/filepath"
)

// Required returns true if no config file exists at path and onboarding
// should be run before starting the main application.
func Required(configPath string) bool {
	_, err := os.Stat(configPath)
	return os.IsNotExist(err)
}

// Result holds the user's choices from the onboarding wizard.
type Result struct {
	Mode    Mode   // LocalOnly or Git
	GitURL  string // only set when Mode == Git
	GitAuth struct {
		Type     string // "token" | "ssh"
		TokenEnv string
	}
	TasksDir string // local tasks directory (always set)
	DataDir  string // dicode data directory
}

// Mode is the storage mode chosen during onboarding.
type Mode string

const (
	ModeLocalOnly Mode = "local"
	ModeGit       Mode = "git"
)

// DefaultLocalConfig returns a fully-commented dicode.yaml for local-only mode.
// All optional sections are included as comments so users can enable them easily.
func DefaultLocalConfig(tasksDir, dataDir string) string {
	return `# dicode configuration
# Generated on first run. Edit this file to change settings.
# Restart dicode after making changes.

# ---------------------------------------------------------------------------
# Task sources — where dicode looks for task folders.
# JS tasks: folder must contain task.yaml + task.js
# Docker tasks: folder must contain task.yaml (no task.js needed)
# ---------------------------------------------------------------------------
sources:
  - type: local
    path: ` + tasksDir + `
    watch: true       # reload tasks automatically when files change (~150ms)

  # Add a git source to version your tasks in GitHub/GitLab:
  # - type: git
  #   url: https://github.com/you/my-tasks
  #   branch: main
  #   poll_interval: 30s
  #   auth:
  #     type: token
  #     token_env: GITHUB_TOKEN

# ---------------------------------------------------------------------------
# Database — stores run history and task KV data.
# ---------------------------------------------------------------------------
database:
  type: sqlite
  path: ` + dataDir + `/data.db

  # Switch to Postgres/MySQL for multi-machine or high-availability setups:
  # type: postgres
  # url_env: DATABASE_URL    # env var holding the DSN

# ---------------------------------------------------------------------------
# Secrets — encrypted storage for API keys and tokens used by tasks.
# Tasks reference secrets by name in task.yaml under env:
# ---------------------------------------------------------------------------
secrets:
  providers:
    - type: local   # encrypted SQLite, master key at ` + dataDir + `/master.key
    - type: env     # fall back to host environment variables

# ---------------------------------------------------------------------------
# Web UI & API server
# ---------------------------------------------------------------------------
server:
  port: 8080
  mcp: true    # expose MCP endpoint at /mcp (for AI agent / Claude Code integration)
  tray: true   # set to false to disable the system tray icon (e.g. on headless servers)
  # secret: ""  # uncomment and set to require a password for the web UI

# ---------------------------------------------------------------------------
# AI task generation — powers the AI chat in the task editor.
# Pick one provider and uncomment the matching block.
# ---------------------------------------------------------------------------
ai:
  # OpenAI (default) — set OPENAI_API_KEY in your environment
  model: gpt-4o
  api_key_env: OPENAI_API_KEY

  # Claude (Anthropic) — uncomment and set ANTHROPIC_API_KEY
  # model: claude-sonnet-4-6
  # api_key_env: ANTHROPIC_API_KEY
  # base_url: https://api.anthropic.com/v1

  # Ollama (local, no key needed)
  # model: qwen2.5-coder:7b
  # base_url: http://localhost:11434/v1

# ---------------------------------------------------------------------------
# Push notifications (optional) — sends alerts to your phone on task failure.
# ---------------------------------------------------------------------------
# notifications:
#   on_failure: true
#   on_success: false
#   provider:
#     type: ntfy              # ntfy.sh is free and self-hostable
#     url: https://ntfy.sh
#     topic: my-dicode-alerts
#     # token_env: NTFY_TOKEN  # only needed for private topics

# ---------------------------------------------------------------------------
# Docker executor (optional)
# ---------------------------------------------------------------------------
# Tasks can use runtime: docker to run containers instead of JS scripts.
# No extra config needed — dicode uses the Docker socket from the environment
# (DOCKER_HOST or the default unix:///var/run/docker.sock).
#
# Example docker task (task.yaml in your tasks dir):
#
#   name: Nginx Dev Server
#   runtime: docker
#   trigger:
#     manual: true
#   docker:
#     image: nginx:alpine
#     pull_policy: missing   # always | missing | never
#     ports:
#       - "8888:80"
#     volumes:
#       - "/tmp:/usr/share/nginx/html:ro"
#
# Docker tasks stream logs live and can be killed from the run detail page.

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------
log_level: info   # debug | info | warn | error
data_dir: ` + dataDir + `
`
}

// WriteConfig writes the generated config to configPath, creating parent
// directories as needed.
func WriteConfig(configPath, content string) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(configPath, []byte(content), 0644)
}
