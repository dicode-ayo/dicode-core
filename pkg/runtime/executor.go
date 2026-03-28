// Package runtime defines the common executor interface used by all runtimes.
package runtime

import (
	"context"

	"github.com/dicode/dicode/pkg/task"
)

// RunOptions controls a single task execution.
type RunOptions struct {
	RunID       string
	ParentRunID string
	Params      map[string]string
	Input       interface{}
}

// RunResult is returned by every Executor.
type RunResult struct {
	RunID      string
	ChainInput interface{} // passed to FireChain; nil for runtimes that don't produce output
	Error      error
}

// Executor is the common interface satisfied by every runtime (Deno, Docker,
// subprocess interpreters, etc.).
type Executor interface {
	Execute(ctx context.Context, spec *task.Spec, opts RunOptions) (*RunResult, error)
}
