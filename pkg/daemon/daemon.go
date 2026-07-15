// Package daemon implements the dicode daemon process. It runs the task
// engine, reconciler, runtimes, HTTP gateway, web UI, and the control
// socket that the dicode CLI connects to.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/dicode/dicode/internal/gitops"
	"github.com/dicode/dicode/pkg/approval"
	"github.com/dicode/dicode/pkg/audit"
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

// resumeSweepInterval is how often runServices sweeps suspended runs whose
// resume_deadline has passed and cancels them with ReasonResumeTimeout (#95).
const resumeSweepInterval = 1 * time.Minute

// Run starts the daemon process. It blocks until the context is cancelled
// (via signal) or a fatal error occurs. configPath is the path to
// dicode.yaml; portOverride, when non-zero, is propagated to the
// onboarding wizard (seeds the advanced default and silent fallback) so
// `dicode daemon --port=N` writes server.port: N on first run.
func Run(configPath string, portOverride int, version string) {
	// Signal-aware context covers both onboarding and the main daemon loop
	// so Ctrl-C during the wizard cancels the ephemeral HTTP listener.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
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

// relayConfigured reports whether the operator has configured the dicode-relay
// server. It mirrors the gate used when exporting DICODE_RELAY_* env vars at boot.
func relayConfigured(cfg *config.Config) bool {
	return cfg.Relay.Enabled && cfg.Relay.ServerURL != ""
}

// gateRelayServerBody disables the buildin/relay-server-body daemon when the
// relay is not configured. With trigger.daemon + restart:always the engine would
// otherwise auto-start it on every daemon, and its `import "npm:dicode-relay/start"`
// reads process.env beyond the task's declared --allow-env names — throwing
// NotCapable at import time and crash-looping even where the relay is unused.
// Disabling before eng.Register keeps the engine from spawning the daemon unless
// the operator has actually configured the relay. No-op for every other task.
func gateRelayServerBody(spec *task.Spec, cfg *config.Config) bool {
	if spec.ID == "buildin/relay-server-body" && !relayConfigured(cfg) {
		spec.Enabled = false
		return true
	}
	return false
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
	// 0. Install the operator-trusted git-remote allowlist (#537) before any
	// source is polled, so both SSRF guard layers honour it from the first
	// clone. Already validated in config.Load; the error path is defensive.
	allowlist, err := cfg.SourceSecurity.Allowlist()
	if err != nil {
		return err
	}
	gitops.SetInternalHostAllowlist(allowlist)

	// 1. Open database.
	database, err := openDatabase(cfg)
	if err != nil {
		return err
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
	reg := setupRegistry(ctx, database, log)

	// 5. HTTP gateway.
	gateway := ipc.NewGateway()

	// 6. Managed runtimes + trigger engine.
	managedRuntimes, eng, denoRT, pythonRT, err := buildRuntimes(ctx, cfg, reg, secretsChain, localSecrets, database, log, gateway)
	if err != nil {
		return err
	}

	// 6a. Run-input persistence (Task 14): wire InputStore when enabled.
	// replayer is nil when persistence is disabled; consumed after webui.New.
	replayer := wireRunInputPersistence(cfg, secretsChain, reg, eng, denoRT, pythonRT, log)

	// 7. Sources + reconciler.
	sourceMgr, rec, err := initSources(cfg, dataDir, reg, denoRT, pythonRT, log)
	if err != nil {
		return err
	}
	arm, disarm := newArmDisarm(cfg, eng, gateway, log)

	// 7a. Approval gate (#392 phase 1): every new or changed task passes the
	// trust-on-change gate before its triggers arm. Approval records live in
	// dicode.lock next to dicode.yaml; trust policy lives in cfg.Approval.
	approvalGate, err := setupApprovalGate(ctx, cfg, configPath, database, secretsChain, eng, denoRT, pythonRT, rec, arm, disarm, log)
	if err != nil {
		return err
	}

	// 8. Web UI (including the 8.5 relay env exports).
	srv, err := buildWebUI(ctx, cfg, configPath, version, dataDir, database, reg, eng, localSecrets, rec, sourceMgr, gateway, logBroadcaster, managedRuntimes, approvalGate, replayer, log)
	if err != nil {
		return err
	}

	// 8.6. Ephemeral per-run MCP token minter: sweep tokens orphaned by a
	// previous daemon session, then wire the minter into both runtimes so
	// `permissions.env: [DICODE_MCP_API_KEY]` tasks get one auto-revoked
	// per-run key instead of `dicode mcp install`'s operator-managed key.
	if err := wireMCPTokenMinter(ctx, srv, denoRT, pythonRT, log); err != nil {
		return err
	}

	// 9. Control socket for CLI clients.
	ctrlSrv, err := buildControlServer(cfg, dataDir, version, database, reg, rec, eng, localSecrets, srv, sourceMgr, approvalGate, log)
	if err != nil {
		return err
	}
	wireCryptoIPC(secretsChain, denoRT, log)

	// 9.5. Audit-log retention (#45).
	auditStore, auditRetentionDays := initAuditPruning(ctx, cfg, database, log)

	// 10. Run everything concurrently.
	return runServices(ctx, rec, eng, srv, ctrlSrv, reg, auditStore, auditRetentionDays, log)
}

// openDatabase opens the daemon's database from the config (step 1).
func openDatabase(cfg *config.Config) (db.DB, error) {
	database, err := db.Open(db.Config{
		Type:   cfg.Database.Type,
		Path:   cfg.Database.Path,
		URLEnv: cfg.Database.URLEnv,
	})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	return database, nil
}

// setupRegistry cleans up container/run state left over from a previous
// session and builds the task registry (step 4).
func setupRegistry(ctx context.Context, database db.DB, log *zap.Logger) *registry.Registry {
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
	// Cancel suspended runs whose resume deadline lapsed while the daemon was
	// down (#95); the periodic sweep in runServices handles the steady state.
	if swept, err := reg.SweepExpiredSuspensions(ctx, time.Now().UnixMilli()); err != nil {
		log.Warn("resume-deadline sweep failed at startup", zap.Error(err))
	} else if len(swept) > 0 {
		log.Info("cancelled suspended runs past resume deadline at startup", zap.Strings("runs", swept))
	}
	return reg
}

// wireRunInputPersistence wires the run-input persistence InputStore into the
// engine and both runtimes when enabled (step 6a, Task 14). Returns the
// Replayer composing InputStore.Fetch + the engine's fireAsync — wired after
// the InputStore so dicode.runs.replay finds a populated store — or nil when
// persistence is disabled or unavailable. srv.SetReplayer is called by run()
// after webui is built.
func wireRunInputPersistence(cfg *config.Config, secretsChain secrets.Chain, reg *registry.Registry, eng *trigger.Engine, denoRT *denoruntime.Runtime, pythonRT *pythonruntime.Runtime, log *zap.Logger) *registry.Replayer {
	if !cfg.Defaults.RunInputs.IsEnabled() {
		log.Info("run-input persistence disabled by config")
		return nil
	}
	var deriver secrets.SubKeyDeriver
	for _, p := range secretsChain {
		if d, ok := p.(secrets.SubKeyDeriver); ok {
			deriver = d
			break
		}
	}
	if deriver == nil {
		log.Warn("run-input persistence: no SubKeyDeriver available in secrets chain — persistence disabled")
		return nil
	}
	key, err := deriver.DeriveSubKey("dicode/run-inputs/v1")
	if err != nil {
		log.Warn("run-input persistence: sub-key derive failed", zap.Error(err))
		return nil
	}
	runner := trigger.NewInputStoreTaskRunner(eng)
	is := registry.NewInputStore(registry.NewInputCrypto(key), runner, cfg.Defaults.RunInputs.StorageTask)
	eng.SetInputStore(is)
	denoRT.SetInputStore(is)
	pythonRT.SetInputStore(is)
	// Replayer composes InputStore.Fetch + the engine's fireAsync.
	// Wired after InputStore so dicode.runs.replay finds a populated store.
	replayer := registry.NewReplayer(reg, is, trigger.NewReplayRunner(eng))
	denoRT.SetReplayer(replayer)
	pythonRT.SetReplayer(replayer)
	log.Info("run-input persistence enabled",
		zap.Duration("retention", cfg.Defaults.RunInputs.Retention),
		zap.String("storage_task", cfg.Defaults.RunInputs.StorageTask),
	)
	return replayer
}

// initSources builds the task sources + source manager, wires them into both
// runtimes, and creates the reconciler (step 7).
func initSources(cfg *config.Config, dataDir string, reg *registry.Registry, denoRT *denoruntime.Runtime, pythonRT *pythonruntime.Runtime, log *zap.Logger) (*webui.SourceManager, *registry.Reconciler, error) {
	sources, sourceMgr, err := buildSources(cfg, dataDir, log)
	if err != nil {
		return nil, nil, fmt.Errorf("build sources: %w", err)
	}
	// *webui.SourceManager satisfies both SourceDevModeSetter (Task 6) and
	// RepoPathResolver (Task 8); wire both interfaces into both runtimes so
	// per-run IPC servers can serve set_dev_mode and git.commit_push.
	denoRT.SetSourceManager(sourceMgr)
	denoRT.SetRepoResolver(sourceMgr)
	pythonRT.SetSourceManager(sourceMgr)
	pythonRT.SetRepoResolver(sourceMgr)
	rec := registry.NewReconciler(reg, sources, dataDir, log)
	return sourceMgr, rec, nil
}

// newArmDisarm builds the arm/disarm pair the approval gate drives (step 7).
func newArmDisarm(cfg *config.Config, eng *trigger.Engine, gateway *ipc.Gateway, log *zap.Logger) (arm func(task.Kinded) error, disarm func(string)) {
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
	arm = func(k task.Kinded) error {
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
		if isSpec && gateRelayServerBody(spec, cfg) {
			log.Info("relay-server-body disabled — relay not configured (set relay.enabled + relay.server_url in dicode.yaml to run it)",
				zap.String("task", spec.ID))
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
		if spec.Trigger.WebhookAuth.RequiresSession() && !cfg.Server.Auth {
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
	disarm = func(id string) {
		webhookMu.Lock()
		path := webhookPaths[id]
		delete(webhookPaths, id)
		webhookMu.Unlock()
		if path != "" {
			gateway.Unregister(path)
		}
		eng.Unregister(id)
	}
	return arm, disarm
}

// setupApprovalGate builds the trust-on-change approval gate (step 7a, #392
// phase 1): protected-path denies, the signed dicode.lock, the bootstrap
// marker/window handling, the engine/runtime fire-guard wiring, and the
// reconciler's OnRegister/OnUnregister hooks that drive arm/disarm.
func setupApprovalGate(ctx context.Context, cfg *config.Config, configPath string, database db.DB, secretsChain secrets.Chain, eng *trigger.Engine, denoRT *denoruntime.Runtime, pythonRT *pythonruntime.Runtime, rec *registry.Reconciler, arm func(task.Kinded) error, disarm func(string), log *zap.Logger) (*approval.Gate, error) {
	configDir, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		return nil, fmt.Errorf("resolve config dir: %w", err)
	}
	lockPath := filepath.Join(configDir, approval.LockFileName)
	// Approval-gate state must never be writable by a task. A task with a broad
	// fs-write grant covering the config dir could otherwise overwrite
	// dicode.lock to self-approve other pending tasks; deny-write on these paths
	// overrides any such allow. Paths are canonicalised (symlinks resolved) so
	// the deny set matches the form a task's write resolves to — a config dir
	// reached via a symlink would otherwise carry a deny entry that never
	// matches the canonical write path.
	protectedPaths := []string{canonicalPath(lockPath)}
	if absConfigPath, err := filepath.Abs(configPath); err == nil {
		protectedPaths = append(protectedPaths, canonicalPath(absConfigPath))
	}
	denoRT.SetProtectedPaths(protectedPaths)
	pythonRT.SetProtectedPaths(protectedPaths)
	_, lockStatErr := os.Stat(lockPath)
	lockExisted := lockStatErr == nil
	dbMarkerExists, err := bootstrapMarkerExists(ctx, database)
	if err != nil {
		return nil, fmt.Errorf("read approval bootstrap marker: %w", err)
	}
	// Derive the lock-signing key from the master key so the HMAC key is
	// never task-reachable and survives across restarts even if the SQLite DB
	// is wiped. Silently falls back to unsigned mode when no SubKeyDeriver is
	// available (e.g. stripped test builds without a local provider).
	var lockSigningKey []byte
	for _, p := range secretsChain {
		if d, ok := p.(secrets.SubKeyDeriver); ok {
			if k, kerr := d.DeriveSubKey("dicode/approval-lock/v1"); kerr == nil {
				lockSigningKey = k
			} else {
				log.Warn("approval lock: sub-key derivation failed — falling back to unsigned mode",
					zap.Error(kerr))
			}
			break
		}
	}
	if lockSigningKey == nil {
		log.Warn("approval lock running in unsigned mode — sub-key derivation unavailable; " +
			"lock file integrity cannot be verified")
	}
	lock, err := approval.LoadSignedLock(lockPath, lockSigningKey)
	if err != nil {
		return nil, fmt.Errorf("load approval lock: %w", err)
	}
	if lock.Tampered() {
		log.Warn("approval lock HMAC verification failed — lock may be tampered or forged; "+
			"all approval records discarded, tasks require explicit re-approval",
			zap.String("lock", lockPath))
	}
	// The effective bootstrap marker is the union of the DB kv row and the lock's
	// own bootstrapped flag. Both must be absent to re-enter bootstrap, so deleting
	// either alone (the SQLite DB or the lock file) cannot reset the gate (#434).
	markerExists := lock.IsBootstrapped() || dbMarkerExists
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

	// First-run bootstrap: on a genuine first run (no lock, no completed-marker)
	// seed the existing inventory as approved instead of stranding every task
	// behind a gate that has no approve UI yet. The settle timer is armed
	// immediately — not on the first registration — so the window also closes
	// on an install with zero tasks; otherwise the first task to ever appear
	// would be auto-approved. Each registration during the window slides it
	// forward (tolerates a slow first git clone). FinishBootstrap is idempotent,
	// so the timer firing is always safe. The completed-marker is set when the
	// window closes; a crash mid-bootstrap leaves the marker unset, so the next
	// start re-bootstraps rather than stranding the inventory.
	//
	// Lock-loss after a prior run must fail closed: the marker present with the
	// lock absent means the approval state was deleted/lost (a broad-fs task can
	// remove it by a vector the file-level deny does not cover). Re-entering
	// bootstrap then would re-seed the attacker's pending change as approved —
	// the #402 escalation — so bootstrap is skipped and the inventory is held
	// pending for explicit re-approval.
	//
	// An existing lock proves first-run is over, so the marker MUST track it:
	// without this backfill an adopted lock (operator-shipped/restored, or one
	// written before a crash interrupted the first bootstrap) would leave the
	// marker unset, and a later lock-deletion would satisfy shouldBootstrap and
	// re-seed as approved. Backfilling keeps "lock present ⇒ marker present" so
	// the fail-closed branch above always fires after the lock vanishes.
	var bootstrapTimer *time.Timer
	if shouldBootstrap(lockExisted, markerExists, gatePolicy.Enabled) {
		approvalGate.SetBootstrap(true)
		bootstrapTimer = time.AfterFunc(bootstrapSettle, func() {
			if approvalGate.FinishBootstrap() {
				// Persist the bootstrap marker in both the lock and the DB.
				// Lock marker: survives DB deletion (the primary attack vector).
				// DB marker: survives lock deletion (fallback / directory-level attacks).
				if err := lock.MarkBootstrapped(); err != nil {
					log.Warn("approval gate: failed to persist bootstrap marker in lock; lock-only DB deletion may re-bootstrap on next start",
						zap.Error(err))
				}
				if err := setBootstrapMarker(ctx, database); err != nil {
					log.Warn("approval gate: failed to persist bootstrap marker; next start may re-bootstrap",
						zap.Error(err))
				}
				log.Info("approval gate active — subsequent task changes require approval")
			}
		})
		log.Info("approval gate: no dicode.lock — seeding current tasks as approved (first-run bootstrap); changes after startup require approval",
			zap.String("lock", lockPath))
	} else if gatePolicy.Enabled && lockExisted && !markerExists {
		if err := lock.MarkBootstrapped(); err != nil {
			log.Warn("approval gate: failed to persist bootstrap marker in lock for an adopted lock; "+
				"a DB deletion could re-enable bootstrap",
				zap.Error(err))
		}
		if err := setBootstrapMarker(ctx, database); err != nil {
			log.Warn("approval gate: failed to persist bootstrap marker for an adopted lock; "+
				"a later lock-loss may fail open rather than closed",
				zap.Error(err))
		}
	} else if gatePolicy.Enabled && !lockExisted && markerExists {
		log.Warn("approval gate: dicode.lock is missing despite a prior run — failing closed; "+
			"all changed tasks require explicit re-approval. If the lock was not removed deliberately, "+
			"this may indicate tampering by a task with a broad filesystem grant",
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
	return approvalGate, nil
}

// buildWebUI exports the relay-related env vars and builds the web UI server
// with its approval / replay wiring (steps 8 + 8.5).
func buildWebUI(ctx context.Context, cfg *config.Config, configPath, version, dataDir string, database db.DB, reg *registry.Registry, eng *trigger.Engine, localSecrets secrets.Manager, rec *registry.Reconciler, sourceMgr *webui.SourceManager, gateway *ipc.Gateway, logBroadcaster *webui.LogBroadcaster, managedRuntimes []pkgruntime.ManagedRuntime, approvalGate *approval.Gate, replayer *registry.Replayer, log *zap.Logger) (*webui.Server, error) {
	port := cfg.Server.Port
	if port == 0 {
		port = 8080
	}

	// 8.5. Export relay-related config to process env so the buildin tasks
	// (relay-client, auth-start, auth-relay) can read them via Deno.env.get.
	// These tasks declare the var names in permissions.env; Deno's allow-env
	// flag exposes them to the task subprocess.
	if err := os.Setenv("DICODE_DATADIR", dataDir); err != nil {
		return nil, fmt.Errorf("setenv DICODE_DATADIR: %w", err)
	}
	if cfg.Defaults.RunInputs.StorageTask != "" {
		if err := os.Setenv("DICODE_STORAGE_TASK", cfg.Defaults.RunInputs.StorageTask); err != nil {
			return nil, fmt.Errorf("setenv DICODE_STORAGE_TASK: %w", err)
		}
	}
	if relayConfigured(cfg) {
		if err := os.Setenv("DICODE_RELAY_SERVER_URL", cfg.Relay.ServerURL); err != nil {
			return nil, fmt.Errorf("setenv DICODE_RELAY_SERVER_URL: %w", err)
		}
		if err := os.Setenv("DICODE_RELAY_LOCAL_PORT", fmt.Sprintf("%d", port)); err != nil {
			return nil, fmt.Errorf("setenv DICODE_RELAY_LOCAL_PORT: %w", err)
		}
		if brokerURL := cfg.Relay.ResolvedBrokerURL(); brokerURL != "" {
			if err := os.Setenv("DICODE_RELAY_BROKER_URL", brokerURL); err != nil {
				return nil, fmt.Errorf("setenv DICODE_RELAY_BROKER_URL: %w", err)
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
		return nil, fmt.Errorf("build webui: %w", err)
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
	// Notify on suspend (and on a suspended conversation's end) so the operator
	// gets pinged to come answer a paused agent (#584). Composes with the
	// WebUI's run:finished broadcast via AddRunFinishedHook; a no-op when
	// ai.notify_task is unset.
	suspendNotify := suspendNotifier{
		notifyTask: cfg.AI.NotifyTask,
		resumeURL:  func(runID string) string { return srv.WebUIBaseURL() + "/?run=" + runID },
		fire: func(id string, params map[string]string) error {
			_, err := eng.FireManual(ctx, id, params)
			return err
		},
		log: log,
	}
	eng.AddRunFinishedHook(suspendNotify.onRunFinished)

	if replayer != nil {
		srv.SetReplayer(replayer)
	}
	// The engine's ResumeRun backs the suspended-run resume endpoint (#95).
	srv.SetResumer(eng)
	return srv, nil
}

// buildControlServer builds the control socket for CLI clients and wires its
// capabilities: API-key minting, the approval-gate test/approve surfaces, AI
// task authoring, and task deletion (step 9).
func buildControlServer(cfg *config.Config, dataDir, version string, database db.DB, reg *registry.Registry, rec *registry.Reconciler, eng *trigger.Engine, localSecrets secrets.Manager, srv *webui.Server, sourceMgr *webui.SourceManager, approvalGate *approval.Gate, log *zap.Logger) (*ipc.ControlServer, error) {
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
		return nil, fmt.Errorf("build control server: %w", err)
	}
	// Wire API-key minting so `dicode mcp install` and friends can
	// auto-generate keys via the control socket. The webui Server holds
	// the apiKeyStore; control just dispatches.
	ctrlSrv.SetAPIKeyMinter(srv)

	// Approval-gate veto for `dicode task test` — same guard as the engine's
	// fire paths and the REST test endpoint.
	ctrlSrv.SetTestGuard(approvalGate.FireGuard)

	// Readiness barrier (#464): the control socket comes up before the
	// reconciler's first sync has registered the initial task inventory, so a
	// CLI command racing daemon startup could observe a spurious "task not
	// found". cli.ready lets task-scoped CLI commands block (bounded) until
	// the first sync completes. The channel never closes if ctx is cancelled
	// mid-sync; the IPC handler's own ctx/timeout select keeps shutdown clean.
	ctrlSrv.SetReadySignal(rec.Ready())

	// `dicode task approve` — the control socket is a trusted local channel.
	ctrlSrv.SetTaskApprover(approvalGate.Approve)

	// `dicode task pending` + `dicode list` annotation — surface the tasks the
	// gate is holding so a headless operator can discover an id to approve.
	ctrlSrv.SetPendingApprovals(func() []ipc.PendingTask {
		ids := approvalGate.Pending()
		out := make([]ipc.PendingTask, 0, len(ids))
		for _, id := range ids {
			hash, _ := approvalGate.PendingHash(id)
			out = append(out, ipc.PendingTask{TaskID: id, Hash: hash})
		}
		return out
	})

	// Wire AI-first task authoring so `dicode task create|edit|save|cancel`
	// reuses the same source manager and author_sessions store the REST
	// handlers use.
	ctrlSrv.SetAuthoringService(srv)

	// Wire task deletion so `dicode task delete` can remove a task from its
	// source. The SourceManager owns source state, repo paths, and dev-clones.
	ctrlSrv.SetTaskDeleter(sourceMgr)

	// Wire suspended-run resume so `dicode resume` reaches Engine.ResumeRun.
	ctrlSrv.SetResumer(resumerAdapter{eng: eng})
	return ctrlSrv, nil
}

// resumerAdapter maps the trigger engine's typed resume errors onto the ipc
// sentinels so the control handler can classify them — pkg/ipc cannot import
// pkg/trigger (trigger imports ipc), so the translation lives here.
type resumerAdapter struct{ eng *trigger.Engine }

func (a resumerAdapter) ResumeRun(ctx context.Context, token string, input []byte) (string, error) {
	id, err := a.eng.ResumeRun(ctx, token, input)
	switch {
	case errors.Is(err, trigger.ErrResumeTokenNotFound):
		return "", ipc.ErrResumeTokenNotFound
	case errors.Is(err, trigger.ErrResumeNotSuspended):
		return "", ipc.ErrResumeNotSuspended
	case errors.Is(err, trigger.ErrResumeExpired):
		return "", ipc.ErrResumeExpired
	case errors.Is(err, trigger.ErrResumePending):
		return "", ipc.ErrResumePending
	}
	return id, err
}

// wireCryptoIPC wires the generic dicode.crypto.{encrypt, decrypt} IPC verb
// so buildin tasks (relay-client, auth-start, auth-relay) can encrypt/decrypt
// blobs via DeriveSubKey-derived sub-keys. Sub-keys never cross IPC; only the
// encrypt/decrypt operations are exposed.
func wireCryptoIPC(secretsChain secrets.Chain, denoRT *denoruntime.Runtime, log *zap.Logger) {
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

// wireMCPTokenMinter sweeps ephemeral per-run MCP API keys orphaned by a
// previous daemon session (a run in flight when the daemon last stopped
// leaves its token behind — nothing will ever call Revoke for it) and then
// wires the minter into both runtimes so a run whose spec declares
// permissions.env DICODE_MCP_API_KEY gets a fresh key at start and has it
// revoked at end, instead of relying on a static `dicode mcp install` key.
// srv.MCPTokenMinter() is nil when no database is configured (tests); the
// static-secret path then still works unchanged.
func wireMCPTokenMinter(ctx context.Context, srv *webui.Server, denoRT *denoruntime.Runtime, pythonRT *pythonruntime.Runtime, log *zap.Logger) error {
	if err := srv.SweepEphemeralMCPTokens(ctx); err != nil {
		return fmt.Errorf("sweep orphaned ephemeral MCP tokens: %w", err)
	}
	if minter := srv.MCPTokenMinter(); minter != nil {
		denoRT.SetMCPTokenMinter(minter)
		pythonRT.SetMCPTokenMinter(minter)
	} else {
		log.Debug("ephemeral MCP token minter not wired (no database) — DICODE_MCP_API_KEY falls back to the secrets chain")
	}
	return nil
}

// initAuditPruning builds the audit store and runs the startup prune pass
// (step 9.5, #45). Event emission is wired implicitly — the trigger engine
// (SetDB), per-run IPC servers, and webui all build their own audit.Store
// over the shared database handle. The daemon only owns pruning: once at
// startup here, then periodically in runServices. retention 0 (explicit
// opt-out) disables pruning entirely; Prune itself also refuses to run with
// a non-positive window.
func initAuditPruning(ctx context.Context, cfg *config.Config, database db.DB, log *zap.Logger) (*audit.Store, int) {
	auditStore := audit.NewStore(database)
	auditRetentionDays := cfg.AuditLog.EffectiveRetentionDays()
	if auditRetentionDays > 0 {
		if err := auditStore.Prune(ctx, auditRetentionDays); err != nil {
			log.Warn("audit log prune failed", zap.Error(err))
		}
		log.Info("audit log retention enabled", zap.Int("retention_days", auditRetentionDays))
	} else {
		log.Info("audit log pruning disabled (audit_log.retention_days: 0)")
	}
	return auditStore, auditRetentionDays
}

// runServices runs every daemon loop concurrently until the context is
// cancelled or one loop fails (step 10): periodic audit pruning, the
// reconciler, the trigger engine, the web UI, the control server, and the
// container image GC.
func runServices(ctx context.Context, rec *registry.Reconciler, eng *trigger.Engine, srv *webui.Server, ctrlSrv *ipc.ControlServer, reg *registry.Registry, auditStore *audit.Store, auditRetentionDays int, log *zap.Logger) error {
	g, ctx := errgroup.WithContext(ctx)
	if auditRetentionDays > 0 {
		g.Go(func() error {
			ticker := time.NewTicker(6 * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-ticker.C:
					if err := auditStore.Prune(ctx, auditRetentionDays); err != nil {
						log.Warn("audit log prune failed", zap.Error(err))
					}
				}
			}
		})
	}
	// Resume-deadline sweep (#95): cancel suspended runs whose resume_deadline
	// has passed, recording ReasonResumeTimeout. Runs on a ticker so a run that
	// suspends and is never resumed doesn't linger indefinitely. Routed through
	// the engine so each cancellation fires the run:finished hook and its
	// resume_timeout chain, matching a normal cancellation's finish side-effects.
	g.Go(func() error {
		ticker := time.NewTicker(resumeSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				swept, err := eng.SweepExpiredSuspensions(ctx, time.Now().UnixMilli())
				if err != nil {
					log.Warn("resume-deadline sweep failed", zap.Error(err))
				} else if len(swept) > 0 {
					log.Info("cancelled suspended runs past resume deadline", zap.Strings("runs", swept))
				}
			}
		}
	})
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
