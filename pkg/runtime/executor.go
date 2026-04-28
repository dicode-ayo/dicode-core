// Package runtime defines the common executor interface used by all runtimes.
package runtime

import (
	"context"

	"github.com/dicode/dicode/pkg/runtime/envresolve"
	"github.com/dicode/dicode/pkg/task"
)

// RunOptions controls a single task execution.
type RunOptions struct {
	RunID       string
	ParentRunID string
	Params      map[string]string
	Input       interface{}

	// PreResolvedEnv, when set, is the result of an env-resolver pass run
	// by the trigger engine before dispatch. The runtime uses these values
	// directly instead of calling the resolver itself, avoiding the
	// double-resolve that would otherwise re-spawn provider tasks.
	//
	// When nil (e.g. legacy callers, tests that bypass the engine), the
	// runtime falls back to its own inline-resolver path.
	PreResolvedEnv *envresolve.Resolved
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
}

// Executor is the common interface satisfied by every runtime (Deno, Docker,
// subprocess interpreters, etc.).
type Executor interface {
	Execute(ctx context.Context, spec *task.Spec, opts RunOptions) (*RunResult, error)
}
