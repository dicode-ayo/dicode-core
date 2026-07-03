// This file holds the shared building blocks of the "socket-bridge" runtimes
// (Deno, Python): subprocess runtimes that talk to the daemon over a per-run
// Unix socket (pkg/ipc). The logic here used to be copy-pasted between
// pkg/runtime/deno and pkg/runtime/python (issue #388); it lives in this
// package alongside SubprocessEnv, following the same precedent.
package runtime

import (
	"context"
	"time"

	"github.com/dicode/dicode/pkg/task"
)

// MergeParams merges trigger-time param overrides over the spec's declared
// defaults. Declared params with an empty default are omitted; overrides win
// on duplicate names and may introduce names not declared in the spec.
func MergeParams(specParams []task.Param, overrides map[string]string) map[string]string {
	out := make(map[string]string, len(specParams))
	for _, p := range specParams {
		if p.Default != "" {
			out[p.Name] = p.Default
		}
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// ExecContext returns the context and cancel function for a task execution.
// A positive timeout creates a child context with that deadline. A zero or
// negative timeout wraps the parent in WithCancel so the caller can still
// cancel early but no new deadline is imposed (issue #389 unified this
// semantics across the Deno and Python runtimes).
func ExecContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(parent, timeout)
	}
	return context.WithCancel(parent)
}
