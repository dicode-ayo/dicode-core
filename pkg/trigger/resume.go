package trigger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
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
//
// #570: when a ResumeStateStore is wired and result.ResumeState exceeds
// resumeStateThresholdBytes, the state is durably written to the storage task
// FIRST and only its {store,key} handle is persisted on the runs row — never
// the reverse order. If the blob write fails, suspendRun returns an error
// without ever calling SuspendRun, so the run fails loudly (dispatch's caller
// marks it StatusFailure) instead of landing a suspended row with a reference
// to nothing.
func (e *Engine) suspendRun(opts *pkgruntime.RunOptions, result *pkgruntime.RunResult) (bool, error) {
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
	// resumes with the same ctx.params and the same chain-depth ceiling.
	var carryJSON []byte
	carry := resumeCarry{Params: opts.Params, ChainDepth: e.chainDepth(opts.RunID)}
	if len(carry.Params) > 0 || carry.ChainDepth > 0 {
		if carryJSON, err = json.Marshal(carry); err != nil {
			return false, fmt.Errorf("marshal resume params: %w", err)
		}
	}

	state := result.ResumeState
	var blobRef *registry.ResumeStateBlobRef
	if e.resumeStateStore != nil && len(state) > e.resumeStateThresholdBytes {
		key, size, storedAt, perr := e.resumeStateStore.Persist(context.Background(), opts.RunID, state)
		if perr != nil {
			// Fail loudly: no SuspendRun call follows, so no row ever points at
			// a blob that doesn't exist.
			return false, fmt.Errorf("suspend: offload resume state to storage: %w", perr)
		}
		blobRef = &registry.ResumeStateBlobRef{StorageKey: key, Size: size, StoredAt: storedAt}
		state = nil // the real state now lives in the blob; don't also write it inline
	}

	return e.registry.SuspendRun(context.Background(), opts.RunID,
		state, result.ResumeSchema, token, nowMs, deadlineMs, carryJSON, blobRef)
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

	// #570: rehydrate an offloaded resume state BEFORE consuming the token —
	// same rationale as the fire-guard probe above. If the blob is missing or
	// the store isn't wired, resume must fail loudly rather than hand the
	// resumed task an empty/wrong state, and the token must stay usable so a
	// retry (once storage is fixed) can still succeed. The caller-facing
	// contract is unchanged either way: opts.ResumeState always ends up
	// holding the full original state, never the internal {store,key} handle.
	resumeState := run.ResumeState
	if run.ResumeStateStorageKey != "" {
		if e.resumeStateStore == nil {
			return "", fmt.Errorf("resume: run %s has an offloaded resume state but no resume-state store is configured", run.ID)
		}
		fetched, ferr := e.resumeStateStore.Fetch(ctx, run.ID, run.ResumeStateStorageKey, run.ResumeStateStoredAt)
		if ferr != nil {
			return "", fmt.Errorf("resume: fetch offloaded resume state: %w", ferr)
		}
		resumeState = fetched
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
		ResumeState: resumeState,
		ResumeInput: input,
	}
	// Restore the original run's fire-time params and chain depth so the
	// continuation sees the same ctx.params (not spec defaults) and stays under
	// the chain-depth ceiling. Chain depth rides in Input because fireAsync reads
	// _chain_depth from there to seed runChainDepth.
	if len(run.ResumeParams) > 0 {
		var carry resumeCarry
		if err := json.Unmarshal(run.ResumeParams, &carry); err != nil {
			return "", fmt.Errorf("resume: decode carried run params: %w", err)
		}
		opts.Params = carry.Params
		if carry.ChainDepth > 0 {
			opts.Input = map[string]any{"_chain_depth": carry.ChainDepth}
		}
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

	// #570: best-effort eager GC. The offloaded blob (if any) was single-use —
	// this specific row's token is now spent and nothing will ever fetch it
	// again — so free it immediately rather than waiting for the TTL sweep.
	// Failure here is non-fatal: the resume already succeeded and the
	// resume-state-cleanup buildin's retention sweep is the backstop for any
	// blob an eager delete missed (daemon restart mid-resume, storage task
	// transiently down, etc).
	if run.ResumeStateStorageKey != "" && e.resumeStateStore != nil {
		if derr := e.resumeStateStore.Delete(context.Background(), run.ResumeStateStorageKey); derr != nil {
			e.log.Warn("resume: eager resume-state blob delete failed; TTL sweep will retry",
				zap.String("run", run.ID), zap.Error(derr))
		} else if cerr := e.registry.ClearResumeStateBlob(context.Background(), run.ID); cerr != nil {
			e.log.Warn("resume: clear resume-state blob columns failed",
				zap.String("run", run.ID), zap.Error(cerr))
		}
	}

	return newRunID, nil
}
