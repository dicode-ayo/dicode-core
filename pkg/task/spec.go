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
	RuntimeJS Runtime = "js"
)

// ChainTrigger fires a task when another task completes.
type ChainTrigger struct {
	From string `yaml:"from"`           // task ID to listen for
	On   string `yaml:"on,omitempty"`   // "success" (default) | "failure" | "always"
}

// TriggerConfig defines how a task is triggered.
// Exactly one of Cron, Webhook, Manual, or Chain should be set.
type TriggerConfig struct {
	Cron    string        `yaml:"cron,omitempty"`    // cron expression e.g. "0 9 * * *"
	Webhook string        `yaml:"webhook,omitempty"` // HTTP path e.g. "/hooks/my-task"
	Manual  bool          `yaml:"manual,omitempty"`  // only via explicit trigger
	Chain   *ChainTrigger `yaml:"chain,omitempty"`   // fire when another task completes
}

// Param defines a user-configurable input for a task.
type Param struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"`        // "string" | "number" | "boolean" | "cron"
	Default     string `yaml:"default"`
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
}

// Spec is parsed from task.yaml.
type Spec struct {
	Name        string        `yaml:"name"`
	Description string        `yaml:"description"`
	Version     string        `yaml:"version"`
	Author      string        `yaml:"author,omitempty"`
	Runtime     Runtime       `yaml:"runtime"`
	Trigger     TriggerConfig `yaml:"trigger"`
	Params      []Param       `yaml:"params,omitempty"`
	Env         []string      `yaml:"env,omitempty"` // env var names required at runtime
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

	if spec.Timeout == 0 {
		spec.Timeout = 60 * time.Second
	}
	if spec.Runtime == "" {
		spec.Runtime = RuntimeJS
	}

	return &spec, nil
}

// ScriptPath returns the path to the task script file.
func (s *Spec) ScriptPath() string {
	switch s.Runtime {
	case RuntimeJS:
		return filepath.Join(s.TaskDir, "task.js")
	default:
		return filepath.Join(s.TaskDir, "task.js")
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
	case RuntimeJS, "":
		// ok
	default:
		return fmt.Errorf("unsupported runtime %q (supported: js)", s.Runtime)
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
