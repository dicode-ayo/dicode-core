// Package trigger manages cron schedules, webhook dispatch, manual fires,
// chain reactions, and daemon (always-on) tasks.
package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dicode/dicode/pkg/audit"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/runtime/envresolve"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

// ErrRunNotFound is returned by WaitRun when no run record exists for the given ID.
var ErrRunNotFound = errors.New("run not found")

// runReturnValueTTL is how long a suppressed-persistence return value
// stays in the in-memory runReturnValue map after the run reaches a
// terminal state. Long enough for a WaitRun caller woken by the runDone
// close to scan the map; short enough that orphaned entries (no waiter)
// don't accumulate. Only `run_result.enabled: false` tasks populate the
// map, so this is a small footprint either way.
const runReturnValueTTL = 30 * time.Second

// Engine coordinates all trigger types and fires task runs.
type Engine struct {
	registry  *registry.Registry
	executors map[task.Runtime]pkgruntime.Executor
	cron      *cron.Cron
	log       *zap.Logger
	audit     *audit.Store // best-effort audit emission; nil-safe, wired by SetDB

	mu                 sync.Mutex
	cronEntries        map[string]cron.EntryID // taskID → cron entry
	webhooks           map[string]string       // webhook path → taskID
	webhookReplayCache *replayCache

	// fireGuard, when set, can veto any run before it starts (approval gate,
	// #392). Checked in startRun — the chokepoint every kind: Task run passes
	// through regardless of trigger path — and in fireKinded for an early,
	// pipeline-covering rejection on manual fires. Tasks held pending never
	// arm cron/webhook/daemon triggers, but manual / chain / replay /
	// pipeline-stage paths resolve tasks from the registry, so they need
	// this veto.
	fireGuardMu sync.RWMutex
	fireGuard   func(taskID string) error

	// registerMu serializes the entire Register path so that concurrent
	// registrations cannot admit a cycle through interleaved snapshots of
	// the pipeline-stage graph (registerPipeline's validatePipelineRefs
	// scans the current registry state; without a mutex two parallel
	// Register calls could each clear cycle detection against their
	// independent snapshots and jointly close A→B→A). Held across
	// validation through the final Unregister/register* mutations.
	// Separate from `mu` because Unregister itself takes `mu`, and from
	// `daemonMu` for the same reason — keeping registerMu coarse-but-shallow
	// avoids re-entrance.
	//
	// In practice the reconciler is single-threaded so registrations
	// don't race today; this is defense-in-depth for future callers.
	registerMu sync.Mutex

	// deferredPipelines holds kind: PipelineTask registrations that were
	// rejected because a stage task was not yet in the registry — the
	// cold-start ordering case where the reconciler registers a pipeline
	// before its stages (#341). Such a pipeline is kept here (keyed by ID)
	// instead of erroring; when a kind: Task later registers successfully,
	// Register replays the deferred set, so a pipeline self-heals once its
	// stages exist — without needing a file change to trigger re-reconcile.
	// Guarded by registerMu (same lock that serialises the whole Register
	// path), so retries can't race a concurrent registration. Entries are
	// dropped on success and on Unregister(id) so a removed/superseded
	// pipeline is never resurrected.
	deferredPipelines map[string]*task.PipelineTask

	runCancels       sync.Map // runID → context.CancelFunc
	runDone          sync.Map // runID (string) → chan struct{}, closed when the run reaches a terminal state
	runTriggerSource sync.Map // runID (string) → triggerSource (registry.TriggerSource)
	runChainDepth    sync.Map // runID (string) → int; _chain_depth from the run's input

	// runReturnValue holds JSON-marshalled return values for runs whose task
	// spec set `run_result.enabled: false`. The value is NOT written to the
	// `runs.return_value` column in those cases, so WaitRun would otherwise
	// see an empty value and break the synchronous dicode.run_task contract.
	//
	// Entries are added by dispatch right after marshalling and deleted by
	// startRun's cleanup func via a time.AfterFunc that fires several
	// seconds after the run reaches a terminal state — long enough for any
	// WaitRun caller woken by the runDone close to scan the map before
	// the entry disappears.
	//
	// Common case (persistence enabled) leaves this map untouched — the
	// DB row carries the value as before, so there is no memory cost for
	// regular tasks.
	//
	// Keys: runID (string). Values: string (JSON-marshalled return value).
	runReturnValue sync.Map

	shutdownMu  sync.RWMutex
	shutdownCtx context.Context

	daemonMu    sync.Mutex
	daemonRuns  map[string]string
	daemonSpecs map[string]*task.Spec

	// daemonStates tracks the lifecycle phase of each daemon task for
	// surfacing in the WebUI (Engine.DaemonState). Independent of daemonMu —
	// guarded by its own RWMutex so a state-read in the API path never has to
	// wait on a long daemon dispatch holding daemonMu.
	daemonStates *daemonStateMap

	// daemonBackoffs tracks the current restart backoff duration for each
	// daemon task (keyed as "daemon-backoff:<taskID>"). Persists the backoff
	// value across restarts within a single daemon lifetime so exponential
	// growth is monotone. Values are time.Duration; nil entry means "use init".
	daemonBackoffs sync.Map

	// crashloops counts consecutive quick daemon-body failures per task so a
	// crash-looping daemon reports DaemonCrashLooping instead of the transient
	// "running" of a spawn that is about to die (issue #458). In-memory only,
	// like daemonBackoffs. See crashloop.go.
	crashloops *crashloopTracker

	// restartGates is a per-daemon at-most-one-in-flight lock for daemon
	// restarts. See daemon_state.go for the coalescing rationale. Also reused
	// by handlePipelineStageRerun, keyed by pipeline ID, so a flurry of
	// mid-pipeline stage re-fires coalesces to at most one outstanding
	// propagation per pipeline.
	restartGates *restartGate

	// livePipelines tracks PipelineRunners whose terminal daemon stage is
	// currently running — i.e. the pipeline parent run is 'running' for the
	// daemon's lifetime. Keyed by pipeline task ID. handlePipelineStageRerun
	// consults this to find which live pipelines contain a just-completed stage
	// task and need their descendants replayed + daemon restarted. Guarded by
	// livePipelineMu (separate from daemonMu so a stage-rerun scan never blocks
	// on a long daemon dispatch holding daemonMu).
	livePipelineMu sync.Mutex
	livePipelines  map[string]*PipelineRunner

	defaultsOnFailureChain task.OnFailureChainSpec // from config.Defaults.OnFailureChain

	db db.DB // optional — enables cron-job persistence and missed-run catchup

	secrets      secrets.Chain      // optional — enables if_missing prereq resolution at dispatch time
	prereqFlight singleflight.Group // collapses concurrent prereq runs keyed on secret name, so parallel webhook calls with the same missing secret don't each spawn a duplicate prereq (OAuth flow, refresh-token rotation, etc.)

	taskSem     chan struct{} // nil = unlimited; capacity = MaxConcurrentTasks
	taskWaiting atomic.Int64  // goroutines parked waiting for a semaphore slot
	started     atomic.Bool   // set to true by Start(); guards SetMaxConcurrentTasks

	runFinishedHook func(taskID, runID, status, triggerSource string, durationMs int64)
	runStartedHook  func(taskID, runID, triggerSource string)

	// denoRuntime / pythonRuntime are typed runtime handles needed by the
	// Engine's ProviderRunner implementation (issue #119). The engine swaps
	// the per-runtime SecretOutputChannel per provider invocation. Wired in
	// daemon.go via SetDenoRuntime / SetPythonRuntime to avoid an import
	// cycle with the runtime packages.
	denoRuntime   DenoRuntimeAPI
	pythonRuntime PythonRuntimeAPI

	// envResolver is the daemon-scoped env resolver shared across all task
	// launches. The cache lives inside this instance, so cross-launch TTL
	// hits work as intended (issue #242). Constructed lazily by Resolver().
	envResolver *envresolve.Resolver

	// providerRunMu serializes Engine.Run invocations so concurrent provider
	// resolutions don't clobber each other's secretOutputCh on the runtime.
	// MVP-quality: a per-run channel registry would allow parallelism but
	// requires runtime changes; revisit when contention shows up.
	providerRunMu sync.Mutex

	// inputStore persists run inputs at run-start (Task 10). nil = disabled.
	// Set via SetInputStore after the daemon has initialised secrets so the
	// derived sub-key is available.
	inputStore *registry.InputStore

	guards *chainGuards
}

// DenoRuntimeAPI is the minimal subset of *deno.Runtime the engine's
// ProviderRunner implementation depends on. Defined here (not imported)
// to keep pkg/trigger free of pkg/runtime/deno; daemon.go wires the real
// runtime via SetDenoRuntime.
type DenoRuntimeAPI interface {
	SetSecretOutputChannel(ch chan map[string]string)
}

// PythonRuntimeAPI is the minimal subset of *python.Runtime the engine's
// ProviderRunner implementation depends on. Mirrors DenoRuntimeAPI.
type PythonRuntimeAPI interface {
	SetSecretOutputChannel(ch chan map[string]string)
}

// New creates a trigger Engine with a default Deno executor.
func New(r *registry.Registry, defaultExec pkgruntime.Executor, log *zap.Logger) *Engine {
	e := &Engine{
		registry:           r,
		executors:          make(map[task.Runtime]pkgruntime.Executor),
		cron:               cron.New(),
		log:                log,
		cronEntries:        make(map[string]cron.EntryID),
		webhooks:           make(map[string]string),
		webhookReplayCache: newReplayCache(1 * time.Hour),
		daemonRuns:         make(map[string]string),
		daemonSpecs:        make(map[string]*task.Spec),
		daemonStates:       newDaemonStateMap(),
		crashloops:         newCrashloopTracker(),
		restartGates:       newRestartGate(),
		livePipelines:      make(map[string]*PipelineRunner),
		deferredPipelines:  make(map[string]*task.PipelineTask),
		guards:             newChainGuards(),
	}
	e.executors[task.RuntimeDeno] = defaultExec
	return e
}

// SetDB wires a database into the engine for cron-job persistence.
// When set, the engine persists each cron task's next scheduled time and
// detects missed runs on startup (e.g. after a process restart).
func (e *Engine) SetDB(d db.DB) {
	e.db = d
	// Audit emission piggybacks on the same handle — one wiring point keeps
	// the #45 footprint minimal. NewStore(nil) → nil → Emit is a no-op.
	e.audit = audit.NewStore(d)
}

// SetSecrets wires the secrets chain into the engine. Required for the
// `env[].if_missing` prereq mechanism: before dispatching a task, the engine
// consults the chain to check whether each if_missing-guarded secret is
// present, and runs the declared prereq task when it isn't.
func (e *Engine) SetSecrets(s secrets.Chain) {
	e.secrets = s
}

// SetDenoRuntime wires the deno runtime so the engine can act as a
// ProviderRunner — swapping the per-run SecretOutputChannel before
// firing a provider task and clearing it after.
func (e *Engine) SetDenoRuntime(r DenoRuntimeAPI) { e.denoRuntime = r }

// SetPythonRuntime wires the python runtime; mirror of SetDenoRuntime.
func (e *Engine) SetPythonRuntime(r PythonRuntimeAPI) { e.pythonRuntime = r }

// SetInputStore wires the InputStore so every run's input is persisted at
// run-start. Called by the daemon after secrets are available (so the derived
// sub-key exists). When nil (the default), input persistence is a no-op.
func (e *Engine) SetInputStore(s *registry.InputStore) { e.inputStore = s }

// SetFireGuard installs a veto consulted before any run starts (see the
// fireGuard field doc). A nil guard removes the veto.
func (e *Engine) SetFireGuard(g func(taskID string) error) {
	e.fireGuardMu.Lock()
	e.fireGuard = g
	e.fireGuardMu.Unlock()
}

// checkFireGuard returns the guard's veto for taskID, or nil when no guard
// is installed.
func (e *Engine) checkFireGuard(taskID string) error {
	e.fireGuardMu.RLock()
	g := e.fireGuard
	e.fireGuardMu.RUnlock()
	if g == nil {
		return nil
	}
	return g(taskID)
}

// Resolver returns the daemon-scoped env resolver, constructing it lazily on
// first call. The resolver's TTL cache survives across task launches so that
// provider.cache_ttl actually provides cross-fire benefit (issue #242).
func (e *Engine) Resolver() *envresolve.Resolver {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.envResolver == nil {
		if e.secrets == nil {
			e.log.Warn("Resolver() called before SetSecrets() — resolver will have no secrets chain")
		}
		e.envResolver = envresolve.New(e.registry, e.secrets, e)
	}
	return e.envResolver
}

// SetMaxConcurrentTasks configures a semaphore that limits how many task
// goroutines run concurrently. n == 0 (the default) means unlimited.
// Must be called before Start(); calls after Start() are ignored with a warning.
func (e *Engine) SetMaxConcurrentTasks(n int) {
	if e.started.Load() {
		e.log.Warn("SetMaxConcurrentTasks called after Start — ignoring; configure before Start()")
		return
	}
	if n < 0 {
		e.log.Warn("SetMaxConcurrentTasks: negative value treated as unlimited", zap.Int("n", n))
		n = 0
	}
	const maxConcurrentTasksLimit = 10000
	if n > maxConcurrentTasksLimit {
		e.log.Warn("SetMaxConcurrentTasks: value exceeds maximum, capping",
			zap.Int("requested", n), zap.Int("cap", maxConcurrentTasksLimit))
		n = maxConcurrentTasksLimit
	}
	if n > 0 {
		e.taskSem = make(chan struct{}, n)
	} else {
		e.taskSem = nil
	}
}

// MaxConcurrentTasks returns the configured concurrency cap, or 0 if unlimited.
func (e *Engine) MaxConcurrentTasks() int {
	if e.taskSem == nil {
		return 0
	}
	return cap(e.taskSem)
}

// ActiveTaskSlots returns the number of semaphore slots currently held by
// running task goroutines. Returns 0 when no cap is configured.
func (e *Engine) ActiveTaskSlots() int {
	if e.taskSem == nil {
		return 0
	}
	return len(e.taskSem)
}

// WaitingTasks returns the number of task goroutines currently parked waiting
// for a semaphore slot to free. Always 0 when no cap is configured.
func (e *Engine) WaitingTasks() int {
	return int(e.taskWaiting.Load())
}

// SetRunStartedHook registers a callback invoked when a run starts.
func (e *Engine) SetRunStartedHook(fn func(taskID, runID, triggerSource string)) {
	e.runStartedHook = fn
}

// SetRunFinishedHook registers a callback invoked after every run completes.
// Called from the goroutine that ran the task, so the hook must be non-blocking
// (e.g. send to a buffered channel).
func (e *Engine) SetRunFinishedHook(fn func(taskID, runID, status, triggerSource string, durationMs int64)) {
	e.runFinishedHook = fn
}

// SetDefaultsOnFailureChain sets the global on_failure_chain to fire when any task fails.
// The caller is expected to have run ValidateAtDefaults() on the spec; the
// daemon does this via cfg.validate() at config-load time. A direct caller
// that bypasses cfg.validate() must call ValidateAtDefaults manually.
// Returns an error if the spec violates ValidateAtDefaults rules
// (reserved-key collision, autonomous-at-defaults).
func (e *Engine) SetDefaultsOnFailureChain(spec task.OnFailureChainSpec) error {
	if err := spec.ValidateAtDefaults(); err != nil {
		return err
	}
	e.defaultsOnFailureChain = spec
	return nil
}

// ActiveRunCount returns the number of task runs currently in progress.
func (e *Engine) ActiveRunCount() int {
	n := 0
	e.runCancels.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// RegisterExecutor registers an executor for the given runtime name.
// Call this before Start to wire in Docker, subprocess, or custom runtimes.
func (e *Engine) RegisterExecutor(rt task.Runtime, exec pkgruntime.Executor) {
	e.mu.Lock()
	e.executors[rt] = exec
	e.mu.Unlock()
}

// Start begins scheduling and runs until ctx is cancelled.
func (e *Engine) Start(ctx context.Context) error {
	e.started.Store(true)
	e.shutdownMu.Lock()
	e.shutdownCtx = ctx
	e.shutdownMu.Unlock()

	// Check for missed cron runs BEFORE registering tasks: registration overwrites
	// next_run_at with a fresh future value, so the catchup must read the prior
	// session's persisted next_run_at first.
	if e.db != nil {
		e.catchupMissedCronRuns(ctx)
	}

	for _, spec := range e.registry.All() {
		e.Register(spec)
	}
	e.cron.Start()

	<-ctx.Done()
	e.cron.Stop()

	e.daemonMu.Lock()
	killList := make(map[string]string, len(e.daemonRuns))
	for k, v := range e.daemonRuns {
		killList[k] = v
	}
	e.daemonMu.Unlock()

	for taskID, runID := range killList {
		e.log.Info("stopping daemon on shutdown", zap.String("task", taskID), zap.String("run", runID))
		e.KillRun(runID)
	}
	return nil
}

// Register adds or updates trigger registrations for the given task, routing by
// kind: a *task.PipelineTask is validated via registerPipeline; a *task.Spec
// follows the cron/webhook/daemon registration path. The call is serialized via
// registerMu so concurrent registrations cannot admit a cycle through
// interleaved registry snapshots. Returns a non-nil error if validation fails
// (e.g. pipeline stage ref/cycle errors); on error, no triggers are modified.
// Callers may pass any task.Kinded;
// *task.Spec satisfies it, so existing call sites are unaffected.
func (e *Engine) Register(k task.Kinded) error {
	e.registerMu.Lock()
	defer e.registerMu.Unlock()

	switch s := k.(type) {
	case *task.PipelineTask:
		return e.registerPipeline(s)
	case *task.Spec:
		// We already hold registerMu, so use the non-locking teardown and drop
		// any deferred-pipeline entry for this ID inline (registerMu guards it).
		// In practice a Spec ID won't be in deferredPipelines (that map only
		// holds pipelines), but the delete is a cheap no-op safeguard if a
		// pipeline is ever replaced in-place by a Task of the same ID.
		delete(e.deferredPipelines, s.ID)
		e.unregisterTriggers(s.ID)

		// Disabled tasks are kept in the registry for API visibility but must not
		// be scheduled, spawned as daemons, or registered as webhook endpoints.
		if !s.Enabled {
			e.log.Info("task registered (disabled — no triggers scheduled)",
				zap.String("task", s.ID),
				zap.String("runtime", string(s.Runtime)),
			)
			return nil
		}

		// Cycle guard for success-chain edges (Fix 1, #387): before arming
		// triggers, verify that registering s does not close a cycle in the
		// trigger.chain graph. A cycle would cause A→B→A→… to loop forever
		// without the depth-cap that protects on_failure_chain. We do this
		// check while holding registerMu so a concurrent registration cannot
		// sneak in a second half of a cycle between our check and our commit.
		if s.Trigger.Chain != nil {
			on := s.Trigger.Chain.ChainOn()
			if on == "success" || on == "always" {
				if e.hasSuccessChainCycle(s.ID, s.Trigger.Chain.From) {
					e.log.Error("task registration rejected: success-chain cycle detected",
						zap.String("task", s.ID),
						zap.String("chains_from", s.Trigger.Chain.From),
						zap.String("hint", "A→B→A loops in trigger.chain would run forever; break the cycle"),
					)
					return fmt.Errorf("task %q: trigger.chain creates a success-chain cycle via %q", s.ID, s.Trigger.Chain.From)
				}
			}
		}

		if s.Trigger.Cron != "" {
			e.registerCron(s)
		}
		if s.Trigger.Webhook != "" {
			e.registerWebhook(s)
		}
		if s.Trigger.Daemon {
			e.registerDaemon(s)
		}
		e.log.Info("task registered",
			zap.String("task", s.ID),
			zap.String("trigger", string(triggerSource(s))),
			zap.String("runtime", string(s.Runtime)),
		)
		// A kind: Task just landed — it may be the stage a deferred pipeline was
		// waiting on (cold-start ordering, #341). Retry the deferred set so such
		// a pipeline schedules itself without needing a file change. Runs under
		// registerMu (held for the whole Register path), which also guards
		// deferredPipelines.
		e.retryDeferredPipelines()
		return nil
	default:
		return fmt.Errorf("engine: unsupported task kind %q", k.KindOf())
	}
}

// Unregister removes all trigger registrations for a task ID and drops any
// deferred-pipeline entry for it, so a pipeline that was parked waiting for its
// stages (cold-start ordering, #341) is never resurrected after removal.
//
// This is the public entry point used by the reconciler's OnUnregister. It must
// NOT be called while holding registerMu; the internal Register path uses
// unregisterTriggers instead (it already holds registerMu and drops the
// deferred entry inline).
func (e *Engine) Unregister(id string) {
	e.registerMu.Lock()
	delete(e.deferredPipelines, id)
	e.registerMu.Unlock()
	e.unregisterTriggers(id)
}

// unregisterTriggers removes all trigger registrations (cron, webhook, daemon)
// for a task ID. Split out from Unregister so the Register path can reuse it
// while already holding registerMu without deadlocking on a re-entrant lock.
// Does NOT touch deferredPipelines (the caller handles that under registerMu).
func (e *Engine) unregisterTriggers(id string) {
	e.mu.Lock()
	hadCron := false
	if entryID, ok := e.cronEntries[id]; ok {
		e.cron.Remove(entryID)
		delete(e.cronEntries, id)
		hadCron = true
	}
	for path, tid := range e.webhooks {
		if tid == id {
			delete(e.webhooks, path)
		}
	}
	e.mu.Unlock()

	if hadCron && e.db != nil {
		if dbErr := e.db.Exec(context.Background(), `DELETE FROM cron_jobs WHERE task_id=?`, id); dbErr != nil {
			e.log.Warn("cron: failed to delete cron_jobs row on unregister",
				zap.String("task", id), zap.Error(dbErr))
		}
	}

	e.daemonMu.Lock()
	delete(e.daemonSpecs, id)
	runID := e.daemonRuns[id]
	delete(e.daemonRuns, id)
	e.daemonMu.Unlock()

	// Unregistration wipes crash-loop tracking (#458): a removed (or
	// reloaded-with-new-content) task starts with a fresh failure counter,
	// mirroring how daemonStates entries don't outlive the registration.
	e.crashloops.reset(id)

	if runID != "" {
		e.log.Info("stopping daemon — task unregistered", zap.String("task", id), zap.String("run", runID))
		e.KillRun(runID)
	}
	e.log.Info("task unregistered", zap.String("task", id))
}

// cronNextRun parses expr and returns the next scheduled time after now.
func cronNextRun(expr string) (time.Time, error) {
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(time.Now()), nil
}

func (e *Engine) registerCron(spec *task.Spec) {
	e.scheduleCron(spec.ID, spec.Trigger.Cron, func() error {
		s, ok := e.registry.Get(spec.ID)
		if !ok {
			return fmt.Errorf("cron task %q gone from registry", spec.ID)
		}
		_, ferr := e.fireAsync(context.Background(), s, pkgruntime.RunOptions{}, registry.TriggerCron)
		return ferr
	})
}

// registerPipelineCron schedules a kind: PipelineTask on its cron expression,
// firing the PipelineRunner via fireKinded. Mirrors registerCron's scheduling
// through the shared scheduleCron primitive.
func (e *Engine) registerPipelineCron(p *task.PipelineTask) {
	e.scheduleCron(p.ID, p.Trigger.Cron, func() error {
		k, ok := e.registry.GetKinded(p.ID)
		if !ok {
			return fmt.Errorf("cron pipeline %q gone from registry", p.ID)
		}
		_, ferr := e.fireKinded(context.Background(), k, pkgruntime.RunOptions{}, registry.TriggerCron)
		return ferr
	})
}

// scheduleCron registers a cron entry that runs fire() on each tick, records the
// entry under id (kind-agnostic — keyed by task ID), and persists the cron_jobs
// row. next_run_at is advanced only when fire() succeeds, so a failed dispatch
// doesn't silently skip the missed run on the next restart. Shared by kind: Task
// (registerCron) and kind: PipelineTask (registerPipelineCron).
func (e *Engine) scheduleCron(id, cronExpr string, fire func() error) {
	entryID, err := e.cron.AddFunc(cronExpr, func() {
		if ferr := fire(); ferr == nil && e.db != nil {
			if next, nerr := cronNextRun(cronExpr); nerr == nil {
				if dbErr := e.db.Exec(context.Background(),
					`UPDATE cron_jobs SET last_run_at=?, next_run_at=? WHERE task_id=?`,
					time.Now().Unix(), next.Unix(), id,
				); dbErr != nil {
					e.log.Warn("cron: failed to persist next_run_at",
						zap.String("task", id), zap.Error(dbErr))
				}
			}
		}
	})
	if err != nil {
		e.log.Error("invalid cron expression",
			zap.String("task", id),
			zap.String("cron", cronExpr),
			zap.Error(err),
		)
		return
	}
	e.mu.Lock()
	e.cronEntries[id] = entryID
	e.mu.Unlock()

	if e.db != nil {
		if next, nerr := cronNextRun(cronExpr); nerr == nil {
			if dbErr := e.db.Exec(context.Background(),
				`INSERT INTO cron_jobs(task_id,cron_expr,next_run_at) VALUES(?,?,?)
				 ON CONFLICT(task_id) DO UPDATE SET cron_expr=excluded.cron_expr, next_run_at=excluded.next_run_at`,
				id, cronExpr, next.Unix(),
			); dbErr != nil {
				e.log.Warn("cron: failed to persist cron_jobs row",
					zap.String("task", id), zap.Error(dbErr))
			}
		}
	}
}

// catchupMissedCronRuns fires any cron tasks whose next_run_at is in the past,
// up to a 24-hour cutoff. Called at startup before tasks are re-registered.
//
// Fire-once semantics: at most one catchup run is fired per task per restart,
// regardless of how many intervals were missed. This prevents bulk-firing a
// high-frequency task after a long outage. Operators can see the skipped count
// in the Warn log entries for rows older than 24h.
func (e *Engine) catchupMissedCronRuns(ctx context.Context) {
	now := time.Now().Unix()
	cutoff := time.Now().Add(-24 * time.Hour).Unix()

	// Remove rows for tasks no longer in the registry (deleted while daemon was offline).
	allSpecs := e.registry.All()
	knownIDs := make([]any, 0, len(allSpecs))
	for _, s := range allSpecs {
		knownIDs = append(knownIDs, s.ID)
	}
	if len(knownIDs) > 0 {
		placeholders := strings.Repeat("?,", len(knownIDs))
		placeholders = placeholders[:len(placeholders)-1]
		if dbErr := e.db.Exec(ctx,
			`DELETE FROM cron_jobs WHERE task_id NOT IN (`+placeholders+`)`,
			knownIDs...,
		); dbErr != nil {
			e.log.Warn("cron catchup: failed to prune orphaned rows", zap.Error(dbErr))
		}
	} else {
		if dbErr := e.db.Exec(ctx, `DELETE FROM cron_jobs`); dbErr != nil {
			e.log.Warn("cron catchup: failed to prune orphaned rows", zap.Error(dbErr))
		}
	}

	type missedRow struct {
		taskID string
		nextAt int64
	}
	var missed, tooOld []missedRow

	if queryErr := e.db.Query(ctx,
		`SELECT task_id, next_run_at FROM cron_jobs WHERE next_run_at < ?`,
		[]any{now},
		func(rows db.Scanner) error {
			for rows.Next() {
				var r missedRow
				if err := rows.Scan(&r.taskID, &r.nextAt); err == nil {
					if r.nextAt > cutoff {
						missed = append(missed, r)
					} else {
						tooOld = append(tooOld, r)
					}
				}
			}
			return nil
		},
	); queryErr != nil {
		e.log.Warn("cron catchup: failed to query missed runs", zap.Error(queryErr))
		return
	}

	for _, m := range tooOld {
		e.log.Warn("cron catchup: missed run is older than 24h — skipping",
			zap.String("task", m.taskID),
			zap.Time("was_due", time.Unix(m.nextAt, 0)),
		)
	}
	for _, m := range missed {
		spec, ok := e.registry.Get(m.taskID)
		if !ok {
			e.log.Warn("cron catchup: task no longer registered, skipping",
				zap.String("task", m.taskID),
				zap.Time("was_due", time.Unix(m.nextAt, 0)),
			)
			continue
		}
		e.log.Info("cron catchup: firing missed run",
			zap.String("task", m.taskID),
			zap.Time("was_due", time.Unix(m.nextAt, 0)),
		)
		e.fireAsync(ctx, spec, pkgruntime.RunOptions{}, registry.TriggerCronCatchup) //nolint:errcheck
	}
}

func (e *Engine) isShuttingDown() bool {
	e.shutdownMu.RLock()
	ctx := e.shutdownCtx
	e.shutdownMu.RUnlock()
	return ctx != nil && ctx.Err() != nil
}

func (e *Engine) getShutdownCtx() context.Context {
	e.shutdownMu.RLock()
	defer e.shutdownMu.RUnlock()
	return e.shutdownCtx
}

// FireManual triggers a task by ID with optional param overrides.
func (e *Engine) FireManual(ctx context.Context, taskID string, params map[string]string) (string, error) {
	return e.FireManualWithActor(ctx, taskID, params, "")
}

// FireManualWithActor is FireManual carrying the identity of the operator
// principal (e.g. the authenticated web session's client IP) that requested
// the run. The actor flows into the run_triggered audit event (#45) as
// actor_id; an empty actor preserves plain FireManual behaviour.
func (e *Engine) FireManualWithActor(ctx context.Context, taskID string, params map[string]string, actor string) (string, error) {
	k, ok := e.registry.GetKinded(taskID)
	if !ok {
		return "", fmt.Errorf("task %q not found", taskID)
	}
	e.log.Info("manual trigger", zap.String("task", taskID))
	return e.fireKinded(context.Background(), k, pkgruntime.RunOptions{Params: params, TriggerActor: actor}, registry.TriggerManual)
}

// FireFromTask triggers a task as a child of an in-flight run. Used by the
// dicode.run_task IPC handler so the new run's parent_run_id (#116) points
// at the caller. Falls back to a plain manual fire when parentRunID is "".
func (e *Engine) FireFromTask(ctx context.Context, taskID, parentRunID string, params map[string]string) (string, error) {
	if parentRunID == "" {
		return e.FireManual(ctx, taskID, params)
	}
	k, ok := e.registry.GetKinded(taskID)
	if !ok {
		return "", fmt.Errorf("task %q not found", taskID)
	}
	e.log.Info("subtask trigger", zap.String("task", taskID), zap.String("parent", parentRunID))
	return e.fireKinded(context.Background(), k,
		pkgruntime.RunOptions{Params: params, ParentRunID: parentRunID},
		registry.TriggerManual)
}

// WaitRun blocks until the run identified by runID reaches a terminal state,
// then returns a RunResult. Implements ipc.EngineRunner.
//
// Channel lifecycle: startRun() registers a chan struct{} in runDone keyed by
// runID. The cleanup func (deferred in fireAsync/fireSync via startRun) closes
// that channel once the run goroutine finishes. WaitRun selects on the channel
// so it is woken up immediately rather than polling.
//
// Race: if WaitRun is called after the channel has already been closed and
// deleted (i.e. the run finished before the caller reached this function), the
// Load will return ok==false. In that case we fall through to a single DB read
// to return the final status.
func (e *Engine) WaitRun(ctx context.Context, runID string) (ipc.RunResult, error) {
	if v, ok := e.runDone.Load(runID); ok {
		// Run is in progress — wait for the completion channel to be closed.
		select {
		case <-v.(chan struct{}):
			// Channel closed: run has finished. Fall through to DB read below.
		case <-ctx.Done():
			return ipc.RunResult{}, ctx.Err()
		}
	}
	// Either the channel was never present (run already finished before we
	// arrived) or it was just closed. Either way, fetch the final record.
	run, err := e.registry.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, registry.ErrRunNotFound) {
			return ipc.RunResult{}, ErrRunNotFound
		}
		return ipc.RunResult{}, err
	}
	// Prefer the persisted return_value when present. Fall back to the
	// in-memory runReturnValue cache for tasks that opted out of
	// persistence via `run_result.enabled: false` — without this fallback,
	// `dicode.run_task` callers would receive nil for those tasks even
	// though the value is available in process. The cache entry survives
	// for runReturnValueTTL after run completion (see startRun cleanup).
	returnJSON := run.ReturnValue
	if returnJSON == "" {
		if v, ok := e.runReturnValue.Load(runID); ok {
			returnJSON, _ = v.(string)
		}
	}
	var returnValue interface{}
	if returnJSON != "" {
		_ = json.Unmarshal([]byte(returnJSON), &returnValue)
	}
	return ipc.RunResult{
		RunID:       runID,
		Status:      run.Status,
		ReturnValue: returnValue,
	}, nil
}

// Run satisfies envresolve.ProviderRunner. It spawns the provider task
// synchronously and waits for it to finish; the secret map is collected
// over the IPC channel pre-wired into the runtime by SetSecretOutputChannel.
//
// Concurrency: serialized through providerRunMu because the runtime's
// secretOutputCh is single-slot global state. MVP-quality — see
// providerRunMu doc on the Engine struct.
//
// Errors:
//   - ctx.Err() if the caller context expires
//   - error if the spawn fails or the run errors out
//   - error if the run finished without sending a map (provider didn't
//     call output(..., {secret: true}))
func (e *Engine) Run(ctx context.Context, providerID string, reqs []envresolve.ProviderRequest) (*envresolve.ProviderResult, error) {
	spec, ok := e.registry.Get(providerID)
	if !ok {
		return nil, fmt.Errorf("provider task %q not registered", providerID)
	}

	e.providerRunMu.Lock()
	defer e.providerRunMu.Unlock()

	ch := make(chan map[string]string, 1)
	switch spec.Runtime {
	case task.RuntimeDeno, "", "js":
		if e.denoRuntime == nil {
			return nil, fmt.Errorf("deno runtime not wired to engine")
		}
		e.denoRuntime.SetSecretOutputChannel(ch)
		defer e.denoRuntime.SetSecretOutputChannel(nil)
	default:
		if e.pythonRuntime == nil {
			return nil, fmt.Errorf("python runtime not wired to engine (runtime=%q)", spec.Runtime)
		}
		e.pythonRuntime.SetSecretOutputChannel(ch)
		defer e.pythonRuntime.SetSecretOutputChannel(nil)
	}

	reqJSON, _ := json.Marshal(reqs)
	runID, err := e.fireAsync(ctx, spec, pkgruntime.RunOptions{
		Params: map[string]string{"requests": string(reqJSON)},
	}, "provider")
	if err != nil {
		return nil, fmt.Errorf("fire provider %q: %w", providerID, err)
	}
	res, werr := e.WaitRun(ctx, runID)
	if werr != nil {
		return nil, fmt.Errorf("wait provider %q: %w", providerID, werr)
	}
	if res.Status != registry.StatusSuccess {
		return nil, fmt.Errorf("provider %q run %s: %s", providerID, runID, res.Status)
	}

	// The buffered (cap=1) channel was populated when the IPC server
	// observed the dicode.output(..., {secret:true}) call — by the time
	// WaitRun returns success the value is already enqueued. A short
	// non-blocking read with a tiny safety timeout diagnoses providers
	// that completed without ever calling output(secret).
	select {
	case sm := <-ch:
		return &envresolve.ProviderResult{Values: sm}, nil
	case <-time.After(50 * time.Millisecond):
		return nil, fmt.Errorf("provider %q completed without secret output (did it call dicode.output(map, { secret: true })?)", providerID)
	}
}

// preflightEnv runs the env resolver once before dispatch so that typed
// provider failures (provider_unavailable / required_secret_missing /
// provider_misconfigured) can be recorded as the run's fail_reason
// instead of surfacing as opaque dispatch errors.
//
// On success, it returns the *Resolved so dispatch can hand it to the
// runtime via RunOptions.PreResolvedEnv, ensuring provider tasks fire
// exactly once per consumer launch instead of twice (issue #235).
//
// Return contract:
//   - success: (resolved, "", "")
//   - typed envresolve failure: (nil, registry.StatusFailure, "<reason>")
//   - non-typed error or skipped (no secrets chain / no env entries):
//     (nil, "", "") — dispatch proceeds and the runtime resolves inline.
func (e *Engine) preflightEnv(ctx context.Context, spec *task.Spec) (*envresolve.Resolved, string, string) {
	// Skip preflight when secrets chain isn't wired (test fixtures) or
	// when the spec has no env entries the resolver could fail on.
	if e.secrets == nil || len(spec.Permissions.Env) == 0 {
		return nil, "", ""
	}
	resolved, err := e.Resolver().Resolve(ctx, spec)
	if err != nil {
		var pu *envresolve.ErrProviderUnavailable
		var rsm *envresolve.ErrRequiredSecretMissing
		var mis *envresolve.ErrProviderMisconfigured
		switch {
		case errors.As(err, &pu):
			return nil, registry.StatusFailure, "provider_unavailable: " + pu.ProviderID
		case errors.As(err, &rsm):
			return nil, registry.StatusFailure, "required_secret_missing: " + rsm.Key + " from " + rsm.ProviderID
		case errors.As(err, &mis):
			return nil, registry.StatusFailure, "provider_misconfigured: " + mis.ProviderID
		}
		// Non-typed error: log for operator visibility (without the error
		// detail, which may contain secret key names), then let dispatch
		// surface it through the runtime's inline resolver path.
		e.log.Warn("preflight env-resolve returned non-typed error — falling through to inline resolution",
			zap.String("task", spec.ID))
		return nil, "", ""
	}
	return resolved, "", ""
}

// KillRun cancels a running task by its run ID.
func (e *Engine) KillRun(runID string) bool {
	v, ok := e.runCancels.Load(runID)
	if !ok {
		return false
	}
	e.log.Info("run kill requested", zap.String("run", runID))
	v.(context.CancelFunc)()
	return true
}

// startRun creates the DB record, stores the cancel func, fires the started
// hook, and returns a ready-to-run context. The caller is responsible for
// calling the returned cleanup func when the run finishes.
// startRunWithParent is like startRun but accepts an explicit parent context
// for the run's cancellation context. The run is cancelled when either the
// parent context expires or KillRun is called. Pass context.Background() to
// reproduce the original independent-context behaviour.
func (e *Engine) startRunWithParent(parent context.Context, spec *task.Spec, opts *pkgruntime.RunOptions, source registry.TriggerSource) (runCtx context.Context, cleanup func(), err error) {
	if err = e.checkFireGuard(spec.ID); err != nil {
		return nil, nil, err
	}
	if _, err = e.registry.StartRunWithID(context.Background(), opts.RunID, spec.ID, opts.ParentRunID, string(source), registry.RunKindTask); err != nil {
		return nil, nil, fmt.Errorf("start run record: %w", err)
	}

	actorID := opts.TriggerActor
	if actorID == "" {
		actorID = opts.ParentRunID
	}
	e.audit.Emit(context.Background(), audit.Event{
		EventType:  audit.EventRunTriggered,
		ActorKind:  string(source),
		ActorID:    actorID,
		TargetKind: "task",
		TargetID:   spec.ID,
		Params:     audit.SanitizeParams(opts.Params),
		RunID:      opts.RunID,
		Allowed:    true,
	})

	if e.inputStore != nil && e.shouldPersistInput(spec) {
		var web *registry.WebhookFields
		if opts.WebhookCtx != nil {
			bft := false
			if spec.RunInputs != nil && spec.RunInputs.BodyFullTextual != nil {
				bft = *spec.RunInputs.BodyFullTextual
			}
			web = &registry.WebhookFields{
				Method:          opts.WebhookCtx.Method,
				Path:            opts.WebhookCtx.Path,
				Headers:         opts.WebhookCtx.Headers,
				Query:           opts.WebhookCtx.Query,
				RawBody:         opts.WebhookCtx.RawBody,
				ContentType:     opts.WebhookCtx.ContentType,
				BodyFullTextual: bft,
			}
		}
		in := registry.BuildPersistedInputFromRunOpts(string(source), opts.Params, opts.Input, web)
		key, size, storedAt, perr := e.inputStore.Persist(context.Background(), opts.RunID, in)
		if perr != nil {
			e.log.Warn("run-input persist failed",
				zap.String("run", opts.RunID),
				zap.String("task", spec.ID),
				zap.String("error_class", "persist"),
			)
		} else {
			if opts.WebhookCtx != nil {
				// Bound RAM exposure: RawBody is no longer needed now that the
				// blob has been persisted. Nil it out so the slice can be GC'd
				// rather than held for the full run lifetime.
				opts.WebhookCtx.RawBody = nil
			}
			if serr := e.registry.SetRunInput(context.Background(), opts.RunID, key, size, storedAt, in.RedactedFields); serr != nil {
				e.log.Warn("run-input set columns failed",
					zap.String("run", opts.RunID),
					zap.String("task", spec.ID),
					zap.Error(serr),
				)
			}
		}
	}

	if h := e.runStartedHook; h != nil {
		h(spec.ID, opts.RunID, string(source))
	}
	var cancel context.CancelFunc
	runCtx, cancel = context.WithCancel(parent)
	e.runCancels.Store(opts.RunID, cancel)
	e.runTriggerSource.Store(opts.RunID, source)

	doneCh := make(chan struct{})
	e.runDone.Store(opts.RunID, doneCh)

	cleanup = func() {
		if opts.ChainParentTask != "" {
			e.guards.releaseSlot(opts.ChainParentTask)
		}
		e.runCancels.Delete(opts.RunID)
		e.runTriggerSource.Delete(opts.RunID)
		e.runChainDepth.Delete(opts.RunID)
		cancel()
		if v, ok := e.runDone.LoadAndDelete(opts.RunID); ok {
			close(v.(chan struct{}))
		}
		// Defer deletion of the suppressed-persistence return-value cache:
		// WaitRun goroutines woken by the runDone close above need time to
		// scan runReturnValue before the entry is removed. The map is only
		// populated for `run_result.enabled: false` tasks, so this AfterFunc
		// is a no-op for the common case.
		runID := opts.RunID
		time.AfterFunc(runReturnValueTTL, func() {
			e.runReturnValue.Delete(runID)
		})
	}
	return runCtx, cleanup, nil
}

// startRun creates a run record and context rooted at context.Background() —
// the standard path for all trigger types. See startRunWithParent for the
// variant that wires an explicit parent (used by fireSync so prereq timeouts
// propagate correctly).
func (e *Engine) startRun(spec *task.Spec, opts *pkgruntime.RunOptions, source registry.TriggerSource) (runCtx context.Context, cleanup func(), err error) {
	return e.startRunWithParent(context.Background(), spec, opts, source)
}

// shouldPersistInput returns true when the run's input should be persisted.
// It enforces two recursion guards and respects the per-task opt-out flag.
func (e *Engine) shouldPersistInput(spec *task.Spec) bool {
	// Per-task opt-out: run_inputs.enabled: false in task.yaml.
	if spec.RunInputs != nil && spec.RunInputs.Enabled != nil && !*spec.RunInputs.Enabled {
		return false
	}
	// Recursion guard: never persist the storage task's own inputs.
	if e.inputStore != nil && spec.ID == e.inputStore.StorageTaskID() {
		return false
	}
	// Recursion guard: cleanup task runs periodically with the same retention
	// config; persisting it every cron tick is pointless and loops.
	if spec.ID == "buildin/run-inputs-cleanup" {
		return false
	}
	return true
}

// runTask executes a task synchronously and handles all post-run bookkeeping
// (logging, notifications, hooks, daemon restart). Returns status and result.
func (e *Engine) runTask(runCtx context.Context, spec *task.Spec, opts pkgruntime.RunOptions, source registry.TriggerSource) (string, *pkgruntime.RunResult) {
	e.log.Info("run started",
		zap.String("task", spec.ID),
		zap.String("run", opts.RunID),
		zap.String("trigger", string(source)),
		zap.String("runtime", string(spec.Runtime)),
	)

	start := time.Now()

	// Preflight env-resolver: typed envresolve failures
	// (provider_unavailable / required_secret_missing / provider_misconfigured)
	// are recorded as the run's fail_reason BEFORE dispatch so the operator
	// gets a categorized reason instead of an opaque executor error.
	//
	// On preflight success the *Resolved is forwarded to the runtime via
	// opts.PreResolvedEnv so provider tasks fire exactly once per consumer
	// launch (issue #235). When preflight is skipped (no secrets chain /
	// no env entries) preResolved is nil and the runtime falls back to its
	// inline-resolver path.
	var status string
	var result *pkgruntime.RunResult
	preResolved, preStatus, preReason := e.preflightEnv(runCtx, spec)
	if preStatus != "" {
		if err := e.registry.FinishRunWithReason(context.Background(), opts.RunID, preStatus, preReason); err != nil {
			e.log.Warn("FinishRun: preflight failure", zap.String("run", opts.RunID), zap.Error(err))
		}
		// Chain-on-failure semantics: dispatch normally fires FireChain;
		// the preflight-env short-circuit replicates it so chain triggers
		// and on_failure_chain still observe the failure.
		//
		// Called synchronously so the caller's deferred cleanup() (which
		// deletes runChainDepth[opts.RunID]) cannot race ahead of FireChain's
		// depth lookup. An earlier draft used `go FireChain(...)` to mirror a
		// fire-and-forget shape, but that allowed cleanup to observe a
		// depth-of-zero and let a chain-fired parent take one hop past the
		// MaxDepth ceiling (issue #334, sister to #331). Matches the normal
		// FireChain call site at the end of dispatch (~line 2759), which is
		// also synchronous.
		e.FireChain(context.Background(), spec.ID, opts.RunID, preStatus, nil, opts.Params)
		status = preStatus
		result = &pkgruntime.RunResult{RunID: opts.RunID, Error: errors.New(preReason)}
	} else {
		opts.PreResolvedEnv = preResolved
		status, result = e.dispatch(runCtx, spec, opts)
	}
	elapsed := time.Since(start)

	runFields := []zap.Field{
		zap.String("task", spec.ID),
		zap.String("run", opts.RunID),
		zap.String("status", status),
		zap.String("trigger", string(source)),
		zap.Duration("duration", elapsed.Truncate(time.Millisecond)),
	}
	if status == registry.StatusSuccess {
		e.log.Debug("run finished", runFields...)
	} else {
		e.log.Warn("run finished", runFields...)
	}

	if h := e.runFinishedHook; h != nil {
		h(spec.ID, opts.RunID, status, string(source), elapsed.Milliseconds())
	}

	// Drive the standalone-daemon lifecycle (restart-policy decision,
	// DaemonState transition) only for runs that the engine actually manages as
	// standalone daemons. A pipeline's terminal stage is dispatched as a
	// daemon-kind run too (source=pipeline-stage), but it is owned by the
	// PipelineRunner, not the standalone-daemon machinery: its lifetime,
	// restarts, and DaemonState are not tracked in daemonRuns/daemonStates.
	// Routing a pipeline-stage daemon run through onDaemonRunFinished would,
	// when the stage task ALSO happens to be a registered standalone daemon,
	// flip that standalone daemon's global DaemonState and possibly schedule an
	// unintended startDaemon on a mid-pipeline restart's KillRun (#344, LOW
	// security). Suppress the hook for pipeline-stage runs. This gate depends on
	// fireStageRaw always dispatching pipeline stages with TriggerPipelineStage
	// (see the INVARIANT note at pipeline_runner.go:fireStageRaw's fireAsync).
	if spec.Trigger.Daemon && source != registry.TriggerPipelineStage {
		e.onDaemonRunFinished(spec, opts.RunID)
	}

	return status, result
}

// finalizeCancelled closes out a run that was cancelled before ever executing
// — e.g. user killed it while queued on the concurrency semaphore, or the
// daemon is shutting down. It updates the registry row and fires the finished
// hook so websocket subscribers see a matching started/finished pair.
// Safe to call even on the hot shutdown path: the DB write is bounded by a
// short timeout so a stuck SQLite connection cannot block the goroutine.
func (e *Engine) finalizeCancelled(spec *task.Spec, opts pkgruntime.RunOptions, source registry.TriggerSource) {
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer finishCancel()
	if err := e.registry.FinishRun(finishCtx, opts.RunID, registry.StatusCancelled); err != nil {
		e.log.Error("failed to finalize cancelled queued run",
			zap.String("run", opts.RunID),
			zap.String("task", spec.ID),
			zap.Error(err))
	}
	if h := e.runFinishedHook; h != nil {
		// Duration is 0 — the run never executed.
		h(spec.ID, opts.RunID, registry.StatusCancelled, string(source), 0)
	}
}

// fireAsync pre-creates the run record, starts execution in a goroutine,
// and returns the run ID immediately.
//
// When a MaxConcurrentTasks semaphore is configured, the goroutine blocks
// until a slot is available or the shutdown context is cancelled — ensuring
// shutdown never deadlocks waiting tasks.
//
// fireKinded dispatches any task kind, returning the (parent) run ID: a
// *task.PipelineTask runs via the PipelineRunner (firePipeline); a *task.Spec
// runs via fireAsync. Trigger entrypoints (manual/cron/webhook/chain) route
// through here so a pipeline and a plain task fire uniformly.
func (e *Engine) fireKinded(ctx context.Context, k task.Kinded, opts pkgruntime.RunOptions, source registry.TriggerSource) (string, error) {
	if err := e.checkFireGuard(k.TaskID()); err != nil {
		return "", err
	}
	switch s := k.(type) {
	case *task.PipelineTask:
		return e.firePipeline(ctx, s, opts, source)
	case *task.Spec:
		return e.fireAsync(ctx, s, opts, source)
	default:
		return "", fmt.Errorf("engine: cannot fire unsupported kind %q", k.KindOf())
	}
}

func (e *Engine) fireAsync(ctx context.Context, spec *task.Spec, opts pkgruntime.RunOptions, source registry.TriggerSource) (string, error) {
	opts.RunID = uuid.New().String()

	runCtx, cleanup, err := e.startRun(spec, &opts, source)
	if err != nil {
		return "", err
	}

	// If this run carries a _chain_depth in its input (set by FireChain), record
	// it in runChainDepth so FireChain can read the depth when this run completes.
	if m, ok := opts.Input.(map[string]any); ok {
		if d, ok2 := m["_chain_depth"]; ok2 {
			switch v := d.(type) {
			case int:
				e.runChainDepth.Store(opts.RunID, v)
			case int64:
				e.runChainDepth.Store(opts.RunID, int(v))
			case float64:
				e.runChainDepth.Store(opts.RunID, int(v))
			}
		}
	}

	go func() {
		defer cleanup()

		// Daemon tasks are long-running; they must not consume semaphore slots
		// or they would permanently starve webhook/cron tasks.
		if e.taskSem != nil && !spec.Trigger.Daemon {
			// Build a done channel that is nil (blocks forever) when the engine
			// has not yet started — nil channels never select, which is safe.
			var shutDone <-chan struct{}
			shutCtx := e.getShutdownCtx()
			if shutCtx != nil {
				shutDone = shutCtx.Done()
			}

			// Increment the "parked waiting for a slot" counter. Each select
			// case below must decrement exactly once: "waiting" ends when
			// either the slot is acquired or the goroutine aborts — NOT
			// when the run itself finishes. A single deferred decrement
			// here would wrongly keep the counter inflated for the entire
			// lifetime of successfully-acquired runs.
			e.taskWaiting.Add(1)
			select {
			case e.taskSem <- struct{}{}:
				e.taskWaiting.Add(-1)
				// Slot acquired; release when done.
				defer func() { <-e.taskSem }()
			case <-runCtx.Done():
				// User killed (or gave up on) the queued run before a slot
				// freed. Finalize it as cancelled so the websocket finished
				// event fires and the DB row doesn't stay stuck in `running`.
				e.taskWaiting.Add(-1)
				e.finalizeCancelled(spec, opts, source)
				return
			case <-shutDone:
				// Engine is shutting down; finalize as cancelled and abort.
				e.taskWaiting.Add(-1)
				e.finalizeCancelled(spec, opts, source)
				return
			}
		}

		e.runTask(runCtx, spec, opts, source)
	}()

	return opts.RunID, nil
}

// fireSync runs the task synchronously and returns the run ID and result.
// The caller's context is used only for cancellation of the run setup; the
// run itself uses an independent context so it is not cancelled when the HTTP
// request context ends mid-execution.
//
// When callerCtx is non-nil, it is used as the parent of the run context so
// a deadline on callerCtx (e.g. the prereq ceiling from resolveIfMissing)
// propagates to the run. Pass context.Background() to preserve the old
// independent-context behaviour.
func (e *Engine) fireSync(callerCtx context.Context, spec *task.Spec, opts pkgruntime.RunOptions, source registry.TriggerSource) (string, *pkgruntime.RunResult, error) {
	opts.RunID = uuid.New().String()

	runCtx, cleanup, err := e.startRunWithParent(callerCtx, spec, &opts, source)
	if err != nil {
		return "", nil, err
	}
	defer cleanup()

	status, result := e.runTask(runCtx, spec, opts, source)
	if result == nil {
		result = &pkgruntime.RunResult{}
	}
	if result.Error == nil && status != registry.StatusSuccess {
		result.Error = fmt.Errorf("run %s", status)
	}
	return opts.RunID, result, nil
}

// ifMissingPrereqTimeout bounds how long resolveIfMissing will wait for a
// single prereq task to complete. The prereq's own spec-level Timeout is
// respected first; this is an engine-level ceiling so a wedged prereq (slow
// DNS, stuck upstream, etc.) can't block the webhook caller indefinitely.
const ifMissingPrereqTimeout = 60 * time.Second

// ifMissingErrorFormat is the prefix used by the errors resolveIfMissing
// returns when a prereq is missing/failed. UI consumers (notably
// tasks/buildin/ai-agent/chat.js `detectSetup`) regex-match this prefix
// to render a "Set up <Provider>" card instead of a raw error log.
//
// STABLE INTERFACE — do not rephrase without updating every consumer.
//
//	if_missing: secret %q requires setup via task %q[: wrapped-error]
//	if_missing: secret %q requires setup via task %q (ran but still unset)[: wrapped-error]
const ifMissingErrorFormat = `if_missing: secret %q requires setup via task %q`

// resolveIfMissing scans the task's env entries for `if_missing` directives
// and, for any whose target secret is not present in the secrets chain,
// synchronously runs the declared prereq task in chain mode. If the prereq
// succeeds and the secret is now resolvable, dispatch continues normally.
// If the prereq fails, its error — typically an OAuth flow's "open this URL
// to authorize" message — bubbles up as the original task's failure so the
// UI can surface a setup call-to-action (see ifMissingErrorFormat).
//
// Concurrency: parallel calls with the same secret are collapsed via
// singleflight, so N webhook calls with the same missing secret yield one
// prereq run, not N. The shared result is returned to all callers — each
// then re-resolves the secret independently to decide whether to proceed.
//
// Timeout: the prereq's own spec Timeout applies first; an engine-level
// ifMissingPrereqTimeout (60s) bounds total wait to protect the HTTP caller
// even if a misconfigured prereq declared a higher timeout.
//
// No-ops when: secrets chain is not wired, the entry has no `if_missing`
// directive, or the secret already resolves. Non-secret-backed entries
// (From/Value/bare) are ignored — if_missing only applies to `secret:`.
func (e *Engine) resolveIfMissing(ctx context.Context, spec *task.Spec, parentRunID string) error {
	if e.secrets == nil {
		return nil
	}
	for _, entry := range spec.Permissions.Env {
		if entry.IfMissing == nil || entry.IfMissing.Task == "" || entry.Secret == "" {
			continue
		}
		if _, err := e.secrets.Resolve(ctx, entry.Secret); err == nil {
			continue // secret already present
		} else {
			var notFound *secrets.NotFoundError
			if !errors.As(err, &notFound) {
				return fmt.Errorf("check secret %q for env %q: %w", entry.Secret, entry.Name, err)
			}
		}

		prereqID := entry.IfMissing.Task
		prereqSpec, ok := e.registry.Get(prereqID)
		if !ok {
			return fmt.Errorf("if_missing task %q for secret %q is not registered", prereqID, entry.Secret)
		}

		e.log.Info("if_missing: running prereq",
			zap.String("task", spec.ID),
			zap.String("secret", entry.Secret),
			zap.String("prereq", prereqID),
		)

		prereqCtx, cancel := context.WithTimeout(ctx, ifMissingPrereqTimeout)
		params := entry.IfMissing.Params
		sfKey := "if_missing:" + entry.Secret
		res, sfErr, _ := e.prereqFlight.Do(sfKey, func() (any, error) {
			// Chain-mode input: non-nil empty map so the prereq task's
			// `input !== null` check treats this as a programmatic
			// invocation (silent refresh path), not an interactive UI click.
			chainInput := map[string]any{}
			// Pass prereqCtx so the 60s engine-level ceiling is enforced even
			// if the prereq task's own spec Timeout is longer (Fix 2, #387).
			prereqRunID, result, fireErr := e.fireSync(prereqCtx, prereqSpec, pkgruntime.RunOptions{
				ParentRunID: parentRunID,
				Input:       chainInput,
				Params:      params,
			}, "if_missing")
			return prereqFlightResult{runID: prereqRunID, result: result}, firstNonNil(fireErr, prereqCtx.Err())
		})
		cancel()

		if sfErr != nil {
			return fmt.Errorf("fire if_missing task %q: %w", prereqID, sfErr)
		}
		pfr := res.(prereqFlightResult)
		if pfr.result != nil && pfr.result.Error != nil {
			// Replay the prereq's logs into the parent run so the webhook
			// response surfaces whatever the prereq printed — typically an
			// "Open this URL to authorize" line the chat UI can render as a
			// clickable setup link. Without this, callers only see
			// "exit status 1" with no actionable context.
			e.copyPrereqLogs(ctx, pfr.runID, parentRunID, prereqID)
			return fmt.Errorf(ifMissingErrorFormat+": %w", entry.Secret, prereqID, pfr.result.Error)
		}

		if _, err := e.secrets.Resolve(ctx, entry.Secret); err != nil {
			e.copyPrereqLogs(ctx, pfr.runID, parentRunID, prereqID)
			return fmt.Errorf(ifMissingErrorFormat+" (ran but still unset): %w", entry.Secret, prereqID, err)
		}
	}
	return nil
}

// prereqFlightResult is the shared payload stored in singleflight so all
// callers waiting on the same secret get the same (runID, result) pair.
type prereqFlightResult struct {
	runID  string
	result *pkgruntime.RunResult
}

// firstNonNil returns the first non-nil error, letting a context-cancel
// surface from resolveIfMissing's timeout even when fireSync itself
// returned a nil error (because the executor observed the context fine).
func firstNonNil(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// copyPrereqLogs fetches a prereq run's log entries and appends them to the
// parent run's log, prefixed so they're visibly attributed. Best-effort:
// logging is a diagnostic aid and must never break the calling path.
func (e *Engine) copyPrereqLogs(ctx context.Context, prereqRunID, parentRunID, prereqID string) {
	if parentRunID == "" || prereqRunID == "" {
		return
	}
	logs, err := e.registry.GetRunLogs(ctx, prereqRunID)
	if err != nil {
		return
	}
	prefix := "[prereq " + prereqID + "] "
	for _, le := range logs {
		_ = e.registry.AppendLog(ctx, parentRunID, le.Level, prefix+le.Message)
	}
}

// dispatch routes a run to the appropriate executor and returns the final status and result.
func (e *Engine) dispatch(ctx context.Context, spec *task.Spec, opts pkgruntime.RunOptions) (string, *pkgruntime.RunResult) {
	e.mu.Lock()
	exec, ok := e.executors[spec.Runtime]
	e.mu.Unlock()

	if !ok || exec == nil {
		e.log.Error("no executor for runtime",
			zap.String("task", spec.ID),
			zap.String("runtime", string(spec.Runtime)),
		)
		if err := e.registry.FinishRun(context.Background(), opts.RunID, registry.StatusFailure); err != nil {
			e.log.Warn("FinishRun: no executor", zap.String("run", opts.RunID), zap.Error(err))
		}
		return registry.StatusFailure, &pkgruntime.RunResult{Error: fmt.Errorf("no executor for runtime %s", spec.Runtime)}
	}

	if err := e.resolveIfMissing(ctx, spec, opts.RunID); err != nil {
		e.log.Warn("if_missing prereq unsatisfied",
			zap.String("task", spec.ID),
			zap.Error(err),
		)
		if err2 := e.registry.FinishRun(context.Background(), opts.RunID, registry.StatusFailure); err2 != nil {
			e.log.Warn("FinishRun: if_missing failure", zap.String("run", opts.RunID), zap.Error(err2))
		}
		return registry.StatusFailure, &pkgruntime.RunResult{Error: err}
	}

	result, err := exec.Execute(ctx, spec, opts)
	if err != nil {
		e.log.Error("executor error",
			zap.String("task", spec.ID),
			zap.String("runtime", string(spec.Runtime)),
			zap.Error(err),
		)
		if err2 := e.registry.FinishRun(context.Background(), opts.RunID, registry.StatusFailure); err2 != nil {
			e.log.Warn("FinishRun: executor error", zap.String("run", opts.RunID), zap.Error(err2))
		}
		return registry.StatusFailure, &pkgruntime.RunResult{Error: err}
	}

	// Compute final status from the result.
	status := registry.StatusSuccess
	if result.Error != nil {
		if ctx.Err() != nil {
			status = registry.StatusCancelled
		} else {
			status = registry.StatusFailure
		}
	}

	// Marshal return value and determine persistence.
	//
	// `run_result.enabled: false` opts out of persisting the JSON-marshalled
	// return value to `runs.return_value`. Structured output_content and
	// output_content_type are unaffected — those carry rich-output payloads
	// (images, HTML) that are addressed separately by the WebUI's content
	// type negotiation and aren't part of the confidentiality concern.
	//
	// In-memory delivery: regardless of persistence, the marshalled JSON is
	// stashed in runReturnValue so WaitRun can serve it to synchronous
	// callers (dicode.run_task -> IPC reply). The entry is cleared by the
	// startRun cleanup func once the run reaches a terminal state.
	retJSON := ""
	if result != nil && result.ReturnValue != nil {
		if b, merr := json.Marshal(result.ReturnValue); merr == nil {
			retJSON = string(b)
		}
	}

	persistedReturnJSON := retJSON
	if persistReturnValue := spec.RunResult.PersistReturnValue(); !persistReturnValue {
		persistedReturnJSON = ""
		if retJSON != "" {
			e.runReturnValue.Store(opts.RunID, retJSON)
		}
	}

	outputContentType := ""
	outputContent := ""
	if result != nil {
		outputContentType = result.OutputContentType
		outputContent = result.OutputContent
	}

	// Atomic: set status + finished_at + return_value + output in one UPDATE.
	// This eliminates the race where a reader polling for status != running
	// could see the status flip before return_value was written.
	if err := e.registry.FinishRunWithResult(context.Background(), opts.RunID, status,
		persistedReturnJSON, outputContentType, outputContent); err != nil {
		e.log.Warn("FinishRun: finalize run result", zap.String("run", opts.RunID), zap.Error(err))
	}

	e.FireChain(context.Background(), spec.ID, opts.RunID, status, result.ChainInput, opts.Params)
	return status, result
}

// triggerSource returns a typed TriggerSource identifying the trigger type of a spec.
func triggerSource(spec *task.Spec) registry.TriggerSource {
	switch {
	case spec.Trigger.Cron != "":
		return registry.TriggerCron
	case spec.Trigger.Webhook != "":
		return registry.TriggerWebhook
	case spec.Trigger.Daemon:
		return registry.TriggerDaemon
	case spec.Trigger.Chain != nil:
		return registry.TriggerChain
	default:
		return registry.TriggerManual
	}
}
