package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/notify"
	"github.com/dicode/dicode/pkg/onboarding"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	denoruntime "github.com/dicode/dicode/pkg/runtime/deno"
	dockerruntime "github.com/dicode/dicode/pkg/runtime/docker"
	podmanruntime "github.com/dicode/dicode/pkg/runtime/podman"
	pythonruntime "github.com/dicode/dicode/pkg/runtime/python"
	"github.com/dicode/dicode/pkg/secrets"
	"path/filepath"
	"strings"

	"github.com/dicode/dicode/pkg/source"
	gitSource "github.com/dicode/dicode/pkg/source/git"
	"github.com/dicode/dicode/pkg/source/local"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"github.com/dicode/dicode/pkg/tray"
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

	if err := run(ctx, cancel, cfg, configPath, logBroadcaster, logger); err != nil {
		logger.Fatal("dicode exited with error", zap.Error(err))
	}
}

func run(ctx context.Context, cancel context.CancelFunc, cfg *config.Config, configPath string, logBroadcaster *webui.LogBroadcaster, log *zap.Logger) error {
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

	// 3. Task registry + startup cleanup.
	// Remove orphaned containers first (before marking runs cancelled)
	// so the DB status reflects what actually happened to each container.
	dockerruntime.CleanupOrphanedContainers(ctx, log)
	podmanruntime.CleanupOrphanedContainers(ctx, log)

	reg := registry.New(database)
	if stale, err := reg.CleanupStaleRuns(ctx); err != nil {
		log.Warn("stale run cleanup failed", zap.Error(err))
	} else if len(stale) > 0 {
		log.Info("cancelled stale runs from previous session", zap.Strings("tasks", stale))
	}

	// 4. HTTP gateway — created early so runtimes can propagate it to IPC servers.
	gateway := ipc.NewGateway()

	// 5. Build managed runtimes (deno is always available; others depend on config).
	managedRuntimes, eng, err := buildRuntimes(ctx, cfg, reg, secretsChain, database, log, gateway)
	if err != nil {
		return err
	}

	// 6. Sources + reconciler.
	// Auto-register webhook tasks in the gateway so HTTP traffic is routed to them.
	sources, sourceMgr, err := buildSources(cfg, log)
	if err != nil {
		return fmt.Errorf("build sources: %w", err)
	}
	rec := registry.NewReconciler(reg, sources, log)
	// Capture the webhook handler once — it is a singleton backed by the engine.
	webhookH := eng.WebhookHandler()
	// Track webhook paths locally so OnUnregister doesn't need to call reg.Get,
	// which would race with a concurrent re-registration of the same task ID.
	var webhookMu sync.Mutex
	webhookPaths := make(map[string]string) // taskID → registered path
	rec.OnRegister = func(spec *task.Spec) {
		eng.Register(spec)
		if spec.Trigger.Webhook != "" {
			gateway.Register(spec.Trigger.Webhook, webhookH)
			webhookMu.Lock()
			webhookPaths[spec.ID] = spec.Trigger.Webhook
			webhookMu.Unlock()
		}
	}
	rec.OnUnregister = func(id string) {
		webhookMu.Lock()
		path := webhookPaths[id]
		delete(webhookPaths, id)
		webhookMu.Unlock()
		if path != "" {
			gateway.Unregister(path)
		}
		eng.Unregister(id)
	}

	// 7. Web UI.
	port := cfg.Server.Port
	if port == 0 {
		port = 8080
	}
	dataDir := cfg.DataDir
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = home + "/.dicode"
	}
	srv, err := webui.New(port, reg, eng, cfg, configPath, localSecrets, rec, sourceMgr, dataDir, logBroadcaster, log, database, gateway)
	if err != nil {
		return fmt.Errorf("build webui: %w", err)
	}
	srv.SetManagedRuntimes(managedRuntimes)

	// 7. Run everything concurrently.
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return rec.Run(ctx) })
	g.Go(func() error { return eng.Start(ctx) })
	g.Go(func() error { return srv.Start(ctx) })

	// 8. System tray (optional).
	trayEnabled := cfg.Server.Tray == nil || *cfg.Server.Tray // nil = auto = enabled
	if trayEnabled {
		g.Go(func() error {
			tray.Run(ctx, cancel, port, log)
			return nil
		})
	}

	return g.Wait()
}

// buildRuntimes initialises all managed runtimes, registers them in the trigger
// engine, and returns the managed-runtime list (for the Config UI).
func buildRuntimes(
	_ context.Context,
	cfg *config.Config,
	reg *registry.Registry,
	secretsChain secrets.Chain,
	database db.DB,
	log *zap.Logger,
	gateway *ipc.Gateway,
) ([]pkgruntime.ManagedRuntime, *trigger.Engine, error) {
	// --- Deno (always-on default executor) ---
	denoRT, err := denoruntime.New(reg, secretsChain, database, log)
	if err != nil {
		return nil, nil, fmt.Errorf("init deno runtime: %w", err)
	}
	eng := trigger.New(reg, denoRT, log)
	// Wire the engine into the Deno runtime so tasks can orchestrate other tasks.
	denoRT.SetEngine(eng)
	denoRT.SetGateway(gateway)
	// Wire AI config so tasks can call dicode.get_config("ai").
	aiAPIKey := cfg.AI.APIKey
	if aiAPIKey == "" && cfg.AI.APIKeyEnv != "" {
		aiAPIKey = os.Getenv(cfg.AI.APIKeyEnv)
	}
	denoRT.SetAIConfig(cfg.AI.BaseURL, cfg.AI.Model, aiAPIKey)
	// Wire config-level on_failure_chain default.
	if cfg.Defaults.OnFailureChain != "" {
		eng.SetDefaultsOnFailureChain(cfg.Defaults.OnFailureChain)
	}
	// Wire notification provider and global defaults.
	if p := cfg.Notifications.Provider; p != nil {
		eng.SetNotifier(notify.NewNotifier(p.Type, p.URL, p.Topic, p.TokenEnv))
	}
	eng.SetNotifyDefaults(cfg.Notifications.NotifyOnSuccess(), cfg.Notifications.NotifyOnFailure())

	var managed []pkgruntime.ManagedRuntime
	managed = append(managed, denoRT) // *denoruntime.Runtime implements ManagedRuntime

	// Allow overriding the deno version at startup (unusual but supported).
	if rc, ok := cfg.Runtimes["deno"]; ok && rc.Version != "" {
		if denoRT.IsInstalled(rc.Version) {
			if p, err := denoRT.BinaryPath(rc.Version); err == nil {
				eng.RegisterExecutor(task.RuntimeDeno, denoRT.NewExecutor(p))
			}
		}
	}

	// --- Docker ---
	eng.RegisterExecutor(task.RuntimeDocker, dockerruntime.New(reg, log))

	// --- Podman — registered as ManagedRuntime; executor added only if binary found ---
	podmanMgr := podmanruntime.New(reg, log)
	managed = append(managed, podmanMgr)
	if podmanMgr.IsInstalled("") {
		if p, err := podmanMgr.BinaryPath(""); err == nil {
			eng.RegisterExecutor(task.RuntimePodman, podmanMgr.NewExecutor(p))
			log.Info("podman runtime registered", zap.String("path", p))
		}
	}

	// --- Python (uv) — register only if configured + installed ---
	pythonMgr, err := pythonruntime.New(reg, secretsChain, database, log)
	if err != nil {
		log.Fatal("python runtime init", zap.Error(err))
	}
	pythonMgr.SetGateway(gateway)
	managed = append(managed, pythonMgr)

	if rc, ok := cfg.Runtimes["python"]; ok && !rc.Disabled {
		version := rc.Version
		if version == "" {
			version = pythonMgr.DefaultVersion()
		}
		if pythonMgr.IsInstalled(version) {
			if p, err := pythonMgr.BinaryPath(version); err == nil {
				eng.RegisterExecutor(task.Runtime("python"), pythonMgr.NewExecutor(p))
				log.Info("python runtime registered", zap.String("version", version))
			}
		} else {
			log.Info("python runtime configured but not installed — run install from Config UI",
				zap.String("version", version))
		}
	}

	return managed, eng, nil
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

func buildSources(cfg *config.Config, log *zap.Logger) ([]source.Source, *webui.SourceManager, error) {
	dataDir := cfg.DataDir
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = home + "/.dicode"
	}

	tasksetSources := make(map[string]*taskset.Source)
	var sources []source.Source

	for _, sc := range cfg.Sources {
		// Use taskset.Source when the new model is indicated (Name or EntryPath set).
		if sc.Name != "" || sc.EntryPath != "" {
			ts, err := buildTaskSetSource(sc, dataDir, log)
			if err != nil {
				return nil, nil, err
			}
			sources = append(sources, ts)
			name := sourceNameFor(sc)
			tasksetSources[name] = ts
			continue
		}

		switch sc.Type {
		case config.SourceTypeLocal:
			s, err := local.New(sc.Path, sc.Path, log)
			if err != nil {
				return nil, nil, fmt.Errorf("local source %q: %w", sc.Path, err)
			}
			sources = append(sources, s)
		case config.SourceTypeGit:
			gs, err := gitSource.New(dataDir, sc.URL, sc.Branch, sc.PollInterval, sc.Auth.TokenEnv, sc.Auth.SSHKey, log)
			if err != nil {
				return nil, nil, fmt.Errorf("git source %q: %w", sc.URL, err)
			}
			sources = append(sources, gs)
		default:
			return nil, nil, fmt.Errorf("unknown source type %q", sc.Type)
		}
	}

	sourceMgr := webui.NewSourceManager(cfg, tasksetSources, dataDir, log)
	return sources, sourceMgr, nil
}

// sourceNameFor derives the canonical name for a SourceConfig.
// Must stay in sync with webui.sourceName.
func sourceNameFor(sc config.SourceConfig) string {
	if sc.Name != "" {
		return sc.Name
	}
	base := sc.URL
	if base == "" {
		base = sc.Path
	}
	base = strings.TrimRight(base, "/")
	name := filepath.Base(base)
	if ext := filepath.Ext(name); ext == ".yaml" || ext == ".yml" {
		name = strings.TrimSuffix(name, ext)
	}
	return name
}

// buildTaskSetSource creates a taskset.Source for a SourceConfig using the new model.
func buildTaskSetSource(sc config.SourceConfig, dataDir string, log *zap.Logger) (*taskset.Source, error) {
	// Derive namespace from Name, falling back to last segment of URL or Path.
	namespace := sc.Name
	if namespace == "" {
		base := sc.URL
		if base == "" {
			base = sc.Path
		}
		// Strip trailing slashes and take the last path segment.
		base = strings.TrimRight(base, "/")
		namespace = filepath.Base(base)
		// Strip common extensions from local paths.
		if ext := filepath.Ext(namespace); ext == ".yaml" || ext == ".yml" {
			namespace = strings.TrimSuffix(namespace, ext)
		}
	}

	var rootRef *taskset.Ref
	if sc.URL != "" {
		entryPath := sc.EntryPath
		if entryPath == "" {
			entryPath = "taskset.yaml"
		}
		rootRef = &taskset.Ref{
			URL:          sc.URL,
			Branch:       sc.Branch,
			Path:         entryPath,
			PollInterval: sc.PollInterval,
			Auth:         taskset.RefAuth{TokenEnv: sc.Auth.TokenEnv, SSHKey: sc.Auth.SSHKey},
		}
	} else {
		// Local source: Path points to the taskset.yaml file.
		entryPath := sc.Path
		if sc.EntryPath != "" {
			entryPath = sc.EntryPath
		}
		rootRef = &taskset.Ref{Path: entryPath}
	}

	id := sc.URL
	if id == "" {
		id = sc.Path
	}

	src := taskset.NewSource(id, namespace, rootRef, sc.ConfigPath, dataDir, false, sc.PollInterval, log)
	return src, nil
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
