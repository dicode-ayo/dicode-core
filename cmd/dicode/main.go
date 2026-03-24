package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/onboarding"
	"github.com/dicode/dicode/pkg/registry"
	jsruntime "github.com/dicode/dicode/pkg/runtime/js"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/source"
	"github.com/dicode/dicode/pkg/source/local"
	"github.com/dicode/dicode/pkg/trigger"
	"github.com/dicode/dicode/pkg/webui"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"
)

var version = "dev"

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", "dicode.yaml", "path to config file")
	flag.Parse()

	// Handle subcommands.
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

	// First-run onboarding: if no config exists, generate a default one.
	if onboarding.Required(configPath) {
		fmt.Println("Welcome to dicode! No config found — creating a local-only setup.")
		home, _ := os.UserHomeDir()
		content := onboarding.DefaultLocalConfig(home+"/dicode-tasks", home+"/.dicode")
		if err := onboarding.WriteConfig(configPath, content); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Created %s\n", configPath)
		if err := os.MkdirAll(home+"/dicode-tasks", 0755); err != nil {
			fmt.Fprintf(os.Stderr, "failed to create tasks dir: %v\n", err)
		}
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	logBroadcaster := webui.NewLogBroadcaster()

	logger, err := buildLogger(cfg.LogLevel, logBroadcaster)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to init logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	logger.Info("dicode starting", zap.String("version", version))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, configPath, logBroadcaster, logger); err != nil {
		logger.Fatal("dicode exited with error", zap.Error(err))
	}
}

func run(ctx context.Context, cfg *config.Config, configPath string, logBroadcaster *webui.LogBroadcaster, log *zap.Logger) error {
	// 1. Open database.
	database, err := db.Open(db.Config{
		Type:   cfg.Database.Type,
		Path:   cfg.Database.Path,
		URLEnv: cfg.Database.URLEnv,
	})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// 2. Build secrets chain.
	secretsChain, localSecrets := buildSecretsChain(cfg, database, log)

	// 3. Task registry.
	reg := registry.New(database)

	// 4. JS runtime.
	rt := jsruntime.New(reg, secretsChain, database, log)

	// 5. Trigger engine.
	eng := trigger.New(reg, rt, log)

	// 6. Sources + reconciler.
	sources, err := buildSources(cfg, log)
	if err != nil {
		return fmt.Errorf("build sources: %w", err)
	}
	rec := registry.NewReconciler(reg, sources, log)
	rec.OnRegister = eng.Register
	rec.OnUnregister = eng.Unregister

	// 7. Web UI.
	port := cfg.Server.Port
	if port == 0 {
		port = 8080
	}
	srv, err := webui.New(port, reg, eng, cfg, configPath, localSecrets, logBroadcaster, log)
	if err != nil {
		return fmt.Errorf("build webui: %w", err)
	}

	// 8. Run everything concurrently.
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return rec.Run(ctx) })
	g.Go(func() error { return eng.Start(ctx) })
	g.Go(func() error { return srv.Start(ctx) })

	return g.Wait()
}

func buildSecretsChain(cfg *config.Config, database db.DB, log *zap.Logger) (secrets.Chain, webui.SecretsManager) {
	var chain secrets.Chain
	var localProvider webui.SecretsManager
	home, _ := os.UserHomeDir()
	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = home + "/.dicode"
	}

	for _, p := range cfg.Secrets.Providers {
		switch p.Type {
		case "local":
			sdb := secrets.NewSQLiteSecretDB(database)
			lp, err := secrets.NewLocalProvider(dataDir, sdb)
			if err != nil {
				log.Warn("local secrets provider init failed", zap.Error(err))
				continue
			}
			chain = append(chain, lp)
			if localProvider == nil {
				localProvider = lp
			}
		case "env", "":
			chain = append(chain, secrets.NewEnvProvider())
		}
	}
	// Default: local + env if no providers configured.
	if len(chain) == 0 {
		sdb := secrets.NewSQLiteSecretDB(database)
		if lp, err := secrets.NewLocalProvider(dataDir, sdb); err == nil {
			chain = append(chain, lp)
			localProvider = lp
		}
		chain = append(chain, secrets.NewEnvProvider())
	}
	return chain, localProvider
}

func buildSources(cfg *config.Config, log *zap.Logger) ([]source.Source, error) {
	var sources []source.Source
	for _, sc := range cfg.Sources {
		switch sc.Type {
		case config.SourceTypeLocal:
			s, err := local.New(sc.Path, sc.Path, log)
			if err != nil {
				return nil, fmt.Errorf("local source %q: %w", sc.Path, err)
			}
			sources = append(sources, s)
		case config.SourceTypeGit:
			log.Warn("git source not yet implemented, skipping", zap.String("url", sc.URL))
		default:
			return nil, fmt.Errorf("unknown source type %q", sc.Type)
		}
	}
	return sources, nil
}

func runTaskCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: dicode task <install|list> [args...]")
		os.Exit(1)
	}
	switch args[0] {
	case "install":
		fmt.Println("task install: not yet implemented")
	case "list":
		fmt.Println("task list: not yet implemented")
	default:
		fmt.Fprintf(os.Stderr, "unknown task subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

func buildLogger(level string, broadcast *webui.LogBroadcaster) (*zap.Logger, error) {
	zapLevel := zapcore.InfoLevel
	if level == "debug" {
		zapLevel = zapcore.DebugLevel
	}
	enc := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	core := zapcore.NewTee(
		zapcore.NewCore(enc, zapcore.AddSync(os.Stderr), zapLevel),
		zapcore.NewCore(enc, zapcore.AddSync(broadcast), zapLevel),
	)
	return zap.New(core, zap.AddCaller()), nil
}
