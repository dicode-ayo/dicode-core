package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/onboarding"
	"go.uber.org/zap"
)

var version = "dev"

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", "dicode.yaml", "path to config file")
	flag.Parse()

	// Handle subcommands
	if flag.NArg() > 0 {
		switch flag.Arg(0) {
		case "task":
			runTaskCmd(flag.Args()[1:])
			return
		case "version":
			fmt.Printf("dicode %s\n", version)
			return
		}
	}

	// First-run onboarding: if no config exists, launch the setup wizard.
	if onboarding.Required(configPath) {
		fmt.Println("Welcome to dicode! Opening setup wizard...")
		// TODO: launch onboarding wizard (temporary HTTP server + open browser)
		// For now, generate a default local-only config so dicode can start.
		home, _ := os.UserHomeDir()
		content := onboarding.DefaultLocalConfig(home+"/dicode-tasks", home+"/.dicode")
		if err := onboarding.WriteConfig(configPath, content); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Created %s — edit it to add git sources or change settings.\n", configPath)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	logger, err := buildLogger(cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to init logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	logger.Info("dicode starting", zap.String("version", version))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, logger); err != nil {
		logger.Fatal("dicode exited with error", zap.Error(err))
	}
}

func run(ctx context.Context, cfg *config.Config, log *zap.Logger) error {
	// TODO: wire up components
	// 1. registry
	// 2. git watcher + reconciler
	// 3. trigger engine
	// 4. web server
	<-ctx.Done()
	log.Info("shutting down")
	return nil
}

func runTaskCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: dicode task <install|list> [args...]")
		os.Exit(1)
	}
	switch args[0] {
	case "install":
		// TODO: pkg/store installer
		fmt.Println("task install: not yet implemented")
	case "list":
		// TODO: list tasks from registry
		fmt.Println("task list: not yet implemented")
	default:
		fmt.Fprintf(os.Stderr, "unknown task subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

func buildLogger(level string) (*zap.Logger, error) {
	switch level {
	case "debug":
		return zap.NewDevelopment()
	default:
		return zap.NewProduction()
	}
}
