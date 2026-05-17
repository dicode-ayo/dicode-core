// Package trigger manages cron schedules, webhook dispatch, manual fires,
// chain reactions, and daemon (always-on) tasks.
package trigger

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/notify"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/runtime/envresolve"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
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

	mu          sync.Mutex
	cronEntries map[string]cron.EntryID // taskID → cron entry
	webhooks    map[string]string       // webhook path → taskID

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

	// daemonStates tracks the preflight/lifecycle phase of each daemon task
	// for surfacing in the WebUI (Engine.DaemonState). Independent of
	// daemonMu — guarded by its own RWMutex so a state-read in the API path
	// never has to wait on a long preflight dispatch holding daemonMu.
	daemonStates *daemonStateMap

	// restartGates is a per-daemon at-most-one-in-flight lock for prereq-
	// driven restarts. See daemon_state.go for the coalescing rationale.
	restartGates *restartGate

	notifier        notify.Notifier
	notifyOnSuccess bool
	notifyOnFailure bool

	defaultsOnFailureChain task.OnFailureChainSpec // from config.Defaults.OnFailureChain

	db db.DB // optional — enables cron-job persistence and missed-run catchup

	secrets      secrets.Chain      // optional — enables if_missing prereq resolution at dispatch time
	prereqFlight singleflight.Group // collapses concurrent prereq runs keyed on secret name, so parallel webhook calls with the same missing secret don't each spawn a duplicate prereq (OAuth flow, refresh-token rotation, etc.)

	taskSem     chan struct{} // nil = unlimited; capacity = MaxConcurrentTasks
	taskWaiting atomic.Int64  // goroutines parked waiting for a semaphore slot
	started     atomic.Bool   // set to true by Start(); guards SetMaxConcurrentTasks

	runFinishedHook func(taskID, runID, status, triggerSource string, durationMs int64, notifyOnSuccess, notifyOnFailure bool)
	runStartedHook  func(taskID, runID, triggerSource string)

	// denoRuntime / pythonRuntime are typed runtime handles needed by the
	// Engine's ProviderRunner implementation (issue #119). The engine swaps
	// the per-runtime SecretOutputChannel per provider invocation. Wired in
	// daemon.go via SetDenoRuntime / SetPythonRuntime to avoid an import
	// cycle with the runtime packages.
	denoRuntime   DenoRuntimeAPI
	pythonRuntime PythonRuntimeAPI

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
		registry:     r,
		executors:    make(map[task.Runtime]pkgruntime.Executor),
		cron:         cron.New(),
		log:          log,
		cronEntries:  make(map[string]cron.EntryID),
		webhooks:     make(map[string]string),
		daemonRuns:   make(map[string]string),
		daemonSpecs:  make(map[string]*task.Spec),
		daemonStates: newDaemonStateMap(),
		restartGates: newRestartGate(),
		guards:       newChainGuards(),
	}
	e.executors[task.RuntimeDeno] = defaultExec
	return e
}

// SetDB wires a database into the engine for cron-job persistence.
// When set, the engine persists each cron task's next scheduled time and
// detects missed runs on startup (e.g. after a process restart).
func (e *Engine) SetDB(d db.DB) {
	e.db = d
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
// notifyOnSuccess and notifyOnFailure carry the resolved per-task notification flags.
func (e *Engine) SetRunFinishedHook(fn func(taskID, runID, status, triggerSource string, durationMs int64, notifyOnSuccess, notifyOnFailure bool)) {
	e.runFinishedHook = fn
}

// SetNotifier configures the push notification provider used for system-level alerts.
func (e *Engine) SetNotifier(n notify.Notifier) {
	e.notifier = n
}

// SetNotifyDefaults sets the global on_success / on_failure defaults.
// Per-task Notify overrides in task.Spec take precedence over these.
func (e *Engine) SetNotifyDefaults(onSuccess, onFailure bool) {
	e.notifyOnSuccess = onSuccess
	e.notifyOnFailure = onFailure
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

// resolveNotify returns the effective notification flags for a task spec,
// falling back to the engine's global defaults when the spec has no override.
func (e *Engine) resolveNotify(spec *task.Spec) (onSuccess, onFailure bool) {
	onSuccess = e.notifyOnSuccess
	onFailure = e.notifyOnFailure
	if n := spec.Notify; n != nil {
		if n.OnSuccess != nil {
			onSuccess = *n.OnSuccess
		}
		if n.OnFailure != nil {
			onFailure = *n.OnFailure
		}
	}
	return
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

// Register adds or updates trigger registrations for a task spec. Returns a
// non-nil error when cross-spec validation fails (currently: invalid
// trigger.before references). On error, no triggers are registered.
func (e *Engine) Register(spec *task.Spec) error {
	// Cross-spec validation must run BEFORE Unregister so that a previously
	// valid registration isn't torn down when an updated spec fails its
	// new-state checks. The registry-snapshot lookups are read-only.
	if err := e.validateBeforeRefs(spec); err != nil {
		return err
	}

	e.Unregister(spec.ID)

	// Disabled tasks are kept in the registry for API visibility but must not
	// be scheduled, spawned as daemons, or registered as webhook endpoints.
	if !spec.Enabled {
		e.log.Info("task registered (disabled — no triggers scheduled)",
			zap.String("task", spec.ID),
			zap.String("runtime", string(spec.Runtime)),
		)
		return nil
	}

	if spec.Trigger.Cron != "" {
		e.registerCron(spec)
	}
	if spec.Trigger.Webhook != "" {
		e.registerWebhook(spec)
	}
	if spec.Trigger.Daemon {
		e.registerDaemon(spec)
	}
	e.log.Info("task registered",
		zap.String("task", spec.ID),
		zap.String("trigger", string(triggerSource(spec))),
		zap.String("runtime", string(spec.Runtime)),
	)
	return nil
}

// validateBeforeRefs checks each trigger.before entry against the current
// registry. Per-spec validation (Spec.validate) already enforces shape;
// here we only catch the things that need the full registry: unknown task
// IDs and references to other daemons.
//
// On cycles: per-spec validation requires trigger.before only on daemon
// tasks, and this function forbids before: references to daemons. Together
// those constraints make cycles structurally unreachable — the only way a
// cycle could form is through a prereq task carrying its own
// trigger.before back to the daemon, but only daemons may have
// trigger.before. We therefore do not implement explicit cycle detection.
func (e *Engine) validateBeforeRefs(spec *task.Spec) error {
	for i, entry := range spec.Trigger.Before {
		ref, ok := e.registry.Get(entry.Task)
		if !ok {
			return fmt.Errorf("trigger.before: task %q not found in registry", entry.Task)
		}
		if ref.Trigger.Daemon {
			return fmt.Errorf("trigger.before: task %q is a daemon (only one-shot tasks can be preflights)", entry.Task)
		}
		// If this edge carries per-firing overrides, verify they merge
		// onto the prereq spec without producing an invalid Spec. Doing
		// this here surfaces malformed overrides at Register time rather
		// than at the first daemon start — operators see a clean error
		// path instead of a silently-failing preflight.
		if entry.Overrides != nil {
			merged := taskset.ApplyOverrides(ref, entry.Overrides)
			if err := merged.Validate(); err != nil {
				return fmt.Errorf("trigger.before: overrides for %q produce invalid spec: %w", entry.Task, err)
			}
		}
		// ${input.output} on before[0] is statically unresolvable: the
		// first pipeline stage has no upstream return value. Reject at
		// registration so operators see the failure at config-load time
		// rather than the first daemon dispatch. Non-first stages are
		// allowed because PR3 will pipe the previous stage's output into
		// upstreamOutput.
		if i == 0 && entry.Overrides != nil {
			for _, p := range entry.Overrides.Params {
				if p.Default == task.InputOutputToken {
					return fmt.Errorf(
						"trigger.before[0].overrides.params.%s: ${input.output} is not available on the first pipeline stage",
						p.Name,
					)
				}
			}
		}
	}
	return nil
}

// Unregister removes all trigger registrations for a task ID.
func (e *Engine) Unregister(id string) {
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
	id, err := e.cron.AddFunc(spec.Trigger.Cron, func() {
		s, ok := e.registry.Get(spec.ID)
		if !ok {
			return
		}
		// Advance next_run_at AFTER fireAsync so that a failed dispatch does not
		// silently advance the schedule and cause the missed run to be invisible
		// on the next restart.
		if _, ferr := e.fireAsync(context.Background(), s, pkgruntime.RunOptions{}, registry.TriggerCron); ferr == nil && e.db != nil {
			if next, nerr := cronNextRun(spec.Trigger.Cron); nerr == nil {
				if dbErr := e.db.Exec(context.Background(),
					`UPDATE cron_jobs SET last_run_at=?, next_run_at=? WHERE task_id=?`,
					time.Now().Unix(), next.Unix(), spec.ID,
				); dbErr != nil {
					e.log.Warn("cron: failed to persist next_run_at",
						zap.String("task", spec.ID), zap.Error(dbErr))
				}
			}
		}
	})
	if err != nil {
		e.log.Error("invalid cron expression",
			zap.String("task", spec.ID),
			zap.String("cron", spec.Trigger.Cron),
			zap.Error(err),
		)
		return
	}
	e.mu.Lock()
	e.cronEntries[spec.ID] = id
	e.mu.Unlock()

	if e.db != nil {
		if next, nerr := cronNextRun(spec.Trigger.Cron); nerr == nil {
			if dbErr := e.db.Exec(context.Background(),
				`INSERT INTO cron_jobs(task_id,cron_expr,next_run_at) VALUES(?,?,?)
				 ON CONFLICT(task_id) DO UPDATE SET cron_expr=excluded.cron_expr, next_run_at=excluded.next_run_at`,
				spec.ID, spec.Trigger.Cron, next.Unix(),
			); dbErr != nil {
				e.log.Warn("cron: failed to persist cron_jobs row",
					zap.String("task", spec.ID), zap.Error(dbErr))
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

// reservedOAuthCompletePath is the webhook path the relay broker delivers
// encrypted OAuth tokens to. Only buildin/auth-relay is allowed to bind it —
// any other task that claims this path would be a drop-in exfiltration
// sink for decrypted credentials once the built-in chains the data forward.
const reservedOAuthCompletePath = "/hooks/oauth-complete"
const oauthRelayBuiltinID = "buildin/auth-relay"

func (e *Engine) registerWebhook(spec *task.Spec) {
	if spec.Trigger.Webhook == reservedOAuthCompletePath && spec.ID != oauthRelayBuiltinID {
		e.log.Warn("rejecting task that tries to shadow reserved OAuth delivery path",
			zap.String("task", spec.ID),
			zap.String("path", reservedOAuthCompletePath))
		return
	}
	e.mu.Lock()
	e.webhooks[spec.Trigger.Webhook] = spec.ID
	e.mu.Unlock()
}

func (e *Engine) registerDaemon(spec *task.Spec) {
	e.daemonMu.Lock()
	e.daemonSpecs[spec.ID] = spec
	_, alreadyRunning := e.daemonRuns[spec.ID]
	e.daemonMu.Unlock()

	if alreadyRunning {
		return
	}
	// Gate the start path with the same per-daemon lock used for restart
	// coalescing. Two concurrent registerDaemon calls (e.g. the reconciler
	// firing OnRegister twice while preflight is in flight, or a manual
	// re-register racing the reconciler) would otherwise both observe
	// `alreadyRunning=false` and both spawn a goroutine that runs preflight
	// + fireAsync. The lock ensures at most one in-flight start per task ID.
	// Release happens inside startDaemon after the daemon's run slot is
	// recorded (or after a preflight/dispatch failure has been logged).
	if !e.restartGates.tryAcquire(spec.ID) {
		e.log.Debug("daemon start coalesced — another start is already in flight",
			zap.String("task", spec.ID))
		return
	}
	// startDaemon may block on preflight (trigger.before); detach so
	// Register and the reconciler's OnRegister callback stay synchronous
	// for non-preflight daemons and don't stall the registration sweep
	// for preflight ones.
	if len(spec.Trigger.Before) == 0 {
		defer e.restartGates.release(spec.ID)
		e.startDaemon(spec)
		return
	}
	go func() {
		defer e.restartGates.release(spec.ID)
		e.startDaemon(spec)
	}()
}

// startDaemon brings a daemon up. When trigger.before is set, runs the
// preflight chain first and only fires the daemon if every prereq returns
// status=success. Sets daemonStates throughout for WebUI visibility.
func (e *Engine) startDaemon(spec *task.Spec) {
	if len(spec.Trigger.Before) > 0 {
		e.setDaemonState(spec.ID, DaemonPrereqRunning)
		if err := e.runPrereqs(context.Background(), spec); err != nil {
			e.setDaemonState(spec.ID, DaemonPrereqFailed)
			e.log.Warn("daemon preflight failed; daemon not started",
				zap.String("task", spec.ID),
				zap.Error(err),
			)
			return
		}
	}

	runID, err := e.fireAsync(context.Background(), spec, pkgruntime.RunOptions{}, registry.TriggerDaemon)
	if err != nil {
		e.setDaemonState(spec.ID, DaemonStopped)
		e.log.Error("daemon start failed", zap.String("task", spec.ID), zap.Error(err))
		return
	}
	e.daemonMu.Lock()
	e.daemonRuns[spec.ID] = runID
	e.daemonMu.Unlock()
	e.setDaemonState(spec.ID, DaemonRunning)
}

// runPrereqs fires every task listed in spec.Trigger.Before in parallel and
// blocks until they all reach a terminal state. Returns nil only if every
// prereq finishes with status=success; otherwise returns the first
// non-success error observed.
//
// Re-fires every prereq on every preflight attempt — no "already satisfied"
// short-circuit. The whole point of preflight is to refresh ephemeral
// state (rendered configs, freshly-rotated credentials) right before the
// daemon starts; reusing yesterday's success would defeat that. If
// operators want caching, that belongs in the prereq task itself, not in
// the trigger engine.
func (e *Engine) runPrereqs(ctx context.Context, spec *task.Spec) error {
	type prereqResult struct {
		refID string
		err   error
	}
	results := make(chan prereqResult, len(spec.Trigger.Before))
	var wg sync.WaitGroup

	for _, entry := range spec.Trigger.Before {
		entry := entry
		refID := entry.Task
		ref, ok := e.registry.Get(refID)
		if !ok {
			// validateBeforeRefs catches this at registration time, but the
			// registry can change between registration and preflight (task
			// unregistered while daemon was queued, etc.). Defensive check.
			results <- prereqResult{refID: refID, err: fmt.Errorf("prereq %q vanished from registry", refID)}
			continue
		}
		// Resolve ${input.output} in overrides.params. PR2 ships with
		// upstreamOutput="" — any token use here fails loudly (the
		// resolver returns an error before reaching the assignment
		// below). PR3 will replace "" with the previous stage's return
		// value once `before:` runs sequentially.
		//
		// TODO(PR3): BeforeEntry.Overrides is a *Overrides — the
		// assignment `entry.Overrides.Params = resolved` mutates the
		// pointed-to struct, which is shared with the registry-held
		// spec. Latent in PR2 (resolver either errors or produces an
		// identical slice). When PR3 wires real upstream values,
		// concurrent preflights of the same daemon would race on this
		// write. PR3's runPrereqs rewrite must either shallow-copy
		// entry.Overrides before assignment, OR thread `resolved`
		// through as a local without writing back (e.g. build the
		// merged spec from resolved directly, bypassing the field).
		if entry.Overrides != nil && entry.Overrides.Params != nil {
			resolved, rerr := task.ResolveInputOutputList(entry.Overrides.Params, "")
			if rerr != nil {
				results <- prereqResult{refID: refID, err: fmt.Errorf("resolve input refs: %w", rerr)}
				continue
			}
			entry.Overrides.Params = resolved
		}
		// Per-edge overrides (#NNN): if this preflight edge declares
		// `overrides:`, merge them onto a deep copy of the prereq spec
		// before dispatching. The registry's canonical spec is left
		// untouched so the prereq's standalone (manual / cron / chain)
		// fires continue using the spec on disk. The merged spec is
		// re-validated to surface override-induced invariant violations
		// (e.g. an override that switches runtime to an unsupported
		// value) — validateBeforeRefs already runs the same check at
		// Register time, this is a defensive second pass for cases where
		// the registry mutated between Register and the preflight fire.
		dispatchSpec := ref
		if entry.Overrides != nil {
			merged := taskset.ApplyOverrides(ref, entry.Overrides)
			if vErr := merged.Validate(); vErr != nil {
				results <- prereqResult{refID: refID, err: fmt.Errorf("overrides produce invalid spec: %w", vErr)}
				continue
			}
			dispatchSpec = merged
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			runID, err := e.fireAsync(ctx, dispatchSpec, pkgruntime.RunOptions{}, registry.TriggerPreflight)
			if err != nil {
				results <- prereqResult{refID: refID, err: fmt.Errorf("dispatch: %w", err)}
				return
			}
			// Use WaitRun (the existing terminal-state waiter, channel-backed
			// rather than polling) so this scales to many parallel prereqs
			// without hammering the DB.
			res, werr := e.WaitRun(ctx, runID)
			if werr != nil {
				results <- prereqResult{refID: refID, err: fmt.Errorf("wait: %w", werr)}
				return
			}
			if res.Status != registry.StatusSuccess {
				results <- prereqResult{refID: refID, err: fmt.Errorf("status=%s", res.Status)}
				return
			}
			results <- prereqResult{refID: refID, err: nil}
		}()
	}

	wg.Wait()
	close(results)

	var firstErr error
	for r := range results {
		if r.err != nil && firstErr == nil {
			firstErr = fmt.Errorf("prereq %q: %w", r.refID, r.err)
		}
	}
	return firstErr
}

func (e *Engine) onDaemonRunFinished(spec *task.Spec, runID string) {
	e.daemonMu.Lock()
	if e.daemonRuns[spec.ID] == runID {
		delete(e.daemonRuns, spec.ID)
	}
	_, stillRegistered := e.daemonSpecs[spec.ID]
	e.daemonMu.Unlock()

	if !stillRegistered || e.isShuttingDown() {
		return
	}

	run, err := e.registry.GetRun(context.Background(), runID)
	if err != nil {
		e.log.Error("daemon: failed to get run status", zap.String("run", runID), zap.Error(err))
		return
	}
	if run.Status == registry.StatusCancelled {
		return
	}

	restart := spec.Trigger.Restart
	if restart == "" {
		restart = "always"
	}
	switch restart {
	case "never":
		e.log.Info("daemon exited — restart=never, not restarting",
			zap.String("task", spec.ID), zap.String("status", run.Status))
		return
	case "on-failure":
		if run.Status != registry.StatusFailure {
			e.log.Info("daemon exited — restart=on-failure, not restarting (no failure)",
				zap.String("task", spec.ID), zap.String("status", run.Status))
			return
		}
	}

	e.log.Info("daemon exited, scheduling restart",
		zap.String("task", spec.ID),
		zap.String("status", run.Status),
		zap.String("restart", restart),
	)

	shutCtx := e.getShutdownCtx()
	if shutCtx == nil {
		shutCtx = context.Background()
	}
	select {
	case <-shutCtx.Done():
		return
	case <-time.After(2 * time.Second):
	}

	if !e.isShuttingDown() {
		e.log.Info("restarting daemon task", zap.String("task", spec.ID))
		e.startDaemon(spec)
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
	spec, ok := e.registry.Get(taskID)
	if !ok {
		return "", fmt.Errorf("task %q not found", taskID)
	}
	e.log.Info("manual trigger", zap.String("task", taskID))
	return e.fireAsync(context.Background(), spec, pkgruntime.RunOptions{Params: params}, registry.TriggerManual)
}

// FireFromTask triggers a task as a child of an in-flight run. Used by the
// dicode.run_task IPC handler so the new run's parent_run_id (#116) points
// at the caller. Falls back to a plain manual fire when parentRunID is "".
func (e *Engine) FireFromTask(ctx context.Context, taskID, parentRunID string, params map[string]string) (string, error) {
	if parentRunID == "" {
		return e.FireManual(ctx, taskID, params)
	}
	spec, ok := e.registry.Get(taskID)
	if !ok {
		return "", fmt.Errorf("task %q not found", taskID)
	}
	e.log.Info("subtask trigger", zap.String("task", taskID), zap.String("parent", parentRunID))
	return e.fireAsync(context.Background(), spec,
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
	r := envresolve.New(e.registry, e.secrets, e)
	resolved, err := r.Resolve(ctx, spec)
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
		// Non-typed error: let dispatch surface it normally.
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

// FireChain checks if any tasks declare a chain trigger from completedTaskID,
// and fires the global on_failure_chain if configured.
func (e *Engine) FireChain(ctx context.Context, completedTaskID, runID, runStatus string, output interface{}) {
	// Preflight restart hook: when a task that some daemon lists in
	// trigger.before finishes with status=success, restart that daemon so
	// it picks up newly-rendered config / freshly-rotated secrets. Failure
	// or cancel does NOT trigger a restart — see the failure-semantics
	// commit and tests for the rationale.
	if runStatus == registry.StatusSuccess {
		e.notifyPrereqCompletion(completedTaskID)
	}

	// Declared chain triggers.
	for _, spec := range e.registry.All() {
		chain := spec.Trigger.Chain
		if chain == nil || chain.From != completedTaskID {
			continue
		}
		on := chain.ChainOn()
		if on != "always" && on != runStatus {
			continue
		}
		// Per-edge overrides (#NNN): when the downstream's trigger.chain
		// declares `overrides:`, apply them to a deep copy of the
		// downstream spec before dispatching. The registry's canonical
		// spec is preserved so manual / cron / non-chain fires of the
		// same downstream see the on-disk values. The merged spec is
		// re-validated; on failure we log and skip this edge rather than
		// dispatching a malformed spec.
		dispatchSpec := spec
		if chain.Overrides != nil {
			merged := taskset.ApplyOverrides(spec, chain.Overrides)
			if vErr := merged.Validate(); vErr != nil {
				e.log.Error("chain trigger skipped — overrides produce invalid spec",
					zap.String("from", completedTaskID),
					zap.String("to", spec.ID),
					zap.Error(vErr),
				)
				continue
			}
			dispatchSpec = merged
		}
		// Dispatch-time ${input.output} interpolation: substitute the
		// literal token in chain.Params with the upstream's return value.
		// Only direct-string upstream returns flow through; non-string
		// returns are treated as "no upstream available" — the chain
		// dispatch is skipped (logged) rather than silently passing the
		// literal token to the downstream.
		upstreamRet := stringRet(output)
		resolvedParams, rerr := task.ResolveInputOutputMap(chain.Params, upstreamRet)
		if rerr != nil {
			e.log.Error("chain trigger skipped — failed to resolve ${input.output}",
				zap.String("from", completedTaskID),
				zap.String("to", spec.ID),
				zap.Error(rerr),
			)
			continue
		}
		e.log.Info("chain trigger",
			zap.String("from", completedTaskID),
			zap.String("to", spec.ID),
			zap.String("on", on),
		)
		go e.fireAsync(ctx, dispatchSpec, pkgruntime.RunOptions{ //nolint:errcheck
			ParentRunID: runID,
			Input:       buildChainInput(resolvedParams, completedTaskID, runID, runStatus, output),
		}, "chain")
	}

	// Config-level default on_failure_chain.
	if runStatus == "failure" {
		chainSpec := e.defaultsOnFailureChain
		if failedSpec, ok := e.registry.Get(completedTaskID); ok {
			if failedSpec.OnFailureChain != nil {
				chainSpec = *failedSpec.OnFailureChain // per-task fully replaces defaults
			}
		}
		targetID := chainSpec.Task
		if targetID != "" && targetID != completedTaskID {
			// Replay-source guard: a replay-fired run that fails must not
			// trigger on_failure_chain — preserves the semantics that replay
			// is a manual / agent-initiated action with no auto-recovery loop.
			// (Spec § 4.3 #5.)
			if src, ok := e.runTriggerSource.Load(runID); ok {
				if ts, _ := src.(registry.TriggerSource); ts == registry.TriggerReplay {
					e.log.Debug("on_failure_chain skipped: replay source",
						zap.String("from", completedTaskID),
						zap.String("run", runID),
					)
					return
				}
			}
			// Depth-tracking ceiling: read the incoming run's chain depth and
			// refuse to fire when the next hop would exceed MaxDepth (default 2).
			// This replaces the old chain-of-chains suppression that blocked all
			// chaining beyond depth 1.
			incomingDepth := 0
			if d, ok := e.runChainDepth.Load(runID); ok {
				incomingDepth, _ = d.(int)
			}
			nextDepth := incomingDepth + 1
			maxDepth := chainSpec.EffectiveMaxDepth()
			if nextDepth > maxDepth {
				e.log.Warn("on_failure_chain suppressed: max_depth exceeded",
					zap.Int("depth", nextDepth),
					zap.Int("max_depth", maxDepth),
					zap.String("from", completedTaskID),
					zap.String("run", runID),
				)
				return
			}
			if targetSpec, ok := e.registry.Get(targetID); ok {
				now := time.Now()

				// 1. Storm check — per-source namespace.
				scope := stormScope(completedTaskID)
				if e.guards.stormSuppressed(scope, now) {
					e.log.Warn("on_failure_chain suppressed: storm circuit breaker tripped",
						zap.String("scope", scope),
						zap.String("from", completedTaskID),
					)
					return
				}

				// 2. Cooldown check.
				cd := effectiveCooldown(chainSpec)
				if e.guards.cooldownActive(completedTaskID, cd, now) {
					e.log.Warn("on_failure_chain suppressed: cooldown active",
						zap.String("from", completedTaskID),
						zap.Duration("cooldown", cd),
					)
					return
				}

				// 3. Concurrency check.
				perTask := chainSpec.MaxConcurrent
				if perTask <= 0 {
					perTask = 1
				}
				global := e.defaultsOnFailureChain.MaxConcurrentGlobal
				if global <= 0 {
					global = 3
				}
				if !e.guards.acquireSlot(completedTaskID, perTask, global) {
					e.log.Warn("on_failure_chain suppressed: concurrency cap reached",
						zap.String("from", completedTaskID),
						zap.Int("cap_per_task", perTask),
						zap.Int("cap_global", global),
					)
					return
				}

				// Dispatch-time ${input.output} interpolation: substitute
				// the literal token in chainSpec.Params with the upstream's
				// return value. Symmetric with the success-chain path above —
				// any chain edge supports the token; non-string upstream
				// returns are treated as "no upstream available" and the
				// failure-chain dispatch is skipped (logged) rather than
				// silently passing the literal token to the downstream.
				// Release the chainGuards slot we just acquired so a failed
				// resolution doesn't burn cap_per_task / cap_global.
				upstreamRet := stringRet(output)
				resolvedParams, rerr := task.ResolveInputOutputMap(chainSpec.Params, upstreamRet)
				if rerr != nil {
					e.guards.releaseSlot(completedTaskID)
					e.log.Error("on_failure_chain skipped — failed to resolve ${input.output}",
						zap.String("from", completedTaskID),
						zap.String("to", targetID),
						zap.Error(rerr),
					)
					return
				}
				e.log.Info("on_failure_chain trigger",
					zap.String("from", completedTaskID),
					zap.String("to", targetID),
					zap.String("run", runID),
					zap.Int("depth", nextDepth),
				)
				// Build input via the shared buildChainPayload kernel so the
				// failure-path and success-path stamps stay in lockstep.
				// Reserved keys (taskID, runID, status, output, _chain_depth)
				// are populated by the engine and are NOT user-overridable;
				// config-load validation (#236 Task 11) rejects any
				// chainSpec.Params containing these keys, so collisions
				// cannot reach here in a well-validated config.
				input := buildChainPayload(resolvedParams, completedTaskID, runID, runStatus, output, nextDepth)
				// Auto-fix safety default: when the chain target is the
				// buildin auto-fix preset, force mode=review unless the
				// operator explicitly set it. Autonomous-by-default would
				// be a foot-gun (the agent could push directly to main
				// without human review). Stamped AFTER the kernel so a
				// chainSpec.Params{mode:"yolo"} entry still wins.
				if targetID == "buildin/auto-fix" {
					if _, ok := input["mode"]; !ok {
						input["mode"] = "review"
					}
				}

				// Synchronously invoke fireAsync — it returns once startRun
				// completes, then continues the run on its own goroutine.
				// Doing this sync (rather than `go fireAsync(...)`) lets us
				// release the chainGuards slot if startRun fails and avoids
				// recording cooldown / storm counters for runs that never
				// executed (false-trip risk under DB flapping).
				if _, err := e.fireAsync(ctx, targetSpec, pkgruntime.RunOptions{
					ParentRunID:     runID,
					Input:           input,
					ChainParentTask: completedTaskID,
				}, registry.TriggerChain); err != nil {
					e.guards.releaseSlot(completedTaskID)
					e.log.Error("on_failure_chain fireAsync failed; released slot",
						zap.String("from", completedTaskID),
						zap.String("to", targetID),
						zap.Error(err),
					)
					return
				}

				// startRun succeeded. Record cooldown timestamp and storm
				// fire — both delayed until here so a failed startRun cannot
				// false-trip the storm breaker or extend the cooldown.
				e.guards.recordChainFire(completedTaskID, now)
				e.guards.observeChainFire(scope, chainSpec.Storm.Rate, chainSpec.Storm.Window,
					chainSpec.Storm.Suppress, now)
			}
		}
	}
}

// stringRet returns the upstream's return value as a string for
// ${input.output} interpolation. JSON-marshalled objects, numbers,
// and lists are NOT auto-stringified — only direct string returns
// flow through. Non-string returns produce "" which propagates as
// ErrInputUnavailable through the resolver.
func stringRet(rv interface{}) string {
	s, _ := rv.(string)
	return s
}

// buildChainInput shapes the `Input` value handed to a downstream task that
// was fired by a success-path trigger.chain (NOT on_failure_chain — that
// path runs through fireFailureChain and always wraps).
//
// Contract:
//
//   - When userParams is empty (the historical case), returns the upstream's
//     raw output unchanged. This preserves the existing contract for tasks
//     that consume `input` as the upstream's return value directly. Adding
//     a wrapping unconditionally would silently break every downstream task
//     in the wild that reads input as e.g. a string or a typed object.
//
//   - When userParams is non-empty, returns a map merging user params with
//     engine-reserved keys (taskID, runID, status, output, _chain_depth).
//     Reserved keys cannot collide with user params: Spec.validate rejects
//     reserved keys in trigger.chain.params at config load.
//
// _chain_depth is set to 0 for success chains. Depth tracking only matters
// on the failure path today (cap loops via OnFailureChainSpec.MaxDepth);
// success chains are intentionally not depth-capped because users build
// fan-out pipelines (render → start daemon, etc.) where depth > 1 is normal.
func buildChainInput(userParams map[string]any, completedTaskID, runID, status string, output any) any {
	if len(userParams) == 0 {
		return output
	}
	return buildChainPayload(userParams, completedTaskID, runID, status, output, 0)
}

// buildChainPayload is the shared kernel that produces the input map fed to
// any chained task — success-path (trigger.chain) AND failure-path
// (on_failure_chain). Both sites used to inline the same five engine-key
// stamps; the unification eliminates that drift (survey §5.4 / 6.2.3).
//
// Reserved keys (taskID, runID, status, output, _chain_depth) are always
// stamped *after* the userParams overlay, so a (well-validated) user map
// cannot collide. Config-load validation enforces the reserved-key
// invariant at three sites; this helper is the runtime backstop.
func buildChainPayload(userParams map[string]any, completedTaskID, runID, status string, output any, depth int) map[string]any {
	m := make(map[string]any, len(userParams)+5)
	for k, v := range userParams {
		m[k] = v
	}
	m["taskID"] = completedTaskID
	m["runID"] = runID
	m["status"] = status
	m["output"] = output
	m["_chain_depth"] = depth
	return m
}

const (
	// webhookMaxBodyBytes caps the body read for HMAC verification.
	webhookMaxBodyBytes = 5 << 20 // 5 MB
	// webhookTimestampTolerance is the replay-protection window.
	webhookTimestampTolerance = 5 * time.Minute
	// webhookSignatureHeader is the default signature header (GitHub-compatible).
	webhookSignatureHeader = "X-Hub-Signature-256"
	// webhookTimestampHeader carries the Unix timestamp for replay protection.
	webhookTimestampHeader = "X-Dicode-Timestamp"
)

// taskErrorPage is the HTML template for task failures that produce no output.
// Uses the same ansi-to-html library and log styling as the webui run-detail component.
// Printf args: %s = runID, %s = error message (html-escaped), %s = JSON log lines array.
const taskErrorPage = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<style>
  body { font-family: system-ui, sans-serif; padding: 2rem; background: #1e1e2e; color: #cdd6f4; margin: 0; }
  h2 { color: #f38ba8; margin-top: 0; }
  .run-id { font-family: monospace; font-size: .85em; color: #6c7086; margin-bottom: 1.5rem; }
  .error-msg { background: #302030; border-left: 3px solid #f38ba8; padding: 1rem; border-radius: 4px;
               white-space: pre-wrap; font-family: monospace; font-size: .9em; margin-bottom: 1.5rem; }
  h3 { color: #cdd6f4; margin-bottom: .5rem; }
  pre#logs { background: #181825; border-radius: 6px; padding: 1rem; overflow-x: auto;
             font-family: monospace; font-size: .85em; line-height: 1.5; white-space: pre-wrap; }
  pre#logs span { display: block; }
  pre#logs span.error { color: #f38ba8; }
  pre#logs span.warn  { color: #f9e2af; }
  pre#logs span.info  { color: #cdd6f4; }
</style>
</head>
<body>
<h2>Task error</h2>
<div class="run-id">Run %s</div>
<div class="error-msg">%s</div>
<h3>Logs</h3>
<pre id="logs"></pre>
<script id="log-data" type="application/json">%s</script>
<script type="module">
import Convert from 'https://esm.sh/ansi-to-html@0.7.2';
const conv = new Convert({ fg: '#cdd6f4', bg: '#181825', escapeXML: true,
  colors: { 1:'#f38ba8',2:'#a6e3a1',3:'#f9e2af',4:'#89b4fa',5:'#cba6f7',6:'#89dceb',7:'#cdd6f4' } });
const logs = JSON.parse(document.getElementById('log-data').textContent);
const pre = document.getElementById('logs');
if (!logs.length) { pre.textContent = '(no logs)'; }
else { pre.innerHTML = logs.map(l => {
  const cls = /error|uncaught|notcapable/i.test(l) ? 'error' : /warn/i.test(l) ? 'warn' : 'info';
  return '<span class="' + cls + '">' + conv.toHtml(l) + '</span>';
}).join(''); }
</script>
</body>
</html>`

// verifyWebhookSignature validates HMAC-SHA256 signature and optional replay
// protection for a webhook request. Returns nil when the request is authentic.
// When no secret is configured on the task the check is skipped (open webhook).
func verifyWebhookSignature(spec *task.Spec, r *http.Request, body []byte) error {
	secret := spec.Trigger.WebhookSecret
	if secret == "" {
		return nil // unauthenticated webhook — allowed for backwards-compat
	}

	// Replay protection via timestamp header (optional — not all senders provide it).
	if tsStr := r.Header.Get(webhookTimestampHeader); tsStr != "" {
		ts, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid %s header", webhookTimestampHeader)
		}
		age := time.Since(time.Unix(ts, 0))
		if age < 0 {
			age = -age
		}
		if age > webhookTimestampTolerance {
			return fmt.Errorf("webhook timestamp out of tolerance window (%v)", age.Round(time.Second))
		}
	}

	got := r.Header.Get(webhookSignatureHeader)
	if got == "" {
		return fmt.Errorf("missing %s header", webhookSignatureHeader)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(got), []byte(want)) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

// WebhookHandler returns an HTTP handler that dispatches webhook-triggered tasks.
//
// Behaviour by request type:
//   - GET  /{hookPath}            — if the task directory contains index.html, serve
//     it with the dicode client SDK injected; otherwise run the task with query params.
//   - GET  /{hookPath}/{asset}    — serve a static asset (CSS/JS/image) from the task
//     directory, sandboxed so path traversal is impossible.
//   - POST /{hookPath}            — run the task. JSON body or form-encoded body are
//     both accepted. Browser form submissions (Content-Type: form) redirect to the run
//     result page; API callers receive the usual JSON envelope.
func (e *Engine) WebhookHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Exact match — normal webhook execution path.
		e.mu.Lock()
		taskID, ok := e.webhooks[path]
		var assetPath, matchedHook string
		if !ok {
			// No exact match — the request is for a static asset under some
			// webhook UI. Walk up one path segment at a time doing exact
			// map lookups, so the most-specific parent hook wins. This
			// matters when both `/hooks/ai` and `/hooks/ai/openai` are
			// registered: `/hooks/ai/openai/chat.js` must bind to the
			// preset, not to the buildin. Exact map lookups (rather than
			// iterating e.webhooks with strings.HasPrefix) are also
			// immune to Go's randomised map iteration order.
			for candidate := path; ; {
				idx := strings.LastIndex(candidate, "/")
				if idx <= 0 {
					break
				}
				candidate = candidate[:idx]
				if tid, found := e.webhooks[candidate]; found {
					taskID = tid
					matchedHook = candidate
					assetPath = path[len(candidate)+1:]
					ok = true
					break
				}
			}
		} else {
			matchedHook = path
		}
		e.mu.Unlock()

		if !ok {
			http.NotFound(w, r)
			return
		}

		spec, ok := e.registry.Get(taskID)
		if !ok {
			http.Error(w, "task not found", http.StatusNotFound)
			return
		}

		// Serve a static asset from the task directory (CSS, JS, images, …).
		// If the sub-path has no recognised file extension and the task has an
		// index.html, fall back to serving that — enabling SPA client-side routing
		// (e.g. /hooks/webui/config, /hooks/webui/tasks/foo all return the SPA shell).
		// This intentionally applies to any webhook task that ships an index.html,
		// not just the built-in webui — it is the standard "SPA shell" pattern.
		if assetPath != "" {
			// Block path traversal before any extension check; the SPA fallback
			// must not silently swallow traversal attempts by serving index.html.
			if strings.Contains(assetPath, "..") {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if r.Method == http.MethodGet &&
				filepath.Ext(assetPath) == "" {
				indexFile := filepath.Join(spec.TaskDir, "index.html")
				if data, err := os.ReadFile(indexFile); err == nil {
					html := injectDicodeSDK(string(data), matchedHook, taskID, r)
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					_, _ = w.Write([]byte(html))
					return
				}
			}
			e.serveTaskAsset(w, r, spec.TaskDir, assetPath)
			return
		}

		// On GET, serve the task's index.html UI when one is present.
		if r.Method == http.MethodGet {
			indexFile := filepath.Join(spec.TaskDir, "index.html")
			if data, err := os.ReadFile(indexFile); err == nil {
				e.log.Info("webhook UI served", zap.String("path", path), zap.String("task", taskID))
				html := injectDicodeSDK(string(data), matchedHook, taskID, r)
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte(html))
				return
			}
		}

		e.log.Info("webhook trigger", zap.String("path", path), zap.String("task", taskID))

		var input interface{}
		isFormSubmit := false
		var body []byte

		if r.Method == http.MethodGet {
			if q := r.URL.Query(); len(q) > 0 {
				m := make(map[string]interface{}, len(q))
				for k, v := range q {
					if len(v) == 1 {
						m[k] = v[0]
					} else {
						m[k] = v
					}
				}
				input = m
			}
		} else {
			// Read the raw body first so HMAC verification always covers the
			// actual request bytes, regardless of content-type.
			if r.Body != nil {
				body, _ = io.ReadAll(io.LimitReader(r.Body, webhookMaxBodyBytes))
			}
			ct := r.Header.Get("Content-Type")
			if strings.Contains(ct, "application/x-www-form-urlencoded") {
				// Replay the raw bytes back into r.Body so ParseForm can read them.
				r.Body = io.NopCloser(bytes.NewReader(body))
				if err := r.ParseForm(); err == nil {
					m := make(map[string]interface{}, len(r.Form))
					for k, v := range r.Form {
						if len(v) == 1 {
							m[k] = v[0]
						} else {
							m[k] = v
						}
					}
					input = m
					isFormSubmit = true
				}
			} else if len(body) > 0 {
				_ = json.Unmarshal(body, &input)
			}
		}

		// Verify HMAC signature when a secret is configured on the task.
		if err := verifyWebhookSignature(spec, r, body); err != nil {
			e.log.Warn("webhook signature verification failed",
				zap.String("path", path),
				zap.String("task", taskID),
				zap.Error(err),
			)
			http.Error(w, "forbidden: "+err.Error(), http.StatusForbidden)
			return
		}

		// Extract a flat string map from the input so it is accessible via
		// params.get() in task scripts (RunOptions.Params), in addition to the
		// raw input being available as the `input` global (RunOptions.Input).
		params := flatStringMap(input)

		// Build the WebhookContext so the persistence layer can apply
		// content-type-aware redaction to the raw body and populate
		// Method/Path/Headers/Query on the stored PersistedInput.
		// For GET requests body is nil; body was already read above for
		// POST/PUT/etc. and is safe to reference here.
		webhookCtx := &pkgruntime.WebhookContext{
			Method:      r.Method,
			Path:        r.URL.Path,
			Headers:     r.Header,
			Query:       r.URL.Query(),
			RawBody:     body,
			ContentType: r.Header.Get("Content-Type"),
		}

		// Default: wait for the run to finish and return the result inline.
		// Pass ?wait=false to fire-and-forget (returns runId immediately).
		async := r.URL.Query().Get("wait") == "false"

		if async {
			runID, err := e.fireAsync(r.Context(), spec, pkgruntime.RunOptions{Input: input, Params: params, WebhookCtx: webhookCtx}, registry.TriggerWebhook)
			if err != nil {
				http.Error(w, "task failed to start", http.StatusInternalServerError)
				return
			}
			w.Header().Set("X-Run-Id", runID)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"runId": runID})
			return
		}

		runID, result, err := e.fireSync(spec, pkgruntime.RunOptions{Input: input, Params: params, WebhookCtx: webhookCtx}, registry.TriggerWebhook)
		if err != nil {
			http.Error(w, "task failed to start", http.StatusInternalServerError)
			return
		}

		// Browser form submissions redirect to the run result page.
		if isFormSubmit {
			http.Redirect(w, r, "/runs/"+runID+"/result", http.StatusSeeOther)
			return
		}

		// Return structured output or return value directly when available.
		if result.OutputContent != "" {
			ct := result.OutputContentType
			if ct == "" {
				ct = "text/plain"
			}
			w.Header().Set("Content-Type", ct+"; charset=utf-8")
			w.Header().Set("X-Run-Id", runID)
			if result.Error != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
			_, _ = w.Write([]byte(result.OutputContent))
			return
		}
		if result.ReturnValue != nil {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Run-Id", runID)
			if result.Error != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
			_ = json.NewEncoder(w).Encode(result.ReturnValue)
			return
		}

		// No output produced — the task either succeeded silently or threw before
		// calling output.*. Collect logs so we can surface them to the caller.
		var logLines []string
		if logEntries, logErr := e.registry.GetRunLogs(context.Background(), runID); logErr == nil {
			for _, le := range logEntries {
				logLines = append(logLines, le.Message)
			}
		}

		if result.Error != nil {
			errMsg := result.Error.Error()
			// Browser: render an error page using the same log style as the webui.
			if strings.Contains(r.Header.Get("Accept"), "text/html") {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("X-Run-Id", runID)
				w.WriteHeader(http.StatusInternalServerError)
				logsJSON, _ := json.Marshal(logLines)
				var safeJSON bytes.Buffer
				json.HTMLEscape(&safeJSON, logsJSON)
				_, _ = fmt.Fprintf(w, taskErrorPage, html.EscapeString(runID), html.EscapeString(errMsg), safeJSON.String())
				return
			}
			// API: JSON envelope with error message.
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Run-Id", runID)
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"runId":  runID,
				"status": "failure",
				"error":  errMsg,
				"logs":   logLines,
			})
			return
		}

		// Successful run with no output.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Run-Id", runID)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"runId":  runID,
			"status": "success",
			"logs":   logLines,
		})
	})
}

// flatStringMap converts a map[string]interface{} into a map[string]string by
// formatting each value with %v. Returns nil if input is not a flat map.
func flatStringMap(v interface{}) map[string]string {
	m, ok := v.(map[string]interface{})
	if !ok || len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		out[k] = fmt.Sprintf("%v", val)
	}
	return out
}

// injectDicodeSDK injects the dicode client SDK script and context meta tags
// into an HTML page's <head>, allowing the page to use window.dicode.
//
// A <base> tag with a trailing slash is also injected so that relative URLs
// in the task's HTML (e.g. href="style.css") resolve to the correct sub-path
// (e.g. /hooks/my-task/style.css) regardless of the page having no trailing
// slash in its URL.
//
// When the request arrives via the relay proxy, the X-Relay-Base header
// provides the relay path prefix (e.g. /u/<uuid>) so that <base href> and
// script sources are adjusted to work through the relay.
// validRelayBaseRe matches only /u/<64-hex-chars> to prevent header injection.
var validRelayBaseRe = regexp.MustCompile(`^/u/[0-9a-f]{64}$`)

func isValidRelayBase(s string) bool {
	return validRelayBaseRe.MatchString(s)
}

func injectDicodeSDK(html, hookPath, taskID string, r *http.Request) string {
	relayBase := r.Header.Get("X-Relay-Base")
	// Only accept relay base paths matching /u/<64-hex-chars>.
	if relayBase != "" && !isValidRelayBase(relayBase) {
		relayBase = ""
	}
	basePath := hookPath
	dicodeJSSrc := "/dicode.js"
	if relayBase != "" {
		basePath = relayBase + hookPath
		dicodeJSSrc = relayBase + "/dicode.js"
	}

	injection := `<base href="` + basePath + `/">` +
		`<meta name="dicode-task" content="` + taskID + `">` +
		`<meta name="dicode-hook" content="` + basePath + `">` +
		`<script src="` + dicodeJSSrc + `"></script>`
	// Inject immediately after <head> so <base> precedes every other element
	// (stylesheets, scripts, images) that carries a relative URL.
	if i := strings.Index(html, "<head>"); i != -1 {
		after := i + len("<head>")
		return html[:after] + "\n" + injection + html[after:]
	}
	// Fallback for pages without a <head> tag.
	return injection + "\n" + html
}

// allowedAssetTypes maps file extensions to their Content-Type for webhook UI assets.
var allowedAssetTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "application/javascript; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
}

// serveTaskAsset serves a static asset file from a webhook task's directory.
// Access is sandboxed: only known file types are served and path traversal is blocked.
func (e *Engine) serveTaskAsset(w http.ResponseWriter, r *http.Request, taskDir, assetPath string) {
	// Block path traversal before filepath.Clean can resolve it.
	if strings.Contains(assetPath, "..") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	clean := filepath.Clean(assetPath)
	if filepath.IsAbs(clean) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	ct, allowed := allowedAssetTypes[strings.ToLower(filepath.Ext(clean))]
	if !allowed {
		http.Error(w, "file type not allowed", http.StatusForbidden)
		return
	}

	fullPath := filepath.Join(taskDir, clean)
	// Double-check the resolved path is still inside taskDir.
	if !strings.HasPrefix(fullPath, filepath.Clean(taskDir)+string(filepath.Separator)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", ct)
	_, _ = w.Write(data)
}

// startRun creates the DB record, stores the cancel func, fires the started
// hook, and returns a ready-to-run context. The caller is responsible for
// calling the returned cleanup func when the run finishes.
func (e *Engine) startRun(spec *task.Spec, opts *pkgruntime.RunOptions, source registry.TriggerSource) (runCtx context.Context, cleanup func(), err error) {
	if _, err = e.registry.StartRunWithID(context.Background(), opts.RunID, spec.ID, opts.ParentRunID, string(source)); err != nil {
		return nil, nil, fmt.Errorf("start run record: %w", err)
	}

	// Best-effort input persistence. Failures do not block the run — the
	// auto-fix loop (#234) handles missing inputs via ErrInputUnavailable.
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
			// Log only a sanitized error category. The full perr chain may
			// transit env-resolver internals where CodeQL tracks a
			// secretKey taint label; emitting it raw causes a false-positive
			// go/clear-text-logging alert. The category is enough for ops to
			// triage; full error is available via the failed task's own logs.
			e.log.Warn("run-input persist failed",
				zap.String("run", opts.RunID),
				zap.String("task", spec.ID),
				zap.String("error_class", "persist"),
			)
		} else {
			// Bound RAM exposure: RawBody is no longer needed now that the
			// blob has been persisted. Nil it out so the slice can be GC'd
			// rather than held for the full run lifetime.
			if opts.WebhookCtx != nil {
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
	runCtx, cancel = context.WithCancel(context.Background())
	e.runCancels.Store(opts.RunID, cancel)
	e.runTriggerSource.Store(opts.RunID, source)

	// Register a completion channel for WaitRun. The channel is closed (not
	// sent to) so that multiple concurrent waiters are all unblocked at once.
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
		// Signal all waiters that the run has finished, then remove the entry.
		if v, ok := e.runDone.LoadAndDelete(opts.RunID); ok {
			close(v.(chan struct{}))
		}
		// Defer deletion of the suppressed-persistence return-value cache:
		// WaitRun goroutines woken by the runDone close above need time to
		// scan runReturnValue before the entry is removed. The map is only
		// populated for `run_result.enabled: false` tasks, so this AfterFunc
		// is a no-op for the common case. Bounded by runReturnValueTTL so
		// orphaned entries (no waiter ever calls WaitRun) don't leak.
		runID := opts.RunID
		time.AfterFunc(runReturnValueTTL, func() {
			e.runReturnValue.Delete(runID)
		})
	}
	return runCtx, cleanup, nil
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
		_ = e.registry.FinishRunWithReason(context.Background(), opts.RunID, preStatus, preReason)
		// dispatch normally fires FireChain; on the preflight short-circuit
		// we replicate it so chain triggers still observe the failure.
		go e.FireChain(context.Background(), spec.ID, opts.RunID, preStatus, nil)
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

	notifyOnSuccess, notifyOnFailure := e.resolveNotify(spec)

	if e.notifier != nil {
		shouldNotify := (status == registry.StatusSuccess && notifyOnSuccess) ||
			(status == registry.StatusFailure && notifyOnFailure)
		if shouldNotify {
			msg := notify.Message{
				Title: fmt.Sprintf("[dicode] %s %s", spec.Name, status),
				Body:  fmt.Sprintf("Run finished in %.1fs", elapsed.Seconds()),
			}
			if status == registry.StatusFailure {
				msg.Priority = notify.PriorityHigh
			}
			go func() {
				if err := e.notifier.Send(context.Background(), msg); err != nil {
					e.log.Warn("notification send failed", zap.Error(err))
				}
			}()
		}
	}

	if h := e.runFinishedHook; h != nil {
		h(spec.ID, opts.RunID, status, string(source), elapsed.Milliseconds(), notifyOnSuccess, notifyOnFailure)
	}

	if spec.Trigger.Daemon {
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
		// Duration is 0 — the run never executed. The notifier (wired via
		// a separate path in runTask) only fires on Success/Failure, so
		// cancelled runs do not send spurious notifications.
		notifyOnSuccess, notifyOnFailure := e.resolveNotify(spec)
		h(spec.ID, opts.RunID, registry.StatusCancelled, string(source), 0, notifyOnSuccess, notifyOnFailure)
	}
}

// fireAsync pre-creates the run record, starts execution in a goroutine,
// and returns the run ID immediately.
//
// When a MaxConcurrentTasks semaphore is configured, the goroutine blocks
// until a slot is available or the shutdown context is cancelled — ensuring
// shutdown never deadlocks waiting tasks.
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
func (e *Engine) fireSync(spec *task.Spec, opts pkgruntime.RunOptions, source registry.TriggerSource) (string, *pkgruntime.RunResult, error) {
	opts.RunID = uuid.New().String()

	runCtx, cleanup, err := e.startRun(spec, &opts, source)
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
			prereqRunID, result, fireErr := e.fireSync(prereqSpec, pkgruntime.RunOptions{
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
		_ = e.registry.FinishRun(context.Background(), opts.RunID, registry.StatusFailure)
		return registry.StatusFailure, &pkgruntime.RunResult{Error: fmt.Errorf("no executor for runtime %s", spec.Runtime)}
	}

	if err := e.resolveIfMissing(ctx, spec, opts.RunID); err != nil {
		e.log.Warn("if_missing prereq unsatisfied",
			zap.String("task", spec.ID),
			zap.Error(err),
		)
		_ = e.registry.FinishRun(context.Background(), opts.RunID, registry.StatusFailure)
		return registry.StatusFailure, &pkgruntime.RunResult{Error: err}
	}

	result, err := exec.Execute(ctx, spec, opts)
	if err != nil {
		e.log.Error("executor error",
			zap.String("task", spec.ID),
			zap.String("runtime", string(spec.Runtime)),
			zap.Error(err),
		)
		_ = e.registry.FinishRun(context.Background(), opts.RunID, registry.StatusFailure)
		return registry.StatusFailure, &pkgruntime.RunResult{Error: err}
	}

	// Store return value and structured output if present.
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
	if result != nil && (result.ReturnValue != nil || result.OutputContent != "") {
		retJSON := ""
		if result.ReturnValue != nil {
			if b, merr := json.Marshal(result.ReturnValue); merr == nil {
				retJSON = string(b)
			}
		}
		persistReturnValue := spec.RunResult.PersistReturnValue()
		// When persistence is suppressed, stash the in-memory copy so
		// WaitRun can still serve it to synchronous callers
		// (dicode.run_task -> IPC reply). Done BEFORE the DB write
		// (skipped below) so a WaitRun caller racing against the runDone
		// close never observes an empty value: dispatch is called from
		// runTask synchronously, the runDone channel is closed only by
		// startRun's cleanup func which runs after runTask returns, and
		// the cleanup defers the runReturnValue deletion to give
		// post-close WaitRun goroutines time to scan the map.
		//
		// Common case (persistence enabled) takes no in-memory slot — the
		// DB row carries the value as before.
		persistedReturnJSON := retJSON
		if !persistReturnValue {
			persistedReturnJSON = ""
			if retJSON != "" {
				e.runReturnValue.Store(opts.RunID, retJSON)
			}
		}
		// Skip the SetRunResult call entirely when nothing would be
		// written (e.g. return-value persistence disabled AND no
		// structured output) to avoid a needless UPDATE statement.
		if persistedReturnJSON != "" || result.OutputContent != "" {
			_ = e.registry.SetRunResult(context.Background(), opts.RunID, persistedReturnJSON, result.OutputContentType, result.OutputContent)
		}
	}

	status := registry.StatusSuccess
	if result.Error != nil {
		if ctx.Err() != nil {
			status = registry.StatusCancelled
		} else {
			status = registry.StatusFailure
		}
	}

	e.FireChain(context.Background(), spec.ID, opts.RunID, status, result.ChainInput)
	return status, result
}

// stormScope returns the namespace used for per-source storm tracking:
// everything before the last '/' in taskID, OR taskID itself for a flat
// (no-slash) task. Flat tasks get their own per-task scope so a single
// flapping flat task cannot suppress on_failure_chain for unrelated
// flat tasks under a shared "global" scope.
func stormScope(taskID string) string {
	if i := strings.LastIndex(taskID, "/"); i > 0 {
		return taskID[:i]
	}
	if taskID == "" {
		return "global"
	}
	return taskID
}

// effectiveCooldown returns the configured cooldown or 10 minutes when zero.
func effectiveCooldown(s task.OnFailureChainSpec) time.Duration {
	if s.Cooldown > 0 {
		return s.Cooldown
	}
	return 10 * time.Minute
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
