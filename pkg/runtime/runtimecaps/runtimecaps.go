// Package runtimecaps declares what each task runtime supports, keyed by its
// task.yaml runtime name.
//
// Must import only pkg/task: pkg/ipc depends on it.
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

// Runtime names not in the table support nothing.
var unknown = Caps{}

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
