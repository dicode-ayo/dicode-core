// Package runtime defines the common executor interface used by all runtimes.
package runtime

import (
	"context"

	"github.com/dicode/dicode/pkg/runtime/envresolve"
	"github.com/dicode/dicode/pkg/task"
)

// WebhookContext captures HTTP-level details for a webhook-triggered run.
// Populated by the trigger engine's webhook handler; nil for other trigger
// sources (cron, manual, chain, daemon, replay).
//
// Used by the run-input persistence layer (#233) to apply content-type-aware
// redaction to the request body and HTTP headers/query before storing.
type WebhookContext struct {
	Method      string
	Path        string
	Headers     map[string][]string
	Query       map[string][]string
	RawBody     []byte
	ContentType string
}

// RunOptions controls a single task execution.
type RunOptions struct {
	RunID       string
	ParentRunID string
	Params      map[string]string
	Input       interface{}

	// TriggerActor identifies the operator principal that initiated this
	// run (e.g. the authenticated web session's client IP for manual API
	// runs). Empty for cron/webhook/chain runs. The trigger engine prefers
	// it over ParentRunID as the run_triggered audit actor_id (#45).
	TriggerActor string

	// PreResolvedEnv, when set, is the result of an env-resolver pass run
	// by the trigger engine before dispatch. The runtime uses these values
	// directly instead of calling the resolver itself, avoiding the
	// double-resolve that would otherwise re-spawn provider tasks.
	//
	// When nil (e.g. legacy callers, tests that bypass the engine), the
	// runtime falls back to its own inline-resolver path.
	PreResolvedEnv *envresolve.Resolved

	// WebhookCtx carries the HTTP-level context for webhook-triggered runs.
	// Nil for all other trigger sources (cron, manual, chain, daemon, replay).
	// Used by the run-input persistence layer to call content-type-aware
	// redaction on the raw body and to populate Method/Path/Headers/Query.
	WebhookCtx *WebhookContext

	// ChainParentTask is the failing-task ID that fired this run via
	// on_failure_chain. Empty for non-chained runs. Used by the trigger
	// engine to release the chainGuards slot on completion.
	ChainParentTask string

	// ResumeState / ResumeInput carry a suspended run's prior state and the
	// user's form submission when the engine re-spawns a task to resume it
	// (#95). Both are opaque JSON blobs surfaced to the task as
	// ctx.resume_state / ctx.resume_input. Nil on a first (non-resume) run.
	// Populated by the resume orchestration (PR 3); the runtime only forwards
	// them to the subprocess.
	ResumeState []byte
	ResumeInput []byte
}

// RunResult is returned by every Executor.
type RunResult struct {
	RunID       string
	ChainInput  interface{} // passed to FireChain; nil for runtimes that don't produce output
	ReturnValue interface{} // return value to store/display; nil for container runtimes
	Error       error

	// Structured output (e.g. output.html / output.text from Deno tasks).
	// Empty string means no structured output was produced.
	OutputContentType string
	OutputContent     string

	// Suspended is set when the task called dicode.suspend() (#95): the run
	// paused cleanly (exit 0, not a failure) and yielded state + a form for
	// later resume. ResumeState/ResumeSchema are opaque JSON blobs; ResumeDeadline
	// is an optional Unix-ms TTL (0 = unset). The trigger engine consumes these
	// to persist the suspended run and mint a resume token (PR 3); this runtime
	// only produces them.
	Suspended      bool
	ResumeState    []byte
	ResumeSchema   []byte
	ResumeDeadline int64
}

// Executor is the common interface satisfied by every runtime (Deno, Docker,
// subprocess interpreters, etc.).
type Executor interface {
	Execute(ctx context.Context, spec *task.Spec, opts RunOptions) (*RunResult, error)
}
