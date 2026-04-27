// Package daemon implements the dicode daemon process. It runs the task
// engine, reconciler, runtimes, HTTP gateway, web UI, and the control
// socket that the dicode CLI connects to.
package daemon

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/metrics"
	"github.com/dicode/dicode/pkg/notify"
	"github.com/dicode/dicode/pkg/onboarding"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/relay"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	denoruntime "github.com/dicode/dicode/pkg/runtime/deno"
	dockerruntime "github.com/dicode/dicode/pkg/runtime/docker"
	podmanruntime "github.com/dicode/dicode/pkg/runtime/podman"
	pythonruntime "github.com/dicode/dicode/pkg/runtime/python"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/source"
	gitSource "github.com/dicode/dicode/pkg/source/git"
	"github.com/dicode/dicode/pkg/source/local"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"github.com/dicode/dicode/pkg/trigger"
	"github.com/dicode/dicode/pkg/webui"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"
	"golang.org/x/term"
)

// Run starts the daemon process. It blocks until the context is cancelled
// (via signal) or a fatal error occurs. configPath is the path to
// dicode.yaml; portOverride, when non-zero, is propagated to the
// onboarding wizard (seeds the advanced default and silent fallback) so
// `dicode daemon --port=N` writes server.port: N on first run.
func Run(configPath string, portOverride int, version string) {
	// Signal-aware context covers both onboarding and the main daemon loop
	// so Ctrl-C during the wizard cancels the ephemeral HTTP listener.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// First-run onboarding: if no config exists, run the wizard.
	if onboarding.Required(configPath) {
		home, _ := os.UserHomeDir()
		opts := onboarding.RunOptions{
			IsTTY:      term.IsTerminal(int(os.Stdin.Fd())),
			HasDisplay: hasDisplay(),
			In:         os.Stdin,
			Out:        os.Stdout,
			Home:       home,
			Env:        os.Getenv,
			Port:       portOverride,
		}
		if err := onboarding.Run(ctx, configPath, opts); err != nil {
			fmt.Fprintf(os.Stderr, "onboarding failed: %v\n", err)
			os.Exit(1)
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

	logger.Info("dicode daemon starting", zap.String("version", version))

	if err := run(ctx, cancel, cfg, configPath, version, logBroadcaster, logger); err != nil {
		logger.Fatal("dicode daemon exited with error", zap.Error(err))
	}
}

// hasDisplay is a best-effort detector for whether a GUI is reachable.
// On darwin/windows we assume yes whenever the process has a TTY (the
// caller checks that separately). On Linux we require an X or Wayland
// server to be advertised, since headless servers commonly have a TTY
// but no way to open a browser.
func hasDisplay() bool {
	switch runtime.GOOS {
	case "darwin", "windows":
		return true
	default:
		return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	}
}

func run(ctx context.Context, cancel context.CancelFunc, cfg *config.Config, configPath, version string, logBroadcaster *webui.LogBroadcaster, log *zap.Logger) error {
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

	// 2. Resolve data directory.
	dataDir := cfg.DataDir
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("cannot determine home directory: %w", err)
		}
		dataDir = home + "/.dicode"
	}

	// 3. Build secrets chain.
	secretsChain, localSecrets := buildSecretsChain(cfg, dataDir, database, log)

	// 4. Task registry + startup cleanup.
	dockerruntime.CleanupOrphanedContainers(ctx, log)
	podmanruntime.CleanupOrphanedContainers(ctx, log)

	reg := registry.New(database)
	if stale, err := reg.CleanupStaleRuns(ctx); err != nil {
		log.Warn("stale run cleanup failed", zap.Error(err))
	} else if len(stale) > 0 {
		log.Info("cancelled stale runs from previous session", zap.Strings("tasks", stale))
	}

	// 5. HTTP gateway.
	gateway := ipc.NewGateway()

	// 6. Managed runtimes + trigger engine.
	managedRuntimes, eng, denoRT, err := buildRuntimes(ctx, cfg, reg, secretsChain, localSecrets, database, log, gateway)
	if err != nil {
		return err
	}

	// 7. Sources + reconciler.
	sources, sourceMgr, err := buildSources(cfg, dataDir, log)
	if err != nil {
		return fmt.Errorf("build sources: %w", err)
	}
	rec := registry.NewReconciler(reg, sources, log)
	webhookH := eng.WebhookHandler()
	var webhookMu sync.Mutex
	webhookPaths := make(map[string]string)
	rec.OnRegister = func(spec *task.Spec) {
		eng.Register(spec)
		if spec.Trigger.Webhook != "" {
			gateway.Register(spec.Trigger.Webhook, webhookH)
			webhookMu.Lock()
			webhookPaths[spec.ID] = spec.Trigger.Webhook
			webhookMu.Unlock()
		}
		if spec.Trigger.WebhookAuth && !cfg.Server.Auth {
			log.Warn("task declares trigger.auth:true but server.auth is disabled — any password logs in",
				zap.String("task", spec.ID),
				zap.String("webhook", spec.Trigger.Webhook),
				zap.String("hint", "set server.auth: true in dicode.yaml to require a real passphrase"))
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

	// 8. Web UI.
	port := cfg.Server.Port
	if port == 0 {
		port = 8080
	}
	webui.Version = version
	srv, err := webui.New(port, reg, eng, cfg, configPath, localSecrets, rec, sourceMgr, dataDir, logBroadcaster, log, database, gateway)
	if err != nil {
		return fmt.Errorf("build webui: %w", err)
	}
	srv.SetManagedRuntimes(managedRuntimes)

	// 9. Control socket for CLI clients.
	socketPath := filepath.Join(dataDir, "daemon.sock")
	tokenPath := filepath.Join(dataDir, "daemon.token")
	mp := ipc.MetricsProvider{
		ReadDaemon: func() (float64, float64, int, *int64) {
			dm := metrics.ReadDaemonMetrics()
			return dm.HeapAllocMB, dm.HeapSysMB, dm.Goroutines, dm.CPUMs
		},
		ActivePIDs: denoruntime.ActivePIDs,
		ReadChildren: func(pids []int, activeTasks int) (float64, *int64) {
			cm := metrics.ReadChildMetrics(pids, activeTasks)
			return cm.ChildRSSMB, cm.ChildCPUMs
		},
	}
	ctrlSrv, err := ipc.NewControlServer(socketPath, tokenPath, reg, eng, localSecrets, mp, version, log, database, cfg.AI.Task)
	if err != nil {
		return fmt.Errorf("build control server: %w", err)
	}

	// 10. Run everything concurrently.
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return rec.Run(ctx) })
	g.Go(func() error { return eng.Start(ctx) })
	g.Go(func() error { return srv.Start(ctx) })
	g.Go(func() error { return ctrlSrv.Start(ctx) })

	// 11. Relay client — optional, enabled via config.
	if cfg.Relay.Enabled && cfg.Relay.ServerURL != "" {
		id, err := relay.LoadOrGenerateIdentity(ctx, database, log)
		if err != nil {
			log.Warn("relay: identity init failed, relay disabled", zap.Error(err))
		} else {
			rc := relay.NewClient(cfg.Relay.ServerURL, id, port, database, log)
			srv.SetRelayClient(rc)
			// Wire the daemon's relay identity into the deno runtime so
			// the auth-start and auth-relay built-in tasks can drive the
			// broker flow via dicode.oauth.*. Pending sessions are shared
			// across runs so auth-start and auth-relay correlate by
			// session id. The broker speaks HTTPS at the same host as the
			// WSS relay endpoint.
			//
			// rc.SupportsOAuth is passed as a gating function (issue #104):
			// pre-split brokers advertise protocol < 2, in which case the
			// IPC layer refuses OAuth flows with a clear operator error
			// rather than silently failing to decrypt the delivery.
			//
			// rotationActive (issue #144) is flipped true by the rotator
			// closure below after `relay.RotateIdentity` succeeds. Until
			// the daemon restarts (which reconstructs denoRT and re-reads
			// Identity), the in-memory `id` still points at the OLD keys;
			// the IPC layer refuses new OAuth flows so the operator isn't
			// silently handed URLs signed under the identity they just
			// retired.
			pending := relay.NewPendingSessions()
			var rotationActive atomic.Bool
			rotationActiveFn := func() bool { return rotationActive.Load() }
			if brokerURL := cfg.Relay.ResolvedBrokerURL(); brokerURL != "" {
				denoRT.SetOAuthBroker(id, brokerURL, pending, rc.BrokerPubkey, rc.SupportsOAuth, rotationActiveFn)
			} else {
				log.Warn("relay: no broker URL (neither relay.broker_url nor a parsable relay.server_url) — OAuth broker disabled",
					zap.String("server_url", cfg.Relay.ServerURL),
					zap.String("broker_url", cfg.Relay.BrokerURL),
					zap.String("hint", "set relay.broker_url: https://... in dicode.yaml, or use server_url: wss://..."))
			}
			g.Go(func() error { pending.StartSweep(ctx); return nil })
			// Identity rotation via `dicode relay rotate-identity`. The
			// callback regenerates BOTH the sign and decrypt keypairs
			// atomically (split identity, issue #104) and invalidates any
			// outstanding OAuth sessions (they were encrypted to the old
			// DecryptKey). The running relay WSS connection keeps the old
			// in-memory identity until the daemon restarts — documented
			// in the RelayRotateResult warning returned to the CLI.
			ctrlSrv.SetRelayIdentityRotator(func(ctx context.Context) (string, error) {
				// Invariant: pending.Clear() runs BEFORE relay.RotateIdentity().
				// Operator intent of rotation is "the old identity is dead from
				// this point forward", so every in-flight flow issued against
				// the old identity must be invalidated at the same moment as
				// the DB swap. If Rotate ran first, the window between Rotate
				// and Clear would leave in-flight flows still completing
				// against the old DecryptKey (the broker has it in
				// session.pubkey, unchanged by DB rotation) — inconsistent
				// with the operator's mental model of rotation as a hard
				// cutover.
				//
				// Note: the `id` pointer captured in denoRT.SetOAuthBroker
				// above is NOT replaced here. The running daemon continues
				// using the old in-memory identity for WSS + ECIES decrypt
				// until a restart. This is the hazard relayRotateWarning
				// calls out — the rotation point is "DB + pending cutover";
				// the running-connection cutover is at restart.
				//
				// rotationActive (issue #144) flips true AFTER the DB swap
				// succeeds so the IPC layer starts refusing dicode.oauth.*
				// immediately. A failed rotation leaves the flag clear,
				// keeping OAuth flows working against the unchanged identity.
				//
				// Trade-off: there is a sub-microsecond window between
				// RotateIdentity returning nil and Store(true) below where
				// a concurrent OAuth IPC call could pass the gate and get a
				// URL signed under the old identity. Flipping the flag first
				// would require rolling back on error (re-setting to false);
				// the current order trades that race for simpler failure
				// semantics. Do not move the Store earlier without adding
				// the rollback path.
				dropped := pending.Clear()
				oldUUID := id.UUID
				newID, err := relay.RotateIdentity(ctx, database)
				if err != nil {
					return "", err
				}
				rotationActive.Store(true)
				log.Warn("relay identity rotated",
					zap.String("old_uuid", oldUUID),
					zap.String("new_uuid", newID.UUID),
					zap.Int("dropped_sessions", dropped),
				)
				return newID.UUID, nil
			})
			g.Go(func() error { return rc.Run(ctx) })
		}
	}

	return g.Wait()
}

func buildRuntimes(
	_ context.Context,
	cfg *config.Config,
	reg *registry.Registry,
	secretsChain secrets.Chain,
	secretsMgr secrets.Manager,
	database db.DB,
	log *zap.Logger,
	gateway *ipc.Gateway,
) ([]pkgruntime.ManagedRuntime, *trigger.Engine, *denoruntime.Runtime, error) {
	denoRT, err := denoruntime.New(reg, secretsChain, database, log)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("init deno runtime: %w", err)
	}
	eng := trigger.New(reg, denoRT, log)
	eng.SetDB(database)
	eng.SetSecrets(secretsChain)
	// Config value first, env var overrides when set. 0 = unlimited. Always
	// call SetMaxConcurrentTasks so an env override of "0" correctly clears
	// a config-set cap, and so operators get observable confirmation of
	// which source (config vs env) won.
	maxConcurrent := cfg.Execution.MaxConcurrentTasks
	source := "config"
	if v := os.Getenv("DICODE_MAX_CONCURRENT_TASKS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxConcurrent = n
			source = "env"
		} else {
			log.Error("DICODE_MAX_CONCURRENT_TASKS: invalid integer value — falling back to config",
				zap.String("value", v),
				zap.Int("using_config_value", maxConcurrent),
				zap.Error(err))
		}
	}
	// Negative values and overflow are handled inside SetMaxConcurrentTasks.
	eng.SetMaxConcurrentTasks(maxConcurrent)
	log.Info("task concurrency configured",
		zap.Int("max_concurrent_tasks", maxConcurrent),
		zap.String("source", source))
	denoRT.SetEngine(eng)
	denoRT.SetGateway(gateway)
	denoRT.SetSecretsManager(secretsMgr)
	if cfg.Defaults.OnFailureChain != "" {
		eng.SetDefaultsOnFailureChain(cfg.Defaults.OnFailureChain)
	}
	if p := cfg.Notifications.Provider; p != nil {
		eng.SetNotifier(notify.NewNotifier(p.Type, p.URL, p.Topic, p.TokenEnv))
	}
	eng.SetNotifyDefaults(cfg.Notifications.NotifyOnSuccess(), cfg.Notifications.NotifyOnFailure())

	var managed []pkgruntime.ManagedRuntime
	managed = append(managed, denoRT)

	if rc, ok := cfg.Runtimes["deno"]; ok && rc.Version != "" {
		if denoRT.IsInstalled(rc.Version) {
			if p, err := denoRT.BinaryPath(rc.Version); err == nil {
				eng.RegisterExecutor(task.RuntimeDeno, denoRT.NewExecutor(p))
			}
		}
	}

	eng.RegisterExecutor(task.RuntimeDocker, dockerruntime.New(reg, log))

	podmanMgr := podmanruntime.New(reg, log)
	managed = append(managed, podmanMgr)
	if podmanMgr.IsInstalled("") {
		if p, err := podmanMgr.BinaryPath(""); err == nil {
			eng.RegisterExecutor(task.RuntimePodman, podmanMgr.NewExecutor(p))
			log.Info("podman runtime registered", zap.String("path", p))
		}
	}

	pythonMgr, err := pythonruntime.New(reg, secretsChain, database, log)
	if err != nil {
		log.Fatal("python runtime init", zap.Error(err))
	}
	pythonMgr.SetGateway(gateway)
	pythonMgr.SetSecretsManager(secretsMgr)
	managed = append(managed, pythonMgr)

	if rc, ok := cfg.Runtimes["python"]; ok && !rc.Disabled {
		ver := rc.Version
		if ver == "" {
			ver = pythonMgr.DefaultVersion()
		}
		if pythonMgr.IsInstalled(ver) {
			if p, err := pythonMgr.BinaryPath(ver); err == nil {
				eng.RegisterExecutor(task.Runtime("python"), pythonMgr.NewExecutor(p))
				log.Info("python runtime registered", zap.String("version", ver))
			}
		} else {
			log.Info("python runtime configured but not installed", zap.String("version", ver))
		}
	}

	return managed, eng, denoRT, nil
}

func buildSecretsChain(cfg *config.Config, dataDir string, database db.DB, log *zap.Logger) (secrets.Chain, secrets.Manager) {
	var chain secrets.Chain
	var localProvider secrets.Manager

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

func buildSources(cfg *config.Config, dataDir string, log *zap.Logger) ([]source.Source, *webui.SourceManager, error) {
	tasksetSources := make(map[string]*taskset.Source)
	var sources []source.Source

	for _, sc := range cfg.Sources {
		if sc.Name != "" || sc.EntryPath != "" {
			ts, err := buildTaskSetSource(sc, dataDir, log)
			if err != nil {
				return nil, nil, err
			}
			sources = append(sources, ts)
			tasksetSources[sourceNameFor(sc)] = ts
			continue
		}
		switch sc.Type {
		case config.SourceTypeLocal:
			// Watch is a *bool pointer after #177; applyDefaults guarantees
			// it's non-nil here, but treat nil as "watch enabled" defensively.
			watchEnabled := sc.Watch == nil || *sc.Watch
			s, err := local.New(sc.Path, sc.Path, watchEnabled, log)
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

func buildTaskSetSource(sc config.SourceConfig, dataDir string, log *zap.Logger) (*taskset.Source, error) {
	namespace := sc.Name
	if namespace == "" {
		base := sc.URL
		if base == "" {
			base = sc.Path
		}
		base = strings.TrimRight(base, "/")
		namespace = filepath.Base(base)
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
	return taskset.NewSource(id, namespace, rootRef, sc.ConfigPath, dataDir, false, sc.PollInterval, log), nil
}

func buildLogger(level string, broadcast *webui.LogBroadcaster) (*zap.Logger, error) {
	zapLevel := zapcore.InfoLevel
	if level == "debug" {
		zapLevel = zapcore.DebugLevel
	}

	// Console encoder: colored level, human-readable timestamp, clean layout.
	consoleCfg := zap.NewDevelopmentEncoderConfig()
	consoleCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
	consoleCfg.EncodeTime = zapcore.TimeEncoderOfLayout("2006-01-02 15:04:05")
	consoleCfg.ConsoleSeparator = "  "
	consoleEnc := zapcore.NewConsoleEncoder(consoleCfg)

	// JSON encoder for the web UI log broadcaster.
	jsonEnc := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())

	core := zapcore.NewTee(
		zapcore.NewCore(consoleEnc, zapcore.AddSync(os.Stderr), zapLevel),
		zapcore.NewCore(jsonEnc, zapcore.AddSync(broadcast), zapLevel),
	)
	return zap.New(core, zap.AddCaller()), nil
}
