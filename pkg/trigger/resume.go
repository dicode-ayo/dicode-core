package trigger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/runinput"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// resumeCarry is the fire-time context of a suspended run that must survive the
// suspend→resume hop: the param overrides the run was fired with (so the
// continuation sees the same ctx.params instead of reverting to spec defaults)
// and the chain depth (so the chain-depth ceiling is not reset by a suspend).
// Persisted as JSON in runs.resume_params at suspend, restored in ResumeRun.
type resumeCarry struct {
	Params     map[string]string `json:"params,omitempty"`
	ChainDepth int               `json:"chain_depth,omitempty"`
}

// defaultResumeTTL is how long a suspended run stays resumable when the task
// did not specify its own deadline. After it, the sweep cancels the run with
// ReasonResumeTimeout.
const defaultResumeTTL = 24 * time.Hour

// secretResolveTimeout bounds each secrets-chain lookup during resume-param
// redaction/restoration, the same way ifMissingPrereqTimeout bounds the
// secrets-chain check in resolveIfMissing: a network-backed provider (Vault,
// AWS SM, …) that hangs must not block a suspend or resume indefinitely.
const secretResolveTimeout = 10 * time.Second

// Resume errors surfaced to callers (webui/CLI in later PRs).
var (
	// ErrResumeTokenNotFound is returned when no suspended run carries the token.
	ErrResumeTokenNotFound = errors.New("resume token not found")
	// ErrResumeNotSuspended is returned when the token's run is no longer
	// suspended — already resumed (single-use token replayed), cancelled, or
	// finished.
	ErrResumeNotSuspended = errors.New("run is not suspended")
	// ErrResumeExpired is returned when the run's resume_deadline has passed.
	// The run is swept to cancelled/resume_timeout as a side effect.
	ErrResumeExpired = errors.New("resume deadline expired")
	// ErrResumePending is returned when the fire guard vetoes the continuation
	// — typically the trust-on-change approval gate holding the task pending
	// after an edit. The token is NOT consumed and the run stays suspended, so
	// it resumes once the task is re-approved. The underlying guard error
	// (e.g. approval.ErrPending) is wrapped for callers that inspect it.
	ErrResumePending = errors.New("resume blocked: task not admitted")
)

// suspendRun persists a run that called dicode.suspend() as suspended: it mints
// an unguessable single-use resume token and records the state/form blobs plus
// the deadline. The deadline comes from the task (result.ResumeDeadline) or
// defaults to defaultResumeTTL from now.
// Reports whether the run was actually suspended: false (with nil error) means
// a concurrent finalize already moved it out of `running`, so no resume state
// was persisted and the caller must not report it as suspended.
func (e *Engine) suspendRun(spec *task.Spec, opts *pkgruntime.RunOptions, result *pkgruntime.RunResult) (bool, error) {
	token, err := newResumeToken()
	if err != nil {
		return false, fmt.Errorf("mint resume token: %w", err)
	}
	nowMs := time.Now().UnixMilli()
	deadlineMs := result.ResumeDeadline
	if deadlineMs <= 0 {
		deadlineMs = time.Now().Add(defaultResumeTTL).UnixMilli()
	}
	// Persist the run's fire-time params and chain depth so the continuation
	// resumes with the same ctx.params and the same chain-depth ceiling. Any
	// param value that matches a live secrets-chain entry the task's own
	// permissions.env declares under that same name is redacted first —
	// ResumeRun restores it from the chain rather than trusting the stored
	// value.
	redactedParams, redactedFields, err := e.redactSecretBackedParams(context.Background(), spec, opts.RunID, opts.Params)
	if err != nil {
		return false, fmt.Errorf("redact resume params: %w", err)
	}
	var carryJSON []byte
	carry := resumeCarry{Params: redactedParams, ChainDepth: e.chainDepth(opts.RunID)}
	if len(carry.Params) > 0 || carry.ChainDepth > 0 {
		if carryJSON, err = json.Marshal(carry); err != nil {
			return false, fmt.Errorf("marshal resume params: %w", err)
		}
	}
	return e.registry.SuspendRun(context.Background(), opts.RunID,
		result.ResumeState, result.ResumeSchema, token, nowMs, deadlineMs, carryJSON, redactedFields)
}

// secretBackedParamNames returns the set of param names spec's own
// permissions.env grants as secrets store keys (EnvEntry.Secret) — the only
// names redactSecretBackedParams/restoreSecretBackedParams may ever treat as
// secret-backed. A task with no such grant for a name has no standing to
// have that name resolved against the global secrets chain, regardless of
// how the name looks or what value it happens to carry.
func secretBackedParamNames(spec *task.Spec) map[string]struct{} {
	allowed := make(map[string]struct{})
	if spec == nil {
		return allowed
	}
	for _, entry := range spec.Permissions.Env {
		if entry.Secret == "" {
			continue
		}
		allowed[entry.Secret] = struct{}{}
	}
	return allowed
}

// redactSecretBackedParams returns a copy of params (never mutated) with any
// value that exactly matches a live secrets-chain entry under a param name
// spec's own permissions.env declares as a Secret key, replaced by
// runinput.RedactPlaceholder — the only case restoreSecretBackedParams can
// recover later, by re-resolving the same key. A param whose name is not
// among spec's granted secret keys is never resolved against the secrets
// chain at all. A granted name whose value has no live secrets-chain match
// is left as-is: dicode has no other way to recover a literal fire-time
// value that never came from the secrets store, and a resumed run must never
// see a value it never actually had.
//
// A real (non-NotFound) error resolving a granted name aborts the whole pass
// with an error rather than leaving that param unredacted: the caller cannot
// tell whether the value matches the secret, and persisting it unredacted on
// that ambiguity would be the exact disclosure this redaction exists to
// prevent. Mirrors resolveIfMissing's own hard-fail on a real secrets error.
//
// Returns the (possibly copied) params map and the dotted "params.<name>"
// paths that were actually redacted, sorted for determinism.
func (e *Engine) redactSecretBackedParams(ctx context.Context, spec *task.Spec, runID string, params map[string]string) (map[string]string, []string, error) {
	if len(params) == 0 || e.secrets == nil {
		return params, nil, nil
	}
	allowed := secretBackedParamNames(spec)
	out := make(map[string]string, len(params))
	names := make([]string, 0, len(params))
	for k, v := range params {
		out[k] = v
		names = append(names, k)
	}
	sort.Strings(names)
	if len(allowed) == 0 {
		return out, nil, nil
	}
	var redacted []string
	for _, name := range names {
		if _, granted := allowed[name]; !granted {
			continue
		}
		resolveCtx, cancel := context.WithTimeout(ctx, secretResolveTimeout)
		secretVal, err := e.secrets.Resolve(resolveCtx, name)
		cancel()
		if err != nil {
			var notFound *secrets.NotFoundError
			if errors.As(err, &notFound) {
				continue
			}
			return nil, nil, fmt.Errorf("resume: redaction check for param %q (run %s, task %q): %w", name, runID, spec.ID, err)
		}
		if secretVal != params[name] {
			continue
		}
		out[name] = runinput.RedactPlaceholder
		redacted = append(redacted, "params."+name)
	}
	return out, redacted, nil
}

// restoreSecretBackedParams reverses redactSecretBackedParams on the resume
// path: for each dotted "params.<name>" path in redactedFields it re-resolves
// name from the secrets chain — the same source the value came from at
// suspend time — instead of trusting the redaction placeholder left in the
// stored blob. params is never mutated. Errors if a redacted field can't be
// restored — including when spec's current permissions.env no longer grants
// that name (e.g. the task was edited between suspend and resume) — because a
// resumed run must never silently run with the literal placeholder, or a
// value it is no longer entitled to, standing in for a real param value.
func (e *Engine) restoreSecretBackedParams(ctx context.Context, spec *task.Spec, params map[string]string, redactedFields []string) (map[string]string, error) {
	if len(redactedFields) == 0 {
		return params, nil
	}
	if e.secrets == nil {
		return nil, fmt.Errorf("resume: %d redacted param(s) but no secrets chain is configured to restore them", len(redactedFields))
	}
	allowed := secretBackedParamNames(spec)
	out := make(map[string]string, len(params))
	for k, v := range params {
		out[k] = v
	}
	for _, field := range redactedFields {
		name, ok := strings.CutPrefix(field, "params.")
		if !ok {
			continue
		}
		if _, granted := allowed[name]; !granted {
			return nil, fmt.Errorf("resume: redacted param %q is no longer granted by task %q's permissions.env; refusing to restore", name, spec.ID)
		}
		resolveCtx, cancel := context.WithTimeout(ctx, secretResolveTimeout)
		v, err := e.secrets.Resolve(resolveCtx, name)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("resume: restore redacted param %q from secrets chain: %w", name, err)
		}
		out[name] = v
	}
	return out, nil
}

// newResumeToken returns a 32-byte crypto/rand token, hex-encoded. Long and
// unpredictable enough to serve as the resume authorization handle.
func newResumeToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ResumeRun consumes a resume token and spawns the continuation run for a
// suspended task. It looks the run up by token, rejects it if not found, not
// suspended, or past its deadline (an expired run is swept to
// cancelled/resume_timeout), verifies the task is still registered and the
// fire guard admits it, marks the original run resumed so the token can't be
// replayed, then fires a fresh run of the same task seeded with the stored
// resume state and the caller's input. The continuation runs the normal
// execution path, so a task that suspends again mints its own token — chaining
// multi-step wizards. Returns the continuation run's ID.
func (e *Engine) ResumeRun(ctx context.Context, token string, input []byte) (string, error) {
	run, err := e.registry.GetRunByResumeToken(ctx, token)
	if err != nil {
		if errors.Is(err, registry.ErrRunNotFound) {
			return "", ErrResumeTokenNotFound
		}
		return "", err
	}
	if run.Status != registry.StatusSuspended {
		return "", ErrResumeNotSuspended
	}
	if run.ResumeDeadline > 0 && time.Now().UnixMilli() > run.ResumeDeadline {
		// Expired: sweep it now so its terminal state reflects the timeout even
		// if the periodic sweep hasn't run yet, then reject. Routed through the
		// engine sweep so the run:finished hook and resume_timeout chain fire.
		if _, serr := e.SweepExpiredSuspensions(ctx, time.Now().UnixMilli()); serr != nil {
			e.log.Warn("resume: sweep expired suspension failed",
				zap.String("run", run.ID), zap.Error(serr))
		}
		return "", ErrResumeExpired
	}

	// Resolve the task BEFORE consuming the token. If it was deregistered or
	// reloaded away between suspend and resume, fail without spending the
	// single-use token or flipping the run out of `suspended` — the suspension
	// stays resumable once the task is back.
	spec, ok := e.registry.Get(run.TaskID)
	if !ok {
		return "", fmt.Errorf("resume: task %q is no longer registered", run.TaskID)
	}

	// Probe the fire guard WITHOUT spawning or consuming the token. The same
	// guard runs again inside the spawn path; checking it here first means a
	// vetoed continuation (e.g. the author edited the task, so the
	// trust-on-change gate holds it pending) leaves the run suspended with its
	// token intact — consuming the token before the veto would strand the
	// resume_state on a terminal `resumed` row forever.
	if gerr := e.checkFireGuard(spec.ID); gerr != nil {
		return "", fmt.Errorf("%w: %w", ErrResumePending, gerr)
	}

	// Restore the original run's fire-time params and chain depth so the
	// continuation sees the same ctx.params (not spec defaults) and stays under
	// the chain-depth ceiling. Done BEFORE consuming the resume token, same as
	// checkFireGuard above: restoration can fail (secrets-chain error, or a
	// redacted field no longer covered by spec's current permissions.env), and
	// a failure after the token is consumed would strand the run resumed with
	// no continuation ever spawned and no way to retry.
	var restoredParams map[string]string
	var chainDepth int
	if len(run.ResumeParams) > 0 {
		var carry resumeCarry
		if err := json.Unmarshal(run.ResumeParams, &carry); err != nil {
			return "", fmt.Errorf("resume: decode carried run params: %w", err)
		}
		restored, err := e.restoreSecretBackedParams(ctx, spec, carry.Params, run.ResumeParamsRedactedFields)
		if err != nil {
			return "", err
		}
		restoredParams = restored
		chainDepth = carry.ChainDepth
	}

	// Consume the token atomically. This is the single-use guard: a second
	// ResumeRun for the same token finds the run already resumed and fails.
	if err := e.registry.MarkRunResumed(ctx, run.ID); err != nil {
		if errors.Is(err, registry.ErrRunNotSuspended) {
			return "", ErrResumeNotSuspended
		}
		return "", fmt.Errorf("mark run resumed: %w", err)
	}

	opts := pkgruntime.RunOptions{
		ParentRunID: run.ID,
		Resumed:     true,
		ResumeState: run.ResumeState,
		ResumeInput: input,
		Params:      restoredParams,
		ChainDepth:  chainDepth,
	}

	// A daemon body's continuation must re-enter the #470 slot accounting: it
	// adopts the slot the suspended body kept reserved so a reconciler reload
	// can't start a second body alongside the continuation, and its
	// onDaemonRunFinished frees the slot correctly. Plain (non-daemon) tasks
	// have no slot to manage and fire directly.
	var newRunID string
	if spec.Trigger.Daemon {
		newRunID, err = e.resumeDaemonBody(spec, run.ID, opts)
	} else {
		newRunID, err = e.fireAsync(context.Background(), spec, opts, registry.TriggerResume)
	}
	if err != nil {
		return "", fmt.Errorf("resume: spawn continuation run: %w", err)
	}
	e.log.Info("run resumed",
		zap.String("task", run.TaskID),
		zap.String("suspended_run", run.ID),
		zap.String("continuation_run", newRunID),
	)
	return newRunID, nil
}
