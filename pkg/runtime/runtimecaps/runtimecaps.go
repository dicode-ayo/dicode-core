// Package runtimecaps is the one place runtime capabilities are declared:
// what each task runtime can do, keyed by its task.yaml runtime name. Code
// outside pkg/runtime asks For(rt) instead of switching on runtime names.
//
// It imports only pkg/task so that packages pkg/runtime itself depends on
// (pkg/ipc) can read it without an import cycle.
package runtimecaps

import "github.com/dicode/dicode/pkg/task"

// RuntimePython is the runtime name of Python tasks.
const RuntimePython task.Runtime = "python"

// Caps describes what a runtime supports.
type Caps struct {
	// Suspend: the runtime reads srv.Suspend() after the task exits to turn
	// dicode.suspend() into a suspended run, so CapSuspend may be granted.
	Suspend bool
	// SecretOutput: the task can send dicode.output(..., {secret: true}) over
	// IPC, so it can act as an env-resolver provider.
	SecretOutput bool
	// LocalImageBuild: the runtime builds local container images from a
	// docker.build section, which image GC must account for.
	LocalImageBuild bool
}

var table = map[task.Runtime]Caps{
	task.RuntimeDeno:   {Suspend: true, SecretOutput: true},
	RuntimePython:      {Suspend: true, SecretOutput: true},
	task.RuntimeDocker: {LocalImageBuild: true},
	task.RuntimePodman: {LocalImageBuild: true},
}

// unknown applies to runtime names not in the table. Spec validation accepts
// any runtime name, so this is reachable; Suspend stays granted because only
// container runtimes are known to drop a suspend payload.
var unknown = Caps{Suspend: true}

// For returns the capabilities of rt. The legacy aliases "" and "js" are deno.
func For(rt task.Runtime) Caps {
	if rt == "" || rt == "js" {
		rt = task.RuntimeDeno
	}
	if c, ok := table[rt]; ok {
		return c
	}
	return unknown
}
