package task

import (
	"os"
	"strings"
)

// Template variables available inside task.yaml as ${VAR}. These are resolved
// at spec-load time (in LoadDir). The resolution order is:
//
//  1. Built-in variables provided by the loader (TASK_DIR, HOME, ...).
//  2. Process environment (os.Getenv).
//  3. Unresolved — the literal ${VAR} is left in place so downstream code can
//     log / warn / handle it, rather than silently replacing with "" and
//     producing a mysterious empty-string bug.
//
// This matches the syntax used by pkg/config/config.go (expandVars) and the
// ${WEBHOOK_SECRET} convention already documented for trigger.webhook_secret.
//
// Future work: the source loader can widen the built-in set with per-source
// context like REPO_ROOT and SKILLS_DIR. Task-level code does not know about
// sources, so those vars cannot live here.
const (
	VarTaskDir    = "TASK_DIR"    // absolute path to the task's own directory
	VarHome       = "HOME"        // user home directory
	VarSourceRoot = "SOURCE_ROOT" // absolute path to the source root (tasks dir / taskset.yaml dir). Injected by the source loader; empty when a task is loaded outside of a source context.
	VarSkillsDir  = "SKILLS_DIR"  // convention: ${SOURCE_ROOT}/skills. Lets skill-aware tasks reference the shared md directory without hardcoding the "skills" subdir.
)

// expandString replaces ${VAR} references in s using the provided vars map,
// falling back to process env, and leaving unresolved references literal.
//
// Unlike os.ExpandEnv, this function does NOT replace ${VAR} with "" when
// VAR is unknown. Leaving the literal makes debugging obvious and avoids
// silently producing broken paths like "/foo//bar" or "".
func expandString(s string, vars map[string]string) string {
	if s == "" || !strings.Contains(s, "${") {
		return s
	}
	return os.Expand(s, func(name string) string {
		if v, ok := vars[name]; ok {
			return v
		}
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		// Unknown — re-emit the literal. os.Expand strips the ${…} wrapper
		// when calling the mapper, so we have to add it back.
		return "${" + name + "}"
	})
}

// expandSpec applies template expansion to every spec field that may
// reasonably contain a path or identifier. This is intentionally a small,
// well-defined set — we do NOT expand every string field, because most
// fields (name, description, system_prompt defaults, ...) should be taken
// literally.
func expandSpec(spec *Spec, vars map[string]string) {
	// trigger.webhook_secret: historically documented as supporting ${VAR}
	// but the expansion was never implemented. Closing that gap.
	spec.Trigger.WebhookSecret = expandString(spec.Trigger.WebhookSecret, vars)

	// permissions.fs[].path: the primary motivation for this change — tasks
	// need to reference shared directories relative to known locations.
	for i := range spec.Permissions.FS {
		spec.Permissions.FS[i].Path = expandString(spec.Permissions.FS[i].Path, vars)
	}

	// permissions.env[].from and .secret: the indirection keys for host env
	// rename and secrets-store lookup. Both are identifiers that might want
	// to be built from a prefix + task-local value.
	for i := range spec.Permissions.Env {
		spec.Permissions.Env[i].From = expandString(spec.Permissions.Env[i].From, vars)
		spec.Permissions.Env[i].Secret = expandString(spec.Permissions.Env[i].Secret, vars)
		spec.Permissions.Env[i].Value = expandString(spec.Permissions.Env[i].Value, vars)
	}
}

// builtinVars returns the template var map for a task loaded from dir, with
// any extra vars merged in (extras take precedence over builtins). Pass nil
// for extras when loading a task outside of a source context.
//
// Keep this in sync with the Var* constants above and with docs/task-yaml.md
// (once that doc exists).
func builtinVars(taskDir string, extras map[string]string) map[string]string {
	vars := map[string]string{
		VarTaskDir: taskDir,
	}
	if home, err := os.UserHomeDir(); err == nil {
		vars[VarHome] = home
	}
	// Convenience: if the caller supplies SOURCE_ROOT but not SKILLS_DIR,
	// auto-derive it. Lets source loaders stay minimal — they only need to
	// set the primitive SOURCE_ROOT and the convention follows.
	if root, ok := extras[VarSourceRoot]; ok {
		if _, set := extras[VarSkillsDir]; !set {
			vars[VarSkillsDir] = root + "/skills"
		}
	}
	for k, v := range extras {
		vars[k] = v
	}
	return vars
}
