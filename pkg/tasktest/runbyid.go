package tasktest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dicode/dicode/pkg/task"
)

// SpecLookup is the registry-shaped interface RunByID needs. Defined locally
// (rather than depending on pkg/registry) so tasktest stays free of the
// registry's database+sync overhead and so callers in tests can stub it.
type SpecLookup interface {
	Get(id string) (*task.Spec, bool)
}

// ErrTaskNotFound is returned by RunByID when the task ID is unknown to the
// supplied registry. Callers (HTTP handlers, MCP wrappers) map this to 404.
var ErrTaskNotFound = errors.New("tasktest: task not found")

// ErrTimeout signals that the configured timeout elapsed before the test
// runner finished. Callers map this to 408 Request Timeout. The accompanying
// Result still carries whatever output was captured before cancellation.
var ErrTimeout = errors.New("tasktest: timeout exceeded")

// ErrParamsInvalid wraps a per-field task.ParamErrors aggregate. Callers
// (HTTP handlers) map this to 422 Unprocessable Entity and surface the
// FieldErrors slice in the response body. errors.As(err, &task.ParamErrors{})
// recovers the original aggregate.
type ErrParamsInvalid struct {
	FieldErrors task.ParamErrors
}

func (e *ErrParamsInvalid) Error() string {
	return fmt.Sprintf("tasktest: %s", e.FieldErrors.Error())
}

func (e *ErrParamsInvalid) Unwrap() error { return e.FieldErrors }

// RunByID is the shared entry point for the test harness used by the
// REST endpoint (POST /api/tasks/{id}/test) and the CLI control-socket
// (cli.task.test). It encapsulates:
//
//  1. Registry lookup → ErrTaskNotFound on miss.
//  2. Params validation against the task's declared schema → *ErrParamsInvalid.
//  3. Timeout enforcement via context.WithTimeout when timeout > 0.
//  4. Delegation to Run.
//  5. Mapping context.DeadlineExceeded → ErrTimeout while still returning
//     the partial Result the runner captured before cancellation.
//
// timeout <= 0 means "use the parent context unchanged" — callers that want
// the runner to inherit a server-wide deadline should pass 0 and bind their
// own ctx upstream.
//
// The validated-and-coerced params are returned alongside the Result so
// callers (or future SDK plumbing once tasktest.Run learns to forward them
// to the runner) can inspect what was actually sent. Today the params are
// validated but not yet forwarded to the Deno runner — see issue #208.
func RunByID(ctx context.Context, reg SpecLookup, taskID string, params map[string]any, timeout time.Duration) (Result, map[string]string, error) {
	if reg == nil {
		return Result{}, nil, fmt.Errorf("tasktest: registry is nil")
	}
	spec, ok := reg.Get(taskID)
	if !ok {
		return Result{TaskID: taskID}, nil, ErrTaskNotFound
	}

	coerced, perrs := task.ValidateParams(spec.Params, params)
	if perrs != nil {
		return Result{TaskID: taskID}, nil, &ErrParamsInvalid{FieldErrors: perrs}
	}

	runCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	res, err := Run(runCtx, spec)
	// A deadline trip surfaces here either as runCtx.Err() (when Run returns
	// nil because the runner cleaned up gracefully) or via the wrapped exec
	// error. Normalise both paths to ErrTimeout so handlers don't have to
	// special-case exec.ExitError vs context.DeadlineExceeded.
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return res, coerced, ErrTimeout
	}
	return res, coerced, err
}
