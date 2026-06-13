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
	"sync"
	"syscall"
	"time"

	"github.com/dicode/dicode/pkg/approval"
	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/metrics"
	"github.com/dicode/dicode/pkg/onboarding"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	denoruntime "github.com/dicode/dicode/pkg/runtime/deno"
	dockerruntime "github.com/dicode/dicode/pkg/runtime/docker"
	podmanruntime "github.com/dicode/dicode/pkg/runtime/podman"
	pythonruntime "github.com/dicode/dicode/pkg/runtime/python"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/source"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"github.com/dicode/dicode/pkg/trigger"
	"github.com/dicode/dicode/pkg/webui"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"
	"golang.org/x/term"
)

// bootstrapSettle is how long the approval gate's first-run seeding window
// stays open after the most recent task registration. It spans the initial
// source sync (including a slow first git clone, which delays the first event
// but then streams quickly) and closes shortly after the burst ends.
const bootstrapSettle = 10 * time.Second

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
// resolveDataDir picks the directory the daemon uses for SQLite, sources,
// and run logs. Resolution order: cfg.DataDir → DICODE_DATA_DIR env var →
// $HOME/.dicode. The env-var fallback is what makes the Docker image's
// `ENV DICODE_DATA_DIR=/data` actually redirect state into the mounted
// volume on the very first run, before onboarding has written a config
// (the onboarding default also honors the env var, so the generated
// dicode.yaml bakes the same path in for subsequent starts).
func resolveDataDir(cfg *config.Config) (string, error) {
	if cfg.DataDir != "" {
		return cfg.DataDir, nil
	}
	if d := os.Getenv("DICODE_DATA_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return home + "/.dicode", nil
}

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
	dataDir, err := resolveDataDir(cfg)
	if err != nil {
		return err
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
	if n, err := reg.SweepStalePins(ctx); err == nil {
		if n > 0 {
			log.Info("cleared stale input pins at startup", zap.Int("count", n))
		}
	} else {
		log.Warn("sweep stale input pins failed", zap.Error(err))
	}

	// 5. HTTP gateway.
	gateway := ipc.NewGateway()

	// 6. Managed runtimes + trigger engine.
	managedRuntimes, eng, denoRT, pythonRT, err := buildRuntimes(ctx, cfg, reg, secretsChain, localSecrets, database, log, gateway)
	if err != nil {
		return err
	}

	// replayer is built inside the run-input block below (needs the InputStore)
	// and consumed after webui.New. Declared here so both sites see it.
	var replayer *registry.Replayer

	// 6a. Run-input persistence (Task 14): wire InputStore when enabled.
	if cfg.Defaults.RunInputs.IsEnabled() {
		var deriver secrets.SubKeyDeriver
		for _, p := range secretsChain {
			if d, ok := p.(secrets.SubKeyDeriver); ok {
				deriver = d
				break
			}
		}
		if deriver == nil {
			log.Warn("run-input persistence: no SubKeyDeriver available in secrets chain — persistence disabled")
		} else {
			key, err := deriver.DeriveSubKey("dicode/run-inputs/v1")
			if err != nil {
				log.Warn("run-input persistence: sub-key derive failed", zap.Error(err))
			} else {
				runner := trigger.NewInputStoreTaskRunner(eng)
				is := registry.NewInputStore(registry.NewInputCrypto(key), runner, cfg.Defaults.RunInputs.StorageTask)
				eng.SetInputStore(is)
				denoRT.SetInputStore(is)
				pythonRT.SetInputStore(is)
				// Replayer composes InputStore.Fetch + the engine's fireAsync.
				// Wired after InputStore so dicode.runs.replay finds a populated store.
				replayer = registry.NewReplayer(reg, is, trigger.NewReplayRunner(eng))
				denoRT.SetReplayer(replayer)
				pythonRT.SetReplayer(replayer)
				// srv.SetReplayer is called below after webui is built.
				log.Info("run-input persistence enabled",
					zap.Duration("retention", cfg.Defaults.RunInputs.Retention),
					zap.String("storage_task", cfg.Defaults.RunInputs.StorageTask),
				)
			}
		}
	} else {
		log.Info("run-input persistence disabled by config")
	}

	// 7. Sources + reconciler.
	sources, sourceMgr, err := buildSources(cfg, dataDir, log)
	if err != nil {
		return fmt.Errorf("build sources: %w", err)
	}
	// *webui.SourceManager satisfies both SourceDevModeSetter (Task 6) and
	// RepoPathResolver (Task 8); wire both interfaces into both runtimes so
	// per-run IPC servers can serve set_dev_mode and git.commit_push.
	denoRT.SetSourceManager(sourceMgr)
	denoRT.SetRepoResolver(sourceMgr)
	pythonRT.SetSourceManager(sourceMgr)
	pythonRT.SetRepoResolver(sourceMgr)
	rec := registry.NewReconciler(reg, sources, dataDir, log)
	webhookH := eng.WebhookHandler()
	var webhookMu sync.Mutex
	webhookPaths := make(map[string]string)
	// bodyFullTextualWarned tracks which task IDs have already had their
	// body_full_textual WARN emitted. The reconciler may re-register the same
	// task on each reload cycle; without deduplication the WARN fires every
	// 30 s, flooding the log. LoadOrStore ensures at most one WARN per task ID
	// per daemon lifetime.
	var bodyFullTextualWarned sync.Map
	// arm wires an approval-gated task into the trigger engine plus the
	// daemon-level gateway webhook route and footgun warnings. Called by the
	// approval gate for every task that passes it (immediately for trusted /
	// builtin / already-approved tasks, later from Approve for pending ones).
	arm := func(k task.Kinded) error {
		// The footgun checks below only apply to kind: Task; the gateway-webhook
		// route, however, applies to BOTH kind: Task and kind: PipelineTask —
		// see registerGatewayWebhook (GAP 1: a pipeline's webhook 404'd because
		// only kind: Task wired the daemon-level gateway route).
		spec, isSpec := k.(*task.Spec)
		// Override buildin/run-inputs-cleanup's retention_seconds default to
		// match dicode.yaml's defaults.run_inputs.retention. This must happen
		// before eng.Register so the engine sees the correct default when
		// building the param map for the next cron fire. We mutate the spec
		// slice element in place; the reconciler replaces the spec on each
		// reload, so the override is re-applied on every registration.
		if isSpec && spec.ID == "buildin/run-inputs-cleanup" && cfg.Defaults.RunInputs.Retention > 0 {
			retStr := fmt.Sprintf("%d", int64(cfg.Defaults.RunInputs.Retention.Seconds()))
			for i := range spec.Params {
				if spec.Params[i].Name == "retention_seconds" {
					spec.Params[i].Default = retStr
					break
				}
			}
		}
		if err := eng.Register(k); err != nil {
			// Cross-spec validation (pipeline stage refs pointing at an unknown
			// task, or pipeline ref/cycle errors) is the only error Register
			// currently returns. The reconciler will retry
			// on the next reload, so a transient registry mismatch heals itself;
			// a persistent config error surfaces here every cycle. Skipping the
			// downstream webhook/footgun checks means a half-registered task
			// doesn't claim its gateway path.
			return err
		}
		// Wire the daemon-level gateway webhook route. Kind-aware: handles both
		// kind: Task and kind: PipelineTask, so a pipeline's webhook trigger is
		// reachable over HTTP (GAP 1). Recording the path under the task ID lets
		// the disarm path deregister it the same way for both kinds.
		//
		// Cross-layer note: for a deferred webhook pipeline (cold start, stage not
		// yet registered) eng.Register returned nil, so we claim the gateway route
		// here even though the engine's webhooks map is not populated until the
		// stage lands and retryDeferredPipelines schedules it. Until then a POST
		// reaches the gateway, misses the engine's webhooks lookup, and 404s from
		// the engine layer (correct "not ready" behaviour); the route starts
		// routing once the stage arrives. The route is released on disarm.
		registerGatewayWebhook(gateway, webhookPaths, &webhookMu, webhookH, k)

		if !isSpec {
			return nil
		}
		if spec.Trigger.WebhookAuth && !cfg.Server.Auth {
			log.Warn("task declares trigger.auth:true but server.auth is disabled — any password logs in",
				zap.String("task", spec.ID),
				zap.String("webhook", spec.Trigger.Webhook),
				zap.String("hint", "set server.auth: true in dicode.yaml to require a real passphrase"))
		}
		// body_full_textual footgun: warn once per task ID when a task opts in
		// to persisting raw textual bodies verbatim. Name-based redaction cannot
		// reach unstructured content; operators should confirm this is intentional.
		if spec.RunInputs != nil && spec.RunInputs.BodyFullTextual != nil && *spec.RunInputs.BodyFullTextual {
			if _, alreadyWarned := bodyFullTextualWarned.LoadOrStore(spec.ID, struct{}{}); !alreadyWarned {
				log.Warn("task persists raw textual bodies — confirm this is intentional",
					zap.String("task", spec.ID))
			}
		}
		return nil
	}
	// disarm tears down a task's triggers and gateway route. Used both for
	// removed tasks and for changed tasks held pending approval (their
	// previous version may still be armed).
	disarm := func(id string) {
		webhookMu.Lock()
		path := webhookPaths[id]
		delete(webhookPaths, id)
		webhookMu.Unlock()
		if path != "" {
			gateway.Unregister(path)
		}
		eng.Unregister(id)
	}

	// 7a. Approval gate (#392 phase 1): every new or changed task passes the
	// trust-on-change gate before its triggers arm. Approval records live in
	// dicode.lock next to dicode.yaml; trust policy lives in cfg.Approval.
	configDir, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		return fmt.Errorf("resolve config dir: %w", err)
	}
	lockPath := filepath.Join(configDir, approval.LockFileName)
	_, lockStatErr := os.Stat(lockPath)
	lockExisted := lockStatErr == nil
	lock, err := approval.LoadLock(lockPath)
	if err != nil {
		return fmt.Errorf("load approval lock: %w", err)
	}
	gatePolicy := approval.Policy{
		Enabled:        cfg.Approval.IsEnabled(),
		TrustedSources: map[string]bool{},
		TrustedTasks:   map[string]bool{},
	}
	for name, p := range cfg.Approval.Sources {
		if p.Trust == "always" {
			gatePolicy.TrustedSources[name] = true
		}
	}
	for id, p := range cfg.Approval.Tasks {
		if p.Trust == "always" {
			gatePolicy.TrustedTasks[id] = true
		}
	}
	approvalGate := approval.NewGate(gatePolicy, lock, arm, log)
	// Pending tasks stay in the registry (API visibility, like Enabled:false)
	// and remain resolvable by manual / chain / replay fire paths — the fire
	// guard vetoes those at the engine chokepoint. The same guard also vetoes
	// the task-TEST surfaces: test files run with full host permissions
	// (deno test --allow-all), so an unapproved task's test code must be
	// refused everywhere it can be invoked — REST (webui), CLI control
	// socket, and the per-run dicode.tasks.test IPC capability.
	eng.SetFireGuard(approvalGate.FireGuard)
	denoRT.SetTestGuard(approvalGate.FireGuard)
	pythonRT.SetTestGuard(approvalGate.FireGuard)

	// First-run bootstrap: with no dicode.lock, seed the existing inventory as
	// approved instead of stranding every task behind a gate that has no
	// approve UI yet. The settle timer is armed immediately — not on the
	// first registration — so the window also closes on an install with zero
	// tasks; otherwise the first task to ever appear would be auto-approved.
	// Each registration during the window slides it forward (tolerates a slow
	// first git clone). FinishBootstrap is idempotent, so the timer firing is
	// always safe.
	var bootstrapTimer *time.Timer
	if !lockExisted && gatePolicy.Enabled {
		approvalGate.SetBootstrap(true)
		bootstrapTimer = time.AfterFunc(bootstrapSettle, func() {
			if approvalGate.FinishBootstrap() {
				log.Info("approval gate active — subsequent task changes require approval")
			}
		})
		log.Info("approval gate: no dicode.lock — seeding current tasks as approved (first-run bootstrap); changes after startup require approval",
			zap.String("lock", lockPath))
	}

	rec.OnRegister = func(k task.Kinded) {
		armed, err := approvalGate.Admit(k)
		if err != nil {
			log.Warn("task registration rejected by engine — fix the spec to re-enable",
				zap.String("task", k.TaskID()),
				zap.Error(err))
			return
		}
		if armed {
			// Slide the bootstrap window forward while the initial inventory
			// is still streaming in (handles slow first git clone).
			if approvalGate.Bootstrapping() && bootstrapTimer != nil {
				bootstrapTimer.Reset(bootstrapSettle)
			}
			return
		}
		disarm(k.TaskID())
		log.Warn("task held pending approval — triggers not armed",
			zap.String("task", k.TaskID()),
			zap.String("hint", "set approval.sources.<source>.trust: always or approval.tasks.<id>.trust: always in dicode.yaml, or approve the task"))
	}
	rec.OnUnregister = func(id string) {
		disarm(id)
		approvalGate.Forget(id)
	}

	// 8. Web UI.
	port := cfg.Server.Port
	if port == 0 {
		port = 8080
	}

	// 8.5. Export relay-related config to process env so the buildin tasks
	// (relay-client, auth-start, auth-relay) can read them via Deno.env.get.
	// These tasks declare the var names in permissions.env; Deno's allow-env
	// flag exposes them to the task subprocess.
	if err := os.Setenv("DICODE_DATADIR", dataDir); err != nil {
		return fmt.Errorf("setenv DICODE_DATADIR: %w", err)
	}
	if cfg.Defaults.RunInputs.StorageTask != "" {
		if err := os.Setenv("DICODE_STORAGE_TASK", cfg.Defaults.RunInputs.StorageTask); err != nil {
			return fmt.Errorf("setenv DICODE_STORAGE_TASK: %w", err)
		}
	}
	if cfg.Relay.Enabled && cfg.Relay.ServerURL != "" {
		if err := os.Setenv("DICODE_RELAY_SERVER_URL", cfg.Relay.ServerURL); err != nil {
			return fmt.Errorf("setenv DICODE_RELAY_SERVER_URL: %w", err)
		}
		if err := os.Setenv("DICODE_RELAY_LOCAL_PORT", fmt.Sprintf("%d", port)); err != nil {
			return fmt.Errorf("setenv DICODE_RELAY_LOCAL_PORT: %w", err)
		}
		if brokerURL := cfg.Relay.ResolvedBrokerURL(); brokerURL != "" {
			if err := os.Setenv("DICODE_RELAY_BROKER_URL", brokerURL); err != nil {
				return fmt.Errorf("setenv DICODE_RELAY_BROKER_URL: %w", err)
			}
		}

		// Migration warning: existing users upgrading from the Go relay client
		// have a legacy identity row at "relay.private_key". The new TS task
		// regenerates fresh identity (different UUID, breaks existing webhook
		// URLs). Warn the operator at boot if we see the legacy row.
		var hasLegacy bool
		_ = database.Query(ctx, `SELECT 1 FROM kv WHERE key = ?`,
			[]any{"relay.private_key"},
			func(rows db.Scanner) error {
				if rows.Next() {
					hasLegacy = true
				}
				return nil
			},
		)
		if hasLegacy {
			log.Warn(
				"relay: legacy identity row detected — the new relay-client task " +
					"generates a fresh identity, which means the daemon UUID will " +
					"change and existing webhook URLs will stop working. To preserve " +
					"the old UUID, file a migration request; otherwise reissue any " +
					"webhook URLs after the daemon stabilises.",
			)
		}
	}

	webui.Version = version
	srv, err := webui.New(port, reg, eng, cfg, configPath, localSecrets, rec, sourceMgr, dataDir, logBroadcaster, log, database, gateway)
	if err != nil {
		return fmt.Errorf("build webui: %w", err)
	}
	srv.SetManagedRuntimes(managedRuntimes)
	srv.SetTestGuard(approvalGate.FireGuard)
	// Approve surfaces (#398): pending visibility + POST /api/tasks/{id}/approve
	// + the single-use tokenized approve link for notifications.
	srv.SetApprovalGate(approvalGate)
	srv.SetApprovalTokenStore(approval.NewTokenStore(database))
	// Phase 3 (#399): notify on the transition into pending. Installed here —
	// after srv exists — because the gate is built earlier (step 7a) than the
	// webui Server (step 8), and the hook needs srv for the broadcast + the
	// tokenized approve link. The gate invokes this without its lock held and
	// only on a true pending transition (new hold / hash change), never on an
	// unchanged re-admit, so the 30s reconcile loop cannot spam notifications.
	notifier := approvalNotifier{
		notifyTask: cfg.Approval.NotifyTask,
		broadcast:  srv.BroadcastApprovalPending,
		mintLink:   func(id string) (string, error) { return srv.MintApproveLink(ctx, id) },
		fire: func(id string, params map[string]string) error {
			_, err := eng.FireManual(ctx, id, params)
			return err
		},
		log: log,
	}
	approvalGate.SetPendingHook(func(k task.Kinded, hash string) {
		id := k.TaskID()
		// The reconciler goroutine drives the hook; never let notification work
		// block it, and never let a notify failure crash the daemon.
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("approval notify hook panicked",
						zap.String("task", id), zap.Any("panic", r))
				}
			}()
			notifier.notify(id, hash)
		}()
	})
	if replayer != nil {
		srv.SetReplayer(replayer)
	}

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
	// Wire API-key minting so `dicode mcp install` and friends can
	// auto-generate keys via the control socket. The webui Server holds
	// the apiKeyStore; control just dispatches.
	ctrlSrv.SetAPIKeyMinter(srv)

	// Approval-gate veto for `dicode task test` — same guard as the engine's
	// fire paths and the REST test endpoint.
	ctrlSrv.SetTestGuard(approvalGate.FireGuard)

	// `dicode task approve` — the control socket is a trusted local channel.
	ctrlSrv.SetTaskApprover(approvalGate.Approve)

	// Wire AI-first task authoring so `dicode task create|edit|save|cancel`
	// reuses the same source manager and author_sessions store the REST
	// handlers use.
	ctrlSrv.SetAuthoringService(srv)

	// Wire task deletion so `dicode task delete` can remove a task from its
	// source. The SourceManager owns source state, repo paths, and dev-clones.
	ctrlSrv.SetTaskDeleter(sourceMgr)

	// Wire the generic dicode.crypto.{encrypt, decrypt} IPC verb so buildin
	// tasks (relay-client, auth-start, auth-relay) can encrypt/decrypt blobs
	// via DeriveSubKey-derived sub-keys. Sub-keys never cross IPC; only the
	// encrypt/decrypt operations are exposed.
	{
		var cryptoDeriver secrets.SubKeyDeriver
		for _, p := range secretsChain {
			if d, ok := p.(secrets.SubKeyDeriver); ok {
				cryptoDeriver = d
				break
			}
		}
		if cryptoDeriver == nil {
			log.Warn("crypto IPC handler: no SubKeyDeriver available in secrets chain — dicode.crypto.{encrypt,decrypt} disabled")
		} else {
			denoRT.SetCryptoHandler(cryptoDeriver)
		}
	}

	// 10. Run everything concurrently.
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return rec.Run(ctx) })
	g.Go(func() error { return eng.Start(ctx) })
	g.Go(func() error { return srv.Start(ctx) })
	g.Go(func() error { return ctrlSrv.Start(ctx) })

	// Best-effort container image GC (issue #380): reclaim dicode-built
	// images whose task is gone or whose Dockerfile hash moved on. The
	// first pass is delayed so the reconciler has populated the registry
	// (an empty registry would treat every dicode image as orphaned);
	// later passes run daily.
	g.Go(func() error {
		timer := time.NewTimer(2 * time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-timer.C:
			}
			specs := reg.All()
			dockerruntime.ReclaimOrphanedImages(ctx, log, specs)
			podmanruntime.ReclaimOrphanedImages(ctx, log, specs)
			timer.Reset(24 * time.Hour)
		}
	})

	// Relay client now runs as the buildin/relay-client daemon task, which
	// reconciler-launches automatically when cfg.Relay.Enabled = true.
	// Daemon-level wiring (env vars + crypto handler) is set above; nothing
	// to do here.

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
) ([]pkgruntime.ManagedRuntime, *trigger.Engine, *denoruntime.Runtime, *pythonruntime.Runtime, error) {
	denoRT, err := denoruntime.New(reg, secretsChain, database, log)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("init deno runtime: %w", err)
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
	// Issue #119: engine implements envresolve.ProviderRunner so the
	// runtimes' env resolvers can spawn provider tasks back through it.
	// SetDenoRuntime/SetPythonRuntime let the engine swap the runtime's
	// per-run secretOutputCh for each provider invocation.
	eng.SetDenoRuntime(denoRT)
	denoRT.SetProviderRunner(eng)
	// Issue #242: share a single env resolver across all task launches so
	// that provider.cache_ttl survives across fires. The resolver's cache is
	// in-memory and reset on daemon restart.
	denoRT.SetEnvResolver(eng.Resolver())
	if !cfg.Defaults.OnFailureChain.IsZero() {
		if err := eng.SetDefaultsOnFailureChain(cfg.Defaults.OnFailureChain); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("set on_failure_chain defaults: %w", err)
		}
	}
	var managed []pkgruntime.ManagedRuntime
	managed = append(managed, denoRT)

	if rc, ok := cfg.Runtimes["deno"]; ok && rc.Version != "" {
		if denoRT.IsInstalled(rc.Version) {
			if p, err := denoRT.BinaryPath(rc.Version); err == nil {
				eng.RegisterExecutor(task.RuntimeDeno, denoRT.NewExecutor(p))
			}
		}
	}

	// Container security floor (issue #380): both container runtimes
	// validate untrusted task host config against the operator policy
	// before creating containers. The zero-value policy (no
	// container_security block in dicode.yaml) denies every dangerous
	// escape.
	secPolicy := cfg.ContainerSecurity.Policy()

	dockerRT := dockerruntime.New(reg, log)
	dockerRT.SetPolicy(secPolicy)
	eng.RegisterExecutor(task.RuntimeDocker, dockerRT)

	podmanMgr := podmanruntime.New(reg, log)
	podmanMgr.SetPolicy(secPolicy)
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
	eng.SetPythonRuntime(pythonMgr)
	pythonMgr.SetProviderRunner(eng)
	pythonMgr.SetEnvResolver(eng.Resolver())
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

	return managed, eng, denoRT, pythonMgr, nil
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

	// Each entry in cfg.Spec.Entries is a named source. The entry key is the
	// namespace; entry.Ref is the source descriptor (local path or git URL).
	for name, entry := range cfg.Spec.Entries {
		if entry == nil || entry.Ref == nil {
			continue
		}
		ts := buildTaskSetSourceFromEntry(name, entry, dataDir, log)
		sources = append(sources, ts)
		tasksetSources[name] = ts
	}

	sourceMgr := webui.NewSourceManager(cfg, tasksetSources, dataDir, log)
	return sources, sourceMgr, nil
}

func buildTaskSetSourceFromEntry(name string, entry *taskset.Entry, dataDir string, log *zap.Logger) *taskset.Source {
	// The entry key is the namespace. The ref points at the taskset.yaml.
	// applyDefaults has already expanded ${VAR} and set branch/poll defaults.
	ref := entry.Ref
	id := ref.URL
	if id == "" {
		id = ref.Path
	}
	pollInterval := ref.PollInterval

	var opts []taskset.SourceOption
	if entry.Overrides != nil {
		opts = append(opts, taskset.WithParentOverrides(entry.Overrides))
	}
	return taskset.NewSource(id, name, ref, "", dataDir, false, pollInterval, log, opts...)
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
