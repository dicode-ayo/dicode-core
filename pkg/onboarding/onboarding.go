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
}

// Run is the single entry point called by the daemon when no config exists.
// It picks a wizard surface, gathers choices, writes the config, and prints
// the success banner.
func Run(ctx context.Context, configPath string, opts RunOptions) error {
	surface, err := PickSurface(opts.In, opts.Out, opts.IsTTY, opts.HasDisplay, opts.Env)
	if err != nil {
		return fmt.Errorf("pick surface: %w", err)
	}

	var res Result
	switch surface {
	case SurfaceSilent:
		res = defaultResult(opts.Home)
	case SurfaceCLI:
		res, err = RunCLI(opts.In, opts.Out, opts.Home)
	case SurfaceBrowser:
		res, err = RunBrowser(ctx, opts.Home)
	}
	if err != nil {
		return fmt.Errorf("wizard: %w", err)
	}

	if err := WriteConfig(configPath, RenderConfig(res)); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	PrintSuccess(opts.Out, res, configPath)
	return nil
}

// defaultResult produces the Result used for non-interactive / silent runs:
// all curated tasksets on, default paths under home, port 8080, generated
// passphrase.
func defaultResult(home string) Result {
	enabled := make(map[string]bool, len(TaskSetPresets))
	for _, p := range TaskSetPresets {
		enabled[p.Name] = p.DefaultOn
	}
	return Result{
		TaskSetsEnabled: enabled,
		LocalTasksDir:   home + "/dicode-tasks",
		DataDir:         home + "/.dicode",
		Port:            defaultPort,
		Passphrase:      GeneratePassphrase(),
	}
}


// WriteConfig writes the generated config to configPath, creating parent
// directories as needed.
func WriteConfig(configPath, content string) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(configPath, []byte(content), 0644)
}
