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
	"strings"
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
// Path fields use ${HOME} and ${DATADIR} template variables for portability.
func DefaultLocalConfig(tasksDir, dataDir string) string {
	// Convert absolute paths to template variables where possible.
	home, _ := os.UserHomeDir()
	if home != "" {
		tasksDir = strings.Replace(tasksDir, home, "${HOME}", 1)
		dataDir = strings.Replace(dataDir, home, "${HOME}", 1)
	}

	return `# dicode configuration
# Generated on first run. Edit this file to change settings.
# Restart dicode after making changes.
#
# Path variables — use these in any path field instead of absolute paths:
#   ${HOME}      — user home directory
#   ${CONFIGDIR} — directory containing this dicode.yaml file
#   ${DATADIR}   — resolved data_dir (default: ~/.dicode)
#   ~/           — also expanded to home directory

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
  path: ${DATADIR}/data.db

  # Switch to Postgres/MySQL for multi-machine or high-availability setups:
  # type: postgres
  # url_env: DATABASE_URL    # env var holding the DSN

# ---------------------------------------------------------------------------
# Secrets — encrypted storage for API keys and tokens used by tasks.
# Tasks reference secrets by name in task.yaml under env:
# ---------------------------------------------------------------------------
secrets:
  providers:
    - type: local   # encrypted SQLite, master key at ${DATADIR}/master.key
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
# AI — the WebUI task-detail chat panel and the 'dicode ai' CLI both forward
# to a single task named here. Provider credentials, model, and skills live
# with the task itself (see tasks/buildin/dicodai and tasks/examples/
# ai-agent-* for presets).
#
# Defaults to buildin/dicodai when omitted — a preset of buildin/ai-agent
# preloaded with the dicode-task-dev skill and wired to OpenAI. Set
# OPENAI_API_KEY in your environment and it works out of the box.
#
# Point at a different ai-agent preset to swap providers without code changes:
# ---------------------------------------------------------------------------
# ai:
#   task: buildin/dicodai                  # default
#   # task: examples/ai-agent-ollama      # local Ollama, no API key
#   # task: examples/ai-agent-groq        # free tier, set GROQ_API_KEY

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
