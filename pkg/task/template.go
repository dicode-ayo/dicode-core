package task

import (
	"maps"
	"os"
	"strings"
)

// Template variables available inside task.yaml as ${VAR}. Resolved at
// spec-load time (in LoadDir). Resolution order:
//
//  1. Built-in variables provided by the loader (TASK_DIR, TASK_SET_DIR,
//     HOME, …) merged with caller-supplied extras.
//  2. Process environment (os.Getenv) — fallback only for fields whose
//     expanded value is not readable from task code. See expandSpec for
//     the per-field policy.
//  3. Unresolved — the literal ${VAR} is left in place so downstream code
//     can log/warn, rather than silently replacing with "" and producing a
//     mysterious empty-string bug.
//
// See docs/task-template-vars.md for the end-user reference.
const (
	// VarTaskDir is the absolute path to the task's own directory
	// (i.e. the directory containing its task.yaml).
	VarTaskDir = "TASK_DIR"

	// VarHome is the daemon-process user's home directory.
	VarHome = "HOME"

	// VarTaskSetDir is the absolute path to the directory containing the
	// root taskset.yaml of the source that loaded this task. Injected by
	// the source loader; absent when a task is loaded outside of a source
	// context (e.g. a raw local folder source or a unit test).
	VarTaskSetDir = "TASK_SET_DIR"

	// VarDataDir is the absolute path to the daemon's data directory
	// (config.DataDir, e.g. ~/.dicode). Injected by the reconciler
	// and the taskset resolver before each task.LoadDir call so tasks can
	// reference shared paths under the data dir from permissions.fs.path
	// and params.default fields.
	VarDataDir = "DATADIR"
)

// expandString replaces ${VAR} references in s using the provided vars map
// and, when envFallback is true, the process environment. Unknown references
// are left literal rather than collapsing to "".
//
// Unlike os.ExpandEnv, this function does NOT replace ${VAR} with "" when
// VAR is unknown. Leaving the literal makes debugging obvious and avoids
// silently producing broken paths like "/foo//bar" or "".
//
// IMPORTANT: envFallback must be false for any field whose expanded value
// is readable from inside the task sandbox. Otherwise a task.yaml from an
// untrusted source could exfiltrate daemon secrets by naming them as a
// template variable and reading the field at runtime.
func expandString(s string, vars map[string]string, envFallback bool) string {
	if s == "" || !strings.Contains(s, "${") {
		return s
	}
	return os.Expand(s, func(name string) string {
		if v, ok := vars[name]; ok {
			return v
		}
		if envFallback {
			if v, ok := os.LookupEnv(name); ok {
				return v
			}
		}
		// Unknown — re-emit the literal. os.Expand strips the ${…} wrapper
		// when calling the mapper, so we have to add it back.
		return "${" + name + "}"
	})
}

// expandSpec applies template expansion to every spec field that may
// reasonably contain a path or identifier. This is a small,
// well-defined set — we do NOT expand every string field, because most
// fields (name, description, system_prompt defaults, ...) should be taken
// literally.
//
// Env-fallback policy (see expandString for the rationale):
//
//   - Fields whose expanded value stays server-side and is NOT readable from
//     task code get envFallback=true: webhook_secret (used by the webhook
//     handler to compute HMAC; task code never sees it), fs.path (consumed
//     by the Deno permission set), env.from / env.secret (both are host-side
//     lookup keys, not values injected into the sandbox).
//
//   - Fields whose expanded value IS readable from task code
//     (permissions.env[].value, params[].default) get envFallback=false.
//     Enabling env fallback on those would be a direct exfiltration
//     primitive: any task.yaml could name a daemon secret as a template
//     variable and read it back at runtime. Builtins + caller extras only.
func expandSpec(spec *Spec, vars map[string]string) {
	// trigger.webhook_secret: historically documented as supporting ${VAR}
	// and this PR closes that long-standing gap. Env fallback is the whole
	// point — users set WEBHOOK_SECRET=… in the daemon env and reference it
	// from task.yaml.
	spec.Trigger.WebhookSecret = expandString(spec.Trigger.WebhookSecret, vars, true)

	// permissions.fs[].path: tasks need to reference shared directories
	// relative to known locations. Env fallback lets `${HOME}/shared` keep
	// working even if a caller forgets to populate HOME as a builtin.
	for i := range spec.Permissions.FS {
		spec.Permissions.FS[i].Path = expandString(spec.Permissions.FS[i].Path, vars, true)
	}

	// permissions.env[].from and .secret: indirection keys for host env
	// rename and secrets-store lookup. Both are identifiers, not values —
	// expanding them never leaks a secret into task-visible state.
	for i := range spec.Permissions.Env {
		spec.Permissions.Env[i].From = expandString(spec.Permissions.Env[i].From, vars, true)
		spec.Permissions.Env[i].Secret = expandString(spec.Permissions.Env[i].Secret, vars, true)
		spec.Permissions.Env[i].Value = expandString(spec.Permissions.Env[i].Value, vars, false)
	}

	// params[].default: lets task.yaml surface loader-provided paths
	// (${TASK_SET_DIR}, ${TASK_DIR}, …) as parameter defaults that task
	// code reads via params.get().
	for i := range spec.Params {
		spec.Params[i].Default = expandString(spec.Params[i].Default, vars, false)
	}

	// docker.*: all fields expanded with envFallback=false — a task.yaml from
	// an untrusted source must not be able to reference arbitrary host paths
	// or daemon env vars through these fields. TASK_DIR and other builtins
	// always resolve because they are in the vars map, not the process env.
	if spec.Docker != nil {
		for i := range spec.Docker.Volumes {
			spec.Docker.Volumes[i] = expandString(spec.Docker.Volumes[i], vars, false)
		}
		for i := range spec.Docker.Command {
			spec.Docker.Command[i] = expandString(spec.Docker.Command[i], vars, false)
		}
		for i := range spec.Docker.Entrypoint {
			spec.Docker.Entrypoint[i] = expandString(spec.Docker.Entrypoint[i], vars, false)
		}
		spec.Docker.WorkingDir = expandString(spec.Docker.WorkingDir, vars, false)
		if spec.Docker.Build != nil {
			spec.Docker.Build.Context = expandString(spec.Docker.Build.Context, vars, false)
			spec.Docker.Build.Dockerfile = expandString(spec.Docker.Build.Dockerfile, vars, false)
		}
	}

	// trigger.chain.overrides: per-edge patch applied to the downstream spec
	// at dispatch time. It must walk the same {Params[].Default, Fs[].Path,
	// Env[].{From,Secret,Value}} surface as its top-level twins — otherwise
	// tokens like ${DATADIR} embedded in an override reach the runtime
	// literally and the dispatch fails (e.g. Deno's --allow-write sandbox
	// sees the unexpanded "${DATADIR}/relay" path and rejects). envFallback
	// policy mirrors the top-level fields exactly: identifier-like fields
	// (Fs[].Path, Env[].From, Env[].Secret) get true, fields readable from
	// task code (Params[].Default, Env[].Value) get false. (PipelineTask
	// stage overrides are expanded separately in expandPipeline.)
	if spec.Trigger.Chain != nil {
		expandOverrides(spec.Trigger.Chain.Overrides, vars)
	}
}

// expandOverrides applies ${VAR} substitution in place to the supplied
// *Overrides — Params[].Default, Fs[].Path, and Env[].{From,Secret,Value}
// fields. Mirrors the per-field envFallback policy from the top-level
// expandSpec.
//
// Only the surface actually consumed by the per-edge dispatch path is touched.
// Other Overrides fields (Net, Runtime, Timeout, …) are either literal
// strings, durations, or already opaque identifiers and don't need expansion.
//
// Precondition: the *Overrides MUST NOT be aliased by any other goroutine
// during the call. Caller is the spec loader (reconciler), which holds
// the only reference at expansion time — same contract as expandSpec.
func expandOverrides(o *Overrides, vars map[string]string) {
	if o == nil {
		return
	}
	for i := range o.Params {
		o.Params[i].Default = expandString(o.Params[i].Default, vars, false)
	}
	for i := range o.Fs {
		o.Fs[i].Path = expandString(o.Fs[i].Path, vars, true)
	}
	for i := range o.Env {
		o.Env[i].From = expandString(o.Env[i].From, vars, true)
		o.Env[i].Secret = expandString(o.Env[i].Secret, vars, true)
		o.Env[i].Value = expandString(o.Env[i].Value, vars, false)
	}
}

// builtinVars returns the template var map for a task loaded from dir, with
// any extra vars merged in (extras take precedence over builtins). Pass nil
// for extras when loading a task outside of a source context.
//
// Keep this in sync with the Var* constants above and docs/task-template-vars.md.
func builtinVars(taskDir string, extras map[string]string) map[string]string {
	vars := map[string]string{
		VarTaskDir: taskDir,
	}
	if home, err := os.UserHomeDir(); err == nil {
		vars[VarHome] = home
	}
	maps.Copy(vars, extras)
	return vars
}
