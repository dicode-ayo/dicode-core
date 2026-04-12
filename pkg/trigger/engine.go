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
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/notify"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

// ErrRunNotFound is returned by WaitRun when no run record exists for the given ID.
var ErrRunNotFound = errors.New("run not found")

// Engine coordinates all trigger types and fires task runs.
type Engine struct {
	registry  *registry.Registry
	executors map[task.Runtime]pkgruntime.Executor
	cron      *cron.Cron
	log       *zap.Logger

	mu          sync.Mutex
	cronEntries map[string]cron.EntryID // taskID → cron entry
	webhooks    map[string]string       // webhook path → taskID

	runCancels sync.Map // runID → context.CancelFunc
	runDone    sync.Map // runID (string) → chan struct{}, closed when the run reaches a terminal state

	shutdownMu  sync.RWMutex
	shutdownCtx context.Context

	daemonMu    sync.Mutex
	daemonRuns  map[string]string
	daemonSpecs map[string]*task.Spec

	notifier        notify.Notifier
	notifyOnSuccess bool
	notifyOnFailure bool

	defaultsOnFailureChain string // from config.Defaults.OnFailureChain

	db db.DB // optional — enables cron-job persistence and missed-run catchup

	runFinishedHook func(taskID, runID, status, triggerSource string, durationMs int64, notifyOnSuccess, notifyOnFailure bool)
	runStartedHook  func(taskID, runID, triggerSource string)
}

// New creates a trigger Engine with a default Deno executor.
func New(r *registry.Registry, defaultExec pkgruntime.Executor, log *zap.Logger) *Engine {
	e := &Engine{
		registry:    r,
		executors:   make(map[task.Runtime]pkgruntime.Executor),
		cron:        cron.New(),
		log:         log,
		cronEntries: make(map[string]cron.EntryID),
		webhooks:    make(map[string]string),
		daemonRuns:  make(map[string]string),
		daemonSpecs: make(map[string]*task.Spec),
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

// SetDefaultsOnFailureChain sets the global task ID to fire when any task fails.
// Corresponds to config.Defaults.OnFailureChain. Per-task on_failure_chain overrides this.
func (e *Engine) SetDefaultsOnFailureChain(taskID string) {
	e.defaultsOnFailureChain = taskID
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

// Register adds or updates trigger registrations for a task spec.
func (e *Engine) Register(spec *task.Spec) {
	e.Unregister(spec.ID)

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
		zap.String("trigger", triggerSource(spec)),
		zap.String("runtime", string(spec.Runtime)),
	)
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
		if _, ferr := e.fireAsync(context.Background(), s, pkgruntime.RunOptions{}, "cron"); ferr == nil && e.db != nil {
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
		e.fireAsync(ctx, spec, pkgruntime.RunOptions{}, "cron-catchup") //nolint:errcheck
	}
}

func (e *Engine) registerWebhook(spec *task.Spec) {
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
	e.startDaemon(spec)
}

func (e *Engine) startDaemon(spec *task.Spec) {
	runID, err := e.fireAsync(context.Background(), spec, pkgruntime.RunOptions{}, "daemon")
	if err != nil {
		e.log.Error("daemon start failed", zap.String("task", spec.ID), zap.Error(err))
		return
	}
	e.daemonMu.Lock()
	e.daemonRuns[spec.ID] = runID
	e.daemonMu.Unlock()
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
	return e.fireAsync(context.Background(), spec, pkgruntime.RunOptions{Params: params}, "manual")
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
	var returnValue interface{}
	if run.ReturnValue != "" {
		_ = json.Unmarshal([]byte(run.ReturnValue), &returnValue)
	}
	return ipc.RunResult{
		RunID:       runID,
		Status:      run.Status,
		ReturnValue: returnValue,
	}, nil
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
		e.log.Info("chain trigger",
			zap.String("from", completedTaskID),
			zap.String("to", spec.ID),
			zap.String("on", on),
		)
		go e.fireAsync(ctx, spec, pkgruntime.RunOptions{Input: output}, "chain") //nolint:errcheck
	}

	// Config-level default on_failure_chain.
	if runStatus == "failure" {
		targetID := e.defaultsOnFailureChain
		if failedSpec, ok := e.registry.Get(completedTaskID); ok {
			if failedSpec.OnFailureChain != nil {
				targetID = *failedSpec.OnFailureChain // "" disables, "other-id" overrides
			}
		}
		if targetID != "" && targetID != completedTaskID {
			if targetSpec, ok := e.registry.Get(targetID); ok {
				e.log.Info("on_failure_chain trigger",
					zap.String("from", completedTaskID),
					zap.String("to", targetID),
					zap.String("run", runID),
				)
				go e.fireAsync(ctx, targetSpec, pkgruntime.RunOptions{ //nolint:errcheck
					Input: map[string]interface{}{
						"taskID": completedTaskID,
						"runID":  runID,
						"status": runStatus,
						"output": output,
					},
				}, "chain")
			}
		}
	}
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
			// Prefix match — request is for a static asset belonging to a webhook UI.
			for hookPath, tid := range e.webhooks {
				if strings.HasPrefix(path, hookPath+"/") {
					taskID = tid
					matchedHook = hookPath
					assetPath = path[len(hookPath)+1:]
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

		// Default: wait for the run to finish and return the result inline.
		// Pass ?wait=false to fire-and-forget (returns runId immediately).
		async := r.URL.Query().Get("wait") == "false"

		if async {
			runID, err := e.fireAsync(r.Context(), spec, pkgruntime.RunOptions{Input: input, Params: params}, "webhook")
			if err != nil {
				http.Error(w, "task failed to start", http.StatusInternalServerError)
				return
			}
			w.Header().Set("X-Run-Id", runID)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"runId": runID})
			return
		}

		runID, result, err := e.fireSync(spec, pkgruntime.RunOptions{Input: input, Params: params}, "webhook")
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
func (e *Engine) startRun(spec *task.Spec, opts *pkgruntime.RunOptions, source string) (runCtx context.Context, cleanup func(), err error) {
	if _, err = e.registry.StartRunWithID(context.Background(), opts.RunID, spec.ID, opts.ParentRunID, source); err != nil {
		return nil, nil, fmt.Errorf("start run record: %w", err)
	}
	if h := e.runStartedHook; h != nil {
		h(spec.ID, opts.RunID, source)
	}
	var cancel context.CancelFunc
	runCtx, cancel = context.WithCancel(context.Background())
	e.runCancels.Store(opts.RunID, cancel)

	// Register a completion channel for WaitRun. The channel is closed (not
	// sent to) so that multiple concurrent waiters are all unblocked at once.
	doneCh := make(chan struct{})
	e.runDone.Store(opts.RunID, doneCh)

	cleanup = func() {
		e.runCancels.Delete(opts.RunID)
		cancel()
		// Signal all waiters that the run has finished, then remove the entry.
		if v, ok := e.runDone.LoadAndDelete(opts.RunID); ok {
			close(v.(chan struct{}))
		}
	}
	return runCtx, cleanup, nil
}

// runTask executes a task synchronously and handles all post-run bookkeeping
// (logging, notifications, hooks, daemon restart). Returns status and result.
func (e *Engine) runTask(runCtx context.Context, spec *task.Spec, opts pkgruntime.RunOptions, source string) (string, *pkgruntime.RunResult) {
	e.log.Info("run started",
		zap.String("task", spec.ID),
		zap.String("run", opts.RunID),
		zap.String("trigger", source),
		zap.String("runtime", string(spec.Runtime)),
	)

	start := time.Now()
	status, result := e.dispatch(runCtx, spec, opts)
	elapsed := time.Since(start)

	runFields := []zap.Field{
		zap.String("task", spec.ID),
		zap.String("run", opts.RunID),
		zap.String("status", status),
		zap.String("trigger", source),
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
		h(spec.ID, opts.RunID, status, source, elapsed.Milliseconds(), notifyOnSuccess, notifyOnFailure)
	}

	if spec.Trigger.Daemon {
		e.onDaemonRunFinished(spec, opts.RunID)
	}

	return status, result
}

// fireAsync pre-creates the run record, starts execution in a goroutine,
// and returns the run ID immediately.
func (e *Engine) fireAsync(ctx context.Context, spec *task.Spec, opts pkgruntime.RunOptions, source string) (string, error) {
	opts.RunID = uuid.New().String()

	runCtx, cleanup, err := e.startRun(spec, &opts, source)
	if err != nil {
		return "", err
	}

	go func() {
		defer cleanup()
		e.runTask(runCtx, spec, opts, source)
	}()

	return opts.RunID, nil
}

// fireSync runs the task synchronously and returns the run ID and result.
// The caller's context is used only for cancellation of the run setup; the
// run itself uses an independent context so it is not cancelled when the HTTP
// request context ends mid-execution.
func (e *Engine) fireSync(spec *task.Spec, opts pkgruntime.RunOptions, source string) (string, *pkgruntime.RunResult, error) {
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
	if result != nil && (result.ReturnValue != nil || result.OutputContent != "") {
		retJSON := ""
		if result.ReturnValue != nil {
			if b, merr := json.Marshal(result.ReturnValue); merr == nil {
				retJSON = string(b)
			}
		}
		_ = e.registry.SetRunResult(context.Background(), opts.RunID, retJSON, result.OutputContentType, result.OutputContent)
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

// triggerSource returns a short string identifying the trigger type of a spec.
func triggerSource(spec *task.Spec) string {
	switch {
	case spec.Trigger.Cron != "":
		return "cron"
	case spec.Trigger.Webhook != "":
		return "webhook"
	case spec.Trigger.Daemon:
		return "daemon"
	case spec.Trigger.Chain != nil:
		return "chain"
	default:
		return "manual"
	}
}
