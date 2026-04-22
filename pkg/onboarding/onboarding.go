// Package onboarding handles the first-run experience when no dicode.yaml
// exists. It picks one of three surfaces (silent / CLI / browser), gathers
// the user's choices, and writes a ready-to-load dicode.yaml with curated
// git-backed tasksets and an auto-generated dashboard passphrase.
package onboarding

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Required returns true if no config file exists at path and onboarding
// should be run before starting the main application.
func Required(configPath string) bool {
	_, err := os.Stat(configPath)
	return os.IsNotExist(err)
}

// RunOptions carries the environment dependencies Run needs so tests can
// drive it hermetically.
type RunOptions struct {
	IsTTY      bool
	HasDisplay bool
	In         io.Reader
	Out        io.Writer
	Home       string              // used to render default ~/dicode-tasks, ~/.dicode
	Env        func(string) string // typically os.Getenv
	Port       int                 // optional --port override; 0 = use default 8080
}

// Run is the single entry point called by the daemon when no config exists.
// It picks a wizard surface, gathers choices, writes the config, and prints
// the success banner.
func Run(ctx context.Context, configPath string, opts RunOptions) error {
	surface, err := PickSurface(opts.In, opts.Out, opts.IsTTY, opts.HasDisplay, opts.Env)
	if err != nil {
		return fmt.Errorf("pick surface: %w", err)
	}

	// The browser surface needs to persist the config atomically with
	// returning the passphrase to the client — otherwise a write failure
	// leaves the user with a credential we never stored. For CLI/silent
	// paths the same function is used after gathering the Result so both
	// code paths go through identical persistence.
	apply := func(r Result) error { return WriteConfig(configPath, RenderConfig(r)) }

	var res Result
	switch surface {
	case SurfaceSilent:
		res = defaultResult(opts.Home, opts.Port)
		if err := apply(res); err != nil {
			return fmt.Errorf("write config: %w", err)
		}
	case SurfaceCLI:
		res, err = RunCLI(opts.In, opts.Out, opts.Home, opts.Port)
		if err != nil {
			return fmt.Errorf("wizard: %w", err)
		}
		if err := apply(res); err != nil {
			return fmt.Errorf("write config: %w", err)
		}
	case SurfaceBrowser:
		// apply runs INSIDE /setup/apply before the passphrase is handed
		// back to the browser — no re-apply here.
		res, err = RunBrowser(ctx, opts.Home, opts.Port, apply)
		if err != nil {
			return fmt.Errorf("wizard: %w", err)
		}
	}

	PrintSuccess(opts.Out, res, configPath)
	return nil
}

// defaultResult produces the Result used for non-interactive / silent
// runs: all curated tasksets on, default paths under home, generated
// passphrase. port, when non-zero, overrides the default 8080 — so
// systemd/Docker installs started with --port honor the flag.
func defaultResult(home string, port int) Result {
	enabled := make(map[string]bool, len(TaskSetPresets))
	for _, p := range TaskSetPresets {
		enabled[p.Name] = p.DefaultOn
	}
	return Result{
		TaskSetsEnabled: enabled,
		LocalTasksDir:   home + "/dicode-tasks",
		DataDir:         home + "/.dicode",
		Port:            portOr(port, defaultPort),
		Passphrase:      GeneratePassphrase(),
	}
}

// WriteConfig writes the generated config to configPath, creating parent
// directories as needed. The file contains server.secret in plaintext, so
// it is written 0600 under a 0700 parent dir — on shared hosts this
// prevents other local users from reading the dashboard passphrase.
func WriteConfig(configPath, content string) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(configPath, []byte(content), 0o600)
}
