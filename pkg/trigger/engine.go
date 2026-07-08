// Package trigger manages cron schedules, webhook dispatch, manual fires,
// chain reactions, and daemon (always-on) tasks.
package trigger

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dicode/dicode/pkg/audit"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/runtime/envresolve"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

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

	// runWG tracks every in-flight run goroutine (task runs via fireAsync and
	// pipeline-parent runs via firePipeline). Start drains it before returning
	// so the daemon's deferred database.Close() cannot run while a run goroutine
	// is still executing FinishRun/status writes — the DB must outlive run
	// finalization (issue #520). Add is always called before `go`, never inside
	// the goroutine, so Wait cannot race past a not-yet-started run.
	runWG sync.WaitGroup

	// drainGrace bounds the shutdown drain of runWG. Defaults to
	// shutdownDrainGrace; overridable in tests.
	drainGrace time.Duration

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
		drainGrace:         shutdownDrainGrace,
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

	// Pipeline terminal-daemon runs are not tracked in daemonRuns (they are
	// owned by the PipelineRunner, not the standalone-daemon machinery), so the
	// loop above misses them. Kill them explicitly so their run goroutines exit
	// and finalize within the drain window below instead of running past DB
	// close.
	e.livePipelineMu.Lock()
	pipelineDaemonRuns := make([]string, 0, len(e.livePipelines))
	for _, r := range e.livePipelines {
		r.mu.Lock()
		runID := r.daemonRunID
		r.mu.Unlock()
		if runID != "" {
			pipelineDaemonRuns = append(pipelineDaemonRuns, runID)
		}
	}
	e.livePipelineMu.Unlock()
	for _, runID := range pipelineDaemonRuns {
		e.KillRun(runID)
	}

	// Drain in-flight run goroutines before returning so the daemon's deferred
	// database.Close() runs only after every FinishRun/status write completes.
	// Bounded by drainGrace so a wedged run cannot hang shutdown forever.
	drained := make(chan struct{})
	go func() {
		e.runWG.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(e.drainGrace):
		e.log.Warn("shutdown drain grace elapsed with runs still in flight; proceeding to close",
			zap.Duration("grace", e.drainGrace))
	}
	return nil
}

// shutdownDrainGrace bounds how long Start waits for in-flight run goroutines to
// finalize after shutdown kills are issued. A wedged run cannot exceed this
// ceiling, so shutdown always makes progress.
const shutdownDrainGrace = 10 * time.Second

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
			if on == registry.StatusSuccess || on == chainOnAlways {
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
