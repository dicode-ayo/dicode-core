package task

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Runtime identifies the scripting engine used to execute a task.
type Runtime string

const (
	RuntimeDeno   Runtime = "deno"
	RuntimeDocker Runtime = "docker"
)

// DockerConfig holds Docker-specific task configuration.
type DockerConfig struct {
	Image      string            `yaml:"image"`                 // e.g. "nginx:alpine"
	Command    []string          `yaml:"command,omitempty"`     // overrides image CMD
	Entrypoint []string          `yaml:"entrypoint,omitempty"`  // overrides image ENTRYPOINT
	Volumes    []string          `yaml:"volumes,omitempty"`     // "host:container[:ro]"
	Ports      []string          `yaml:"ports,omitempty"`       // "hostPort:containerPort[/proto]"
	WorkingDir string            `yaml:"working_dir,omitempty"` // container working dir
	EnvVars    map[string]string `yaml:"env_vars,omitempty"`    // extra env vars (literal)
	PullPolicy string            `yaml:"pull_policy,omitempty"` // "always" | "missing" (default) | "never"
}

// ChainTrigger fires a task when another task completes.
type ChainTrigger struct {
	From string `yaml:"from"`         // task ID to listen for
	On   string `yaml:"on,omitempty"` // "success" (default) | "failure" | "always"
}

// TriggerConfig defines how a task is triggered.
// Exactly one of Cron, Webhook, Manual, Chain, or Daemon should be set.
type TriggerConfig struct {
	Cron    string        `yaml:"cron,omitempty"`    // cron expression e.g. "0 9 * * *"
	Webhook string        `yaml:"webhook,omitempty"` // HTTP path e.g. "/hooks/my-task"
	Manual  bool          `yaml:"manual,omitempty"`  // only via explicit trigger
	Chain   *ChainTrigger `yaml:"chain,omitempty"`   // fire when another task completes
	Daemon  bool          `yaml:"daemon,omitempty"`  // start on app start, restart on exit
	Restart string        `yaml:"restart,omitempty"` // daemon only: "always"(default)|"on-failure"|"never"
}

// Param defines a user-configurable input for a task.
type Param struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"` // "string" | "number" | "boolean" | "cron"
	Default     string `yaml:"default"`
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
}

// FSEntry declares a path a task is allowed to access.
type FSEntry struct {
	Path       string `yaml:"path"`
	Permission string `yaml:"permission"` // "r" | "w" | "rw"
}

// Spec is parsed from task.yaml.
type Spec struct {
	Name        string        `yaml:"name"`
	Description string        `yaml:"description"`
	Version     string        `yaml:"version"`
	Author      string        `yaml:"author,omitempty"`
	Runtime     Runtime       `yaml:"runtime"`
	Docker      *DockerConfig `yaml:"docker,omitempty"` // required when runtime == "docker"
	Trigger     TriggerConfig `yaml:"trigger"`
	Params      []Param       `yaml:"params,omitempty"`
	Env         []string      `yaml:"env,omitempty"` // env var names required at runtime
	FS          []FSEntry     `yaml:"fs,omitempty"`  // filesystem access declarations
	Timeout     time.Duration `yaml:"timeout"`

	// TaskDir is the directory path of the task in the repo (not stored in YAML).
	TaskDir string `yaml:"-"`
	// ID is derived from the directory name (not stored in YAML).
	ID string `yaml:"-"`
}

// LoadDir reads a task from its directory (expects task.yaml and task.<ext>).
func LoadDir(dir string) (*Spec, error) {
	specPath := filepath.Join(dir, "task.yaml")
	f, err := os.Open(specPath)
	if err != nil {
		return nil, fmt.Errorf("open task.yaml in %s: %w", dir, err)
	}
	defer f.Close()

	var spec Spec
	if err := yaml.NewDecoder(f).Decode(&spec); err != nil {
		return nil, fmt.Errorf("parse task.yaml in %s: %w", dir, err)
	}

	if err := spec.validate(); err != nil {
		return nil, fmt.Errorf("invalid task in %s: %w", dir, err)
	}

	spec.TaskDir = dir
	spec.ID = filepath.Base(dir)

	if spec.Runtime == "" || spec.Runtime == "js" {
		spec.Runtime = RuntimeDeno
	}
	// Docker and daemon tasks may run indefinitely; don't impose a default timeout.
	if spec.Timeout == 0 && spec.Runtime != RuntimeDocker && !spec.Trigger.Daemon {
		spec.Timeout = 60 * time.Second
	}

	return &spec, nil
}

// ScriptPath returns the path to the task script file.
// Returns empty string for runtimes that don't use a script file (e.g. Docker).
// For the deno runtime, task.ts is preferred over task.js.
func (s *Spec) ScriptPath() string {
	switch s.Runtime {
	case RuntimeDeno:
		ts := filepath.Join(s.TaskDir, "task.ts")
		if _, err := os.Stat(ts); err == nil {
			return ts
		}
		return filepath.Join(s.TaskDir, "task.js")
	default:
		return ""
	}
}

// Script reads and returns the task script source.
func (s *Spec) Script() (string, error) {
	b, err := os.ReadFile(s.ScriptPath())
	if err != nil {
		return "", fmt.Errorf("read script %s: %w", s.ScriptPath(), err)
	}
	return string(b), nil
}

func (s *Spec) validate() error {
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	triggers := 0
	if s.Trigger.Cron != "" {
		triggers++
	}
	if s.Trigger.Webhook != "" {
		triggers++
	}
	if s.Trigger.Manual {
		triggers++
	}
	if s.Trigger.Daemon {
		triggers++
		switch s.Trigger.Restart {
		case "", "always", "on-failure", "never":
			// ok
		default:
			return fmt.Errorf("trigger.restart must be always, on-failure, or never")
		}
	}
	if s.Trigger.Chain != nil {
		triggers++
		if s.Trigger.Chain.From == "" {
			return fmt.Errorf("trigger.chain.from is required")
		}
		switch s.Trigger.Chain.On {
		case "", "success", "failure", "always":
			// ok
		default:
			return fmt.Errorf("trigger.chain.on must be success, failure, or always")
		}
	}
	if triggers == 0 {
		return fmt.Errorf("at least one trigger must be configured (cron, webhook, manual, or chain)")
	}
	if triggers > 1 {
		return fmt.Errorf("only one trigger type is allowed per task")
	}
	switch s.Runtime {
	case RuntimeDeno, "js", "":
		// ok — "js" is a legacy alias for "deno"
	case RuntimeDocker:
		if s.Docker == nil {
			return fmt.Errorf("runtime docker requires a docker: section in task.yaml")
		}
		if s.Docker.Image == "" {
			return fmt.Errorf("docker.image is required")
		}
		switch s.Docker.PullPolicy {
		case "", "missing", "always", "never":
			// ok
		default:
			return fmt.Errorf("docker.pull_policy must be always, missing, or never")
		}
	default:
		return fmt.Errorf("unsupported runtime %q (supported: deno, js, docker)", s.Runtime)
	}
	return nil
}

// ChainOn returns the normalized "on" condition for a chain trigger.
// Defaults to "success" if unset.
func (c *ChainTrigger) ChainOn() string {
	if c.On == "" {
		return "success"
	}
	return c.On
}
