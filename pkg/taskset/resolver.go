package taskset

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dicode/dicode/internal/fsutil"
	"github.com/dicode/dicode/internal/pathguard"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// Ref path template variables expanded before path resolution.
const (
	// VarRepoDir expands to the root of the source (git clone dir or local source root).
	VarRepoDir = "REPO_DIR"
	// VarTaskSetRefDir expands to the directory containing the current taskset.yaml.
	// Named differently from task.VarTaskSetDir to avoid confusion: VarTaskSetDir
	// is expanded inside task.yaml specs; VarTaskSetRefDir is expanded in ref paths.
	VarTaskSetRefDir = "TASKSET_DIR"
)

// repoKey is the deduplication key for a git repository clone.
//
// AllowAuth partitions the cache by trust tier, not just (URL, Branch): an
// allowAuth=false resolution (a ref discovered inside an already-resolved
// sub-tree, #740) must never be served the directory an allowAuth=true
// resolution populated with credentials. Without this, a nested, untrusted
// entry could name the SAME (url, branch) as an operator-configured root ref
// and read the daemon's already-authenticated clone straight out of the
// cache — no token_env of its own required, defeating the auth gate in
// resolveRef/gatedTokenEnv entirely. See TestEnsureClone_UntrustedCannotReuseAuthenticatedCache.
type repoKey struct {
	URL       string
	Branch    string
	AllowAuth bool
}

// ResolveFailure describes one taskset entry that failed to resolve, load, or
// validate during a Resolve call. Unlike a whole-Resolve error (a malformed
// taskset.yaml itself), a ResolveFailure is scoped to a single entry — the
// rest of the tree still resolves normally. Callers use the ID to keep the
// entry discoverable elsewhere (e.g. the webui task list) instead of it
// silently vanishing when its task.yaml fails to parse (#649).
type ResolveFailure struct {
	// ID is the namespaced entry ID (matches ResolvedTask.ID for a
	// successfully resolved sibling), e.g. "infra/backend/deploy".
	ID string
	// Error is the underlying resolution/load/validate failure.
	Error error
}

// Resolver resolves a TaskSet tree into a flat list of ResolvedTasks.
// It deduplicates git clones so that N entries referencing the same (url, branch)
// pair share a single local clone directory.
type Resolver struct {
	dataDir string
	devMode bool
	log     *zap.Logger

	mu               sync.Mutex
	clones           map[repoKey]string // (url, branch) → absolute local dir
	allowedTokenEnvs []string           // operator allowlist for ref.auth.token_env (#753); nil/empty = unrestricted
}

// NewResolver creates a Resolver.
// dataDir is the base directory for cloned repos (e.g. ~/.dicode).
func NewResolver(dataDir string, devMode bool, log *zap.Logger) *Resolver {
	return &Resolver{
		dataDir: dataDir,
		devMode: devMode,
		log:     log,
		clones:  make(map[repoKey]string),
	}
}

// SetDevMode enables or disables dev ref substitution on future Resolve calls.
func (r *Resolver) SetDevMode(enabled bool) {
	r.mu.Lock()
	r.devMode = enabled
	r.mu.Unlock()
}

// DevMode reports whether dev mode is currently active.
func (r *Resolver) DevMode() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.devMode
}

// SetAllowedTokenEnvs installs the operator-declared allowlist of environment
// variable names a git ref's auth.token_env may name (#753's first residual:
// #740/#748 gated WHICH ref may carry auth but left WHICH variable it may
// name unconstrained). A nil or empty allowlist is fully permissive — this
// is optional, additive hardening, not a default restriction — so an
// operator who never sets source_security.allowed_token_envs keeps today's
// behavior unchanged. The allowlist only ever narrows a ref that gatedTokenEnv
// already decided to trust; it has no effect on a ref discovered inside a
// resolved sub-tree, which gatedTokenEnv strips regardless.
func (r *Resolver) SetAllowedTokenEnvs(envs []string) {
	r.mu.Lock()
	r.allowedTokenEnvs = envs
	r.mu.Unlock()
}

// Resolve walks the TaskSet rooted at tsRef with the given namespace prefix.
//
// The override precedence is three levels (lowest to highest):
//  1. task.yaml base spec
//  2. This TaskSet's spec.defaults
//  3. Per-entry overrides (parent entry patch merged with local entry overrides)
//
// configDefaults (from a colocated kind:Config file) is accepted for backwards
// compatibility but is no longer applied to the override stack. A deprecation
// warning is logged when it is non-nil. Use dicode.yaml defaults: instead.
//
// parentOverrides carries per-entry patches from the parent TaskSet (level 3).
// Passing parentOverrides.Defaults is also deprecated and now a no-op; only
// Entries is honoured.
//
// extraVars is the per-resolve template-variable set injected into every
// task.yaml ${VAR} expansion. Pass nil when the caller has no additional
// context — the resolver itself always sets TASK_SET_DIR from the resolved
// root taskset.yaml path, so source loaders don't need to. The map is
// treated as read-only; the resolver never mutates or retains it.
//
// The returned []ResolveFailure lists entries that failed to resolve/load/
// validate — these are omitted from the []*ResolvedTask result (as before)
// but are not silently dropped: callers can surface them so a task with a
// parse error stays discoverable instead of vanishing (#649). A non-nil
// error return is reserved for failures of the ROOT taskset.yaml itself
// (can't even start resolving); per-entry failures always come back via the
// []ResolveFailure slice with a nil error.
func (r *Resolver) Resolve(ctx context.Context, namespace string, tsRef *Ref, configDefaults *Defaults, parentOverrides *Overrides, extraVars map[string]string) ([]*ResolvedTask, []ResolveFailure, error) {
	// tsRef is always operator-configured (dicode.yaml's source entry, or a
	// sibling root ref a caller constructed directly) — never a ref discovered
	// while resolving a tree — so it always honours ref.auth. The mustBeTask
	// signal is irrelevant here: LoadTaskSet below already requires kind:
	// TaskSet unconditionally, so a task.yaml misconfigured as the root ref
	// fails there regardless.
	tsPath, _, err := r.resolveRef(ctx, tsRef, "", nil, "", true)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve ref for namespace %q: %w", namespace, err)
	}

	ts, err := LoadTaskSet(tsPath)
	if err != nil {
		return nil, nil, err
	}

	// Compute the repo-root directory for ref path template expansion.
	// For local sources this is the directory containing the root taskset.yaml;
	// for git sources it would be the clone directory (already resolved above).
	repoDir := filepath.Dir(tsPath)

	// cloneRoot constrains local ref paths when the root taskset was resolved
	// from a git source: relative paths in nested refs must stay within the
	// clone directory. For local sources, cloneRoot is "" so no containment
	// check is applied (the operator chose those paths explicitly).
	cloneRoot := ""
	if tsRef.IsGit() {
		cloneRoot = repoDir
	}

	// At the TOP-level Resolve only, inject TASK_SET_DIR from the resolved
	// root taskset path. Nested resolveNestedRef calls receive this same
	// map unchanged, so the variable always points at the ROOT taskset dir
	// regardless of recursion depth (matches docs/task-template-vars.md).
	//
	// Done here rather than in source loaders so both local and git sources
	// — and any future source type funnelling through the resolver — behave
	// identically. Git sources used to pass nil, which left the literal
	// ${TASK_SET_DIR} in task.yaml paths.
	rootVars := withTaskSetDir(extraVars, tsPath)

	// allowAuth=true: entries declared directly in this call's taskset.yaml are
	// the source's ROOT taskset — the file the operator pointed dicode.yaml (or
	// a sibling root ref) at. That is operator-owned config, so a git entry's
	// ref.auth.token_env is honoured. Anything resolveBody recurses into via a
	// KindTaskSet entry is a resolved sub-tree, not operator-owned config, and
	// resolveNestedRef always resolves it with allowAuth=false (#740).
	return r.resolveBody(ctx, namespace, tsPath, ts, configDefaults, parentOverrides, rootVars, repoDir, cloneRoot, true)
}

// withTaskSetDir returns a copy of base with VarTaskSetDir set to
// filepath.Dir(tsPath) unless the caller already provided one (explicit
// caller override always wins). Returns a fresh map — never mutates base.
//
// tsPath is always non-empty by construction: Resolve calls resolveRef
// first, which errors out before this helper can see an empty path.
func withTaskSetDir(base map[string]string, tsPath string) map[string]string {
	out := make(map[string]string, len(base)+1)
	maps.Copy(out, base)
	if _, set := out[task.VarTaskSetDir]; !set {
		out[task.VarTaskSetDir] = filepath.Dir(tsPath)
	}
	return out
}

func (r *Resolver) resolveBody(
	ctx context.Context,
	namespace, tsPath string,
	ts *TaskSetSpec,
	configDefaults *Defaults,
	parentOverrides *Overrides,
	extraVars map[string]string,
	repoDir string,
	cloneRoot string,
	allowAuth bool,
) ([]*ResolvedTask, []ResolveFailure, error) {
	// Deprecation warnings for removed precedence levels.
	if defaultsNonEmpty(configDefaults) {
		r.log.Warn("taskset: kind:Config spec.defaults is deprecated and no longer applied to the override stack; migrate settings to dicode.yaml defaults:",
			zap.String("taskset", tsPath),
		)
	}
	if parentOverrides != nil && defaultsNonEmpty(parentOverrides.Defaults) {
		r.log.Warn("taskset: overrides.defaults cross-boundary cascade is deprecated and no longer applied; use per-entry overrides.entries[key] to patch nested tasks explicitly",
			zap.String("taskset", tsPath),
		)
	}

	var results []*ResolvedTask
	var failures []ResolveFailure

	for key, entry := range ts.Spec.Entries {
		fullID := joinNamespace(namespace, key)

		// Per-entry patch injected by the parent (via parent.overrides.entries).
		var parentEntryOverride *Overrides
		if parentOverrides != nil && parentOverrides.Entries != nil {
			parentEntryOverride = parentOverrides.Entries[key]
		}

		// Determine enabled state.
		// Precedence (highest wins): parentEntryOverride.Enabled >
		// entry.Overrides.Enabled > default true.
		// Note: the top-level `entry.Enabled` shortcut is always lifted into
		// entry.Overrides.Enabled by LiftEntryEnabled during load/validate, so
		// there is no need to check entry.Enabled here.
		enabled := true
		if entry.Overrides != nil && entry.Overrides.Enabled != nil {
			enabled = *entry.Overrides.Enabled
		}
		if parentEntryOverride != nil && parentEntryOverride.Enabled != nil {
			enabled = *parentEntryOverride.Enabled
		}

		if entry.Inline != nil {
			// Expand ${VAR} references in the inline base spec itself before
			// applying overrides — mirrors what LoadDirWithVars does for a
			// ref-loaded task.yaml. Without this, an inline entry's own
			// fields (e.g. permissions.fs[].path) never see substitution,
			// even though the override layers stacked on top of it do (#726).
			task.ExpandSpec(entry.Inline, filepath.Dir(tsPath), r.withDataDir(extraVars))
			layers := expandOverrideLayers(
				buildOverrideLayers(ts.Spec.Defaults, parentEntryOverride, entry.Overrides),
				filepath.Dir(tsPath), r.withDataDir(extraVars))
			resolved := applyOverrides(entry.Inline, layers...)
			resolved.ID = fullID
			resolved.TaskDir = filepath.Dir(tsPath)
			resolved.Enabled = enabled
			// Re-validate after the override merge so a bad override
			// surfaces here (with the operator-relevant taskset path)
			// rather than later at engine.Register (survey §5.1).
			if err := resolved.Validate(); err != nil {
				r.log.Warn("taskset: merged spec failed validate after override apply; skipping",
					zap.String("entry", fullID), zap.Error(err))
				failures = append(failures, ResolveFailure{ID: fullID, Error: err})
				continue
			}
			results = append(results, &ResolvedTask{
				Kinded:  resolved,
				ID:      fullID,
				TaskDir: resolved.TaskDir,
			})
			continue
		}

		// Ref-based entry.
		ref := entry.Ref
		if r.devMode && ref.DevRef != nil {
			ref = ref.DevRef
		}

		// Build ref path template variables for ${REPO_DIR} / ${TASKSET_DIR}.
		refVars := map[string]string{
			VarRepoDir:       repoDir,
			VarTaskSetRefDir: filepath.Dir(tsPath),
		}

		localPath, mustBeTask, err := r.resolveRef(ctx, ref, tsPath, refVars, cloneRoot, allowAuth)
		if err != nil {
			r.log.Warn("taskset: failed to resolve ref",
				zap.String("entry", fullID), zap.Error(err))
			failures = append(failures, ResolveFailure{ID: fullID, Error: err})
			continue
		}

		kind, err := DetectKind(localPath)
		if err != nil {
			r.log.Warn("taskset: failed to detect kind",
				zap.String("path", localPath), zap.Error(err))
			failures = append(failures, ResolveFailure{ID: fullID, Error: err})
			continue
		}

		// A ref that resolves to a task.yaml/task.yml file (as every entry
		// taskset.AddTaskEntry writes does — see taskEntryRefPath — and as any
		// directory-valued ref whose resolveYAMLPath probe lands on one, #753)
		// can legitimately be kind: Task or kind: PipelineTask, but never kind:
		// TaskSet. Letting it resolve as a TaskSet would route an
		// attacker-editable task file into the KindTaskSet branch below —
		// recursing into it with full ref-auth/entry-merge machinery instead of
		// loading it as the leaf task its path declares it to be (#740). Treat
		// that mismatch as a load failure, not a routing choice.
		if mustBeTask && kind == KindTaskSet {
			err := fmt.Errorf("ref path %q resolves to a task file (%s) but declares kind TaskSet — a task ref must not resolve as a TaskSet", ref.Path, localPath)
			r.log.Warn("taskset: task ref resolved to kind TaskSet; refusing",
				zap.String("entry", fullID), zap.Error(err))
			failures = append(failures, ResolveFailure{ID: fullID, Error: err})
			continue
		}

		switch kind {
		case KindTask:
			taskDir := filepath.Dir(localPath)
			extras := r.withDataDir(extraVars)
			spec, err := task.LoadDirWithVars(taskDir, extras)
			if err != nil {
				r.log.Warn("taskset: failed to load task",
					zap.String("entry", fullID), zap.Error(err))
				failures = append(failures, ResolveFailure{ID: fullID, Error: err})
				continue
			}
			for _, w := range spec.Warnings {
				r.log.Warn("taskset: task config warning",
					zap.String("entry", fullID),
					zap.String("warning", w),
				)
			}
			layers := expandOverrideLayers(
				buildOverrideLayers(ts.Spec.Defaults, parentEntryOverride, entry.Overrides),
				taskDir, extras)
			resolved := applyOverrides(spec, layers...)
			resolved.ID = fullID
			resolved.Enabled = enabled
			// Re-validate after the override merge so a bad override
			// surfaces here (with the operator-relevant taskset path)
			// rather than later at engine.Register (survey §5.1).
			if err := resolved.Validate(); err != nil {
				r.log.Warn("taskset: merged spec failed validate after override apply; skipping",
					zap.String("entry", fullID), zap.Error(err))
				failures = append(failures, ResolveFailure{ID: fullID, Error: err})
				continue
			}
			results = append(results, &ResolvedTask{
				Kinded:  resolved,
				ID:      fullID,
				TaskDir: taskDir,
			})

		case KindPipelineTask:
			taskDir := filepath.Dir(localPath)
			extras := r.withDataDir(extraVars)
			p, err := task.LoadPipelineDir(taskDir, extras)
			if err != nil {
				r.log.Warn("taskset: failed to load pipeline",
					zap.String("entry", fullID), zap.Error(err))
				failures = append(failures, ResolveFailure{ID: fullID, Error: err})
				continue
			}
			p.ID = fullID
			p.Enabled = enabled
			// Pipelines do not support taskset override layers in v1 — stage
			// overrides live in the pipeline spec. Re-validate after stamping the
			// namespaced ID so a stage self-reference surfaces with the taskset path.
			if err := p.Validate(); err != nil {
				r.log.Warn("taskset: pipeline failed validate; skipping",
					zap.String("entry", fullID), zap.Error(err))
				failures = append(failures, ResolveFailure{ID: fullID, Error: err})
				continue
			}
			results = append(results, &ResolvedTask{
				Kinded:  p,
				ID:      fullID,
				TaskDir: taskDir,
			})

		case KindTaskSet:
			// A disabled TaskSet entry propagates disabled to all its children.
			// Resolve the sub-tree so tasks remain visible in the API; the
			// engine skips scheduling for any task where Enabled == false.
			nestedOverrides := entry.Overrides
			if parentEntryOverride != nil {
				nestedOverrides = mergeOverrides(parentEntryOverride, nestedOverrides)
			}
			// When a nested taskset was resolved from a git ref, its local refs
			// must be constrained to THAT clone's root, not the parent's. Update
			// cloneRoot to the directory containing the resolved nested taskset.
			nestedCloneRoot := cloneRoot
			if ref.IsGit() {
				nestedCloneRoot = filepath.Dir(localPath)
			}
			// resolveNestedRef always resolves the sub-tree with allowAuth=false:
			// this is a resolved TaskSet, not operator-owned config, regardless of
			// whether the CURRENT level (this KindTaskSet entry's own ref, fetched
			// above via r.resolveRef with the caller's allowAuth) was trusted.
			nested, nestedFailures, err := r.resolveNestedRef(ctx, fullID, localPath, nestedOverrides, extraVars, repoDir, nestedCloneRoot)
			if err != nil {
				r.log.Warn("taskset: failed to resolve nested taskset",
					zap.String("entry", fullID), zap.Error(err))
				failures = append(failures, ResolveFailure{ID: fullID, Error: err})
				continue
			}
			// If this entry is disabled, propagate disabled to all its children
			// so the whole sub-tree stays visible in the API but is not scheduled.
			if !enabled {
				for _, rt := range nested {
					rt.Kinded.SetEnabled(false)
				}
			}
			results = append(results, nested...)
			failures = append(failures, nestedFailures...)

		default:
			r.log.Warn("taskset: unknown kind, skipping",
				zap.String("entry", fullID), zap.String("kind", string(kind)))
			failures = append(failures, ResolveFailure{ID: fullID, Error: fmt.Errorf("unknown kind %q", kind)})
		}
	}

	return results, failures, nil
}

func (r *Resolver) resolveNestedRef(ctx context.Context, namespace, tsPath string, overrides *Overrides, extraVars map[string]string, repoDir string, cloneRoot string) ([]*ResolvedTask, []ResolveFailure, error) {
	ts, err := LoadTaskSet(tsPath)
	if err != nil {
		return nil, nil, err
	}
	// Pass nil for configDefaults: deprecation warnings are emitted once at the
	// public Resolve entry point; nested sets do not re-emit them.
	// allowAuth=false: a nested taskset is a resolved sub-tree, never
	// operator-owned config, so its entries' git refs never carry auth (#740).
	return r.resolveBody(ctx, namespace, tsPath, ts, nil, overrides, extraVars, repoDir, cloneRoot, false)
}

// resolveRef returns the absolute local path to the yaml file pointed to by ref,
// and reports whether that path resolves to a task file (a "task.yaml" or
// "task.yml" basename, so the resolved file must not be kind: TaskSet — see
// the mustBeTask check in resolveBody, #740).
//
// mustBeTask is true if EITHER the ref's own configured path or the path
// resolveYAMLPath actually landed on names a task file (#753's second
// residual, hardened further after a security review of the first draft).
// Checking the resolved path alone catches a ref whose path names a
// directory (e.g. "./x" or "./x/") whose resolveYAMLPath probe lands on a
// task.yaml inside it — before this fix that shape always got
// mustBeTask=false, silently exempting it from the #740 hardening below.
// Checking the configured path alone would miss that same case, but is
// needed for the reverse: a writable source shadowing a scaffolded
// ".../task.yaml" ref path with a DIRECTORY of that name holding a nested
// taskset.yaml, which resolveYAMLPath would silently probe into and return
// (basename "taskset.yaml"), clearing mustBeTask despite the ref being
// declared exactly the trusted shape taskEntryRefPath writes. Checking both
// closes each without reopening the other.
// For git refs this may trigger a clone or pull.
// parentTSPath is the absolute path of the parent taskset.yaml — used to resolve
// relative paths in local refs against the parent's directory.
// refVars holds template variables (REPO_DIR, TASKSET_DIR) to expand in the
// ref's Path field before resolution. Pass nil when no expansion is needed
// (e.g. the root Resolve call before repoDir is known).
// cloneRoot, when non-empty, constrains local (non-git) refs: the resolved path
// must remain within cloneRoot. Pass "" to skip the containment check.
// allowAuth gates whether a git ref's ref.auth.token_env is honoured: only
// true for refs declared in operator-owned config (dicode.yaml's source ref,
// or an entry in a source's root taskset.yaml) — never for a ref discovered
// while resolving an already-resolved sub-tree, where a writable source could
// otherwise name any daemon env var as a credential to hand to a host of its
// choosing (#740). When false, a git ref's token_env is ignored (logged, not
// silently dropped) and the clone proceeds unauthenticated.
func (r *Resolver) resolveRef(ctx context.Context, ref *Ref, parentTSPath string, refVars map[string]string, cloneRoot string, allowAuth bool) (string, bool, error) {
	path := expandRefPath(ref.Path, refVars)

	var resolved string
	if !ref.IsGit() {
		if !filepath.IsAbs(path) {
			resolved = filepath.Clean(filepath.Join(filepath.Dir(parentTSPath), path))
		} else {
			resolved = filepath.Clean(path)
		}
		// When inside a git-cloned source, local refs must stay within the clone root.
		if cloneRoot != "" {
			if err := containedPath(cloneRoot, resolved); err != nil {
				return "", false, fmt.Errorf("ref path %q: %w", ref.Path, err)
			}
		}
	} else {
		branch := ref.effectiveBranch()
		// ensureClone re-derives the effective token itself from (allowAuth,
		// ref.Auth.TokenEnv) — it does not trust this call to have already
		// gated it — so passing the raw TokenEnv here is safe regardless of
		// what any other caller does. See ensureClone's doc comment.
		localDir, err := r.ensureClone(ctx, ref.URL, branch, ref.effectivePoll(), ref.Auth.TokenEnv, allowAuth)
		if err != nil {
			return "", false, err
		}
		resolved = filepath.Clean(filepath.Join(localDir, path))
		if err := containedPath(localDir, resolved); err != nil {
			return "", false, fmt.Errorf("ref path %q: %w", ref.Path, err)
		}
	}
	resolvedPath := resolveYAMLPath(resolved)
	// See this function's doc comment for why mustBeTask ORs the configured
	// and resolved basenames rather than checking either alone.
	mustBeTask := isTaskFileName(filepath.Base(path)) || isTaskFileName(filepath.Base(resolvedPath))
	return resolvedPath, mustBeTask, nil
}

// isTaskFileName reports whether name is a recognized task-file basename.
// task.LoadDirWithVars and buildin/write-task-file's assertTaskDocument both
// accept task.yaml and task.yml interchangeably; the mustBeTask hardening
// (#740) must recognize the same pair or a ref resolving to a bare task.yml
// would skip the "must not be kind: TaskSet" check entirely (#753).
func isTaskFileName(name string) bool {
	return name == "task.yaml" || name == "task.yml"
}

// gatedTokenEnv decides whether a git ref's ref.auth.token_env is honoured.
// tokenEnv is returned unchanged when allowAuth is true or the ref carries no
// auth to begin with; otherwise it is stripped to "" and blocked reports true
// so the caller can log what it dropped. Split out from resolveRef as a pure
// function so the trust decision itself — not the clone it gates — is unit
// testable (#740).
func gatedTokenEnv(allowAuth bool, tokenEnv string) (effective string, blocked bool) {
	if allowAuth || tokenEnv == "" {
		return tokenEnv, false
	}
	return "", true
}

// tokenEnvAllowed reports whether envVar may be named as a git ref's
// auth.token_env under the operator's allowlist (#753's first residual: an
// empty/nil allowlist is unrestricted, matching behavior before this
// allowlist existed. A non-empty allowlist is exhaustive — envVar must
// appear in it exactly). Pure function, split out from ensureClone for the
// same reason gatedTokenEnv is: the trust decision should be unit testable
// independent of the clone it gates.
func tokenEnvAllowed(envVar string, allowlist []string) bool {
	if len(allowlist) == 0 {
		return true
	}
	for _, v := range allowlist {
		if v == envVar {
			return true
		}
	}
	return false
}

// applyTokenEnvAllowlist returns tokenEnv unchanged when it is empty or
// allowed by r.allowedTokenEnvs; otherwise it logs the drop and returns "".
// Shared by every call site that hands a token_env to cloneOrPull — Pull
// (a source's root ref, always allowAuth=true) and ensureClone (a nested
// ref, post-gatedTokenEnv) — so the allowlist covers every git-authenticated
// clone path consistently. A caller that already knows tokenEnv is "" may
// skip calling this; it is a no-op in that case regardless.
func (r *Resolver) applyTokenEnvAllowlist(url, tokenEnv string) string {
	if tokenEnv == "" {
		return ""
	}
	r.mu.Lock()
	allowlist := r.allowedTokenEnvs
	r.mu.Unlock()
	if !tokenEnvAllowed(tokenEnv, allowlist) {
		r.log.Warn("taskset: ignoring ref.auth.token_env — not in the operator's source_security.allowed_token_envs allowlist (#753)",
			zap.String("url", url), zap.String("token_env", tokenEnv))
		return ""
	}
	return tokenEnv
}

// containedPath reports whether target stays within root, rejecting both
// lexical escapes (`..`, absolute overrides) and symlink escapes. The lexical
// check alone is insufficient because go-git materializes repo-committed
// symlinks as real on-disk links: a directory symlink inside the clone (e.g.
// `sub -> /etc`) keeps target lexically under root while the downstream
// os.Open/os.Stat would follow it outside. Symlinks are canonicalized away
// before the containment check, mirroring the symlink policy in ScriptPath and
// pkg/task/hash.go.
func containedPath(root, target string) error {
	within, err := pathguard.WithinResolved(root, target)
	if err != nil {
		return err
	}
	if !within {
		return fmt.Errorf("escapes repo root")
	}
	return nil
}

// expandRefPath replaces ${REPO_DIR} and ${TASKSET_DIR} placeholders in a ref
// path with the corresponding values from vars. Unknown or absent variables are
// left as-is. Returns the path unchanged when vars is nil or the path contains
// no placeholders.
func expandRefPath(path string, vars map[string]string) string {
	if len(vars) == 0 || !strings.Contains(path, "${") {
		return path
	}
	for k, v := range vars {
		path = strings.ReplaceAll(path, "${"+k+"}", v)
	}
	return path
}

// resolveYAMLPath returns path unchanged if it is already a file.
// If path is a directory it probes, in order, for taskset.yaml, task.yaml,
// then task.yml inside it — the same pair isTaskFileName recognizes, so a
// directory-valued ref containing only a task.yml (CodeRabbit review of
// #753) resolves onto it instead of silently staying an unresolved
// directory that DetectKind then fails to load.
func resolveYAMLPath(path string) string {
	fi, err := os.Stat(path)
	if err != nil || !fi.IsDir() {
		return path
	}
	for _, candidate := range []string{"taskset.yaml", "task.yaml", "task.yml"} {
		p := filepath.Join(path, candidate)
		if fsutil.Exists(p) {
			return p
		}
	}
	return path
}

// repoCloneDir returns the deterministic on-disk directory for (url, branch)
// under the given trust tier. allowAuth=true reuses the same hash dicode has
// always used (no on-disk migration for operator-configured sources).
//
// allowAuth=false hashes url and branch INDEPENDENTLY first, then combines
// the two digests under a fixed domain-separation prefix, rather than
// concatenating url+branch with an "@untrusted" suffix the way an earlier
// version of this function did. Plain suffixing was not injective: an
// attacker fully controls both fields of their own untrusted ref, so they
// could choose an (url, branch) pair whose "url@branch@untrusted" string
// reproduced an operator's real trusted "url@branch" seed byte-for-byte
// whenever the operator's own branch name happened to end in "@untrusted"
// (or, via a chosen url, in fewer contrived cases still) — landing both
// tiers' clones in the SAME directory despite the different repoKey. Hashing
// url and branch separately before combining removes that structural
// overlap entirely: reproducing a trusted seed from the untrusted branch
// would require a SHA-256 preimage, not a string-concatenation trick (#740
// review).
//
// Note: the directory name below keeps only the first 8 bytes of the final
// digest (h[:8], unchanged from the pre-#740 scheme), so the residual
// cross-tier bound is actually a 64-bit truncated-digest collision rather
// than a full SHA-256 preimage — infeasible for an attacker targeting one
// specific operator directory, but worth stating precisely rather than
// overclaiming full preimage resistance.
func repoCloneDir(dataDir, url, branch string, allowAuth bool) string {
	var h [32]byte
	if allowAuth {
		h = sha256.Sum256([]byte(url + "@" + branch))
	} else {
		hu := sha256.Sum256([]byte(url))
		hb := sha256.Sum256([]byte(branch))
		h = sha256.Sum256([]byte(fmt.Sprintf("dicode-untrusted-clone-v1:%x:%x", hu, hb)))
	}
	return filepath.Join(dataDir, "repos", fmt.Sprintf("ts-%x", h[:8]))
}

// Pull clones or fetches the latest commits for the given git ref and returns
// the local directory. It also updates the clone cache so subsequent Resolve
// calls can find the directory without re-cloning.
// For local refs it is a no-op and returns filepath.Dir(ref.Path).
//
// Pull is only ever called by callers holding an operator-configured ref
// (the source's own root ref — see pkg/taskset/source.go), so it always
// populates the allowAuth=true (trusted) cache partition, matching what a
// root-level resolveRef call for the same (url, branch) would use.
func (r *Resolver) Pull(ctx context.Context, ref *Ref) (string, error) {
	if !ref.IsGit() {
		return filepath.Dir(ref.Path), nil
	}
	branch := ref.effectiveBranch()
	dir := repoCloneDir(r.dataDir, ref.URL, branch, true)
	// Pull always fetches an operator-owned root ref (allowAuth=true by
	// construction — see this method's doc comment), so token_env only ever
	// needs the allowlist check, never gatedTokenEnv's allowAuth gate. This
	// is the primary case source_security.allowed_token_envs documents
	// itself as covering (security review of #753's first draft: this path
	// was missed entirely, so a root ref's token_env went unchecked).
	effectiveTokenEnv := r.applyTokenEnvAllowlist(ref.URL, ref.Auth.TokenEnv)
	if err := cloneOrPull(ctx, dir, ref.URL, branch, effectiveTokenEnv); err != nil {
		return "", fmt.Errorf("pull %s@%s: %w", ref.URL, branch, err)
	}
	key := repoKey{URL: ref.URL, Branch: branch, AllowAuth: true}
	r.mu.Lock()
	r.clones[key] = dir
	r.mu.Unlock()
	return dir, nil
}

// ensureClone returns the local dir for (url, branch), cloning if necessary.
// Within a single resolution pass it deduplicates: once a repo is cloned the
// cached path is returned without a second network round-trip.
// Use Pull to force a fetch from the remote.
//
// allowAuth also selects which trust-tier cache partition (and physical
// directory, via repoCloneDir) this call reads and writes — see repoKey's
// doc comment. ensureClone re-derives the effective token itself via
// gatedTokenEnv rather than trusting the caller to have already gated
// tokenEnv against the same allowAuth: passing the ref's raw, ungated
// auth.token_env alongside allowAuth=false is always safe, by construction,
// regardless of what any other call site does or how it evolves (#740).
//
// A token that survives gatedTokenEnv is then checked against
// r.allowedTokenEnvs (#753): even a ref trusted enough to carry auth can
// only name a variable the operator has explicitly allowlisted via
// source_security.allowed_token_envs. An unset allowlist is unrestricted.
func (r *Resolver) ensureClone(ctx context.Context, url, branch string, _ time.Duration, tokenEnv string, allowAuth bool) (string, error) {
	key := repoKey{URL: url, Branch: branch, AllowAuth: allowAuth}

	r.mu.Lock()
	if dir, ok := r.clones[key]; ok {
		r.mu.Unlock()
		return dir, nil
	}
	r.mu.Unlock()

	effectiveTokenEnv, blocked := gatedTokenEnv(allowAuth, tokenEnv)
	if blocked {
		r.log.Warn("taskset: ignoring ref.auth.token_env on a ref discovered outside operator-owned config — credentials are only honoured on dicode.yaml sources and a source's root taskset (#740)",
			zap.String("url", url))
	}
	effectiveTokenEnv = r.applyTokenEnvAllowlist(url, effectiveTokenEnv)

	dir := repoCloneDir(r.dataDir, url, branch, allowAuth)

	if err := cloneOrPull(ctx, dir, url, branch, effectiveTokenEnv); err != nil {
		return "", fmt.Errorf("clone %s@%s: %w", url, branch, err)
	}

	r.mu.Lock()
	r.clones[key] = dir
	r.mu.Unlock()

	return dir, nil
}

// ClonedRepos returns a snapshot of all (url, branch) → localDir mappings.
// Used by tests and diagnostics. A (url, branch) pair resolved under both
// trust tiers appears twice, prefixed "untrusted:" for the allowAuth=false
// entry, since the two never share a directory. The marker is prefixed, not
// suffixed, so it can never be produced by a url/branch value itself — a
// suffix (e.g. "@untrusted") is a url/branch-shaped string an operator's own
// branch name could coincidentally end in, which would silently collide two
// distinct entries into one map key here (a diagnostics-only misreporting,
// since ClonedRepos has no production caller — not the trust-boundary break
// repoCloneDir's own hashing was fixed against for the same reason).
func (r *Resolver) ClonedRepos() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.clones))
	for k, v := range r.clones {
		name := k.URL + "@" + k.Branch
		if !k.AllowAuth {
			name = "untrusted:" + name
		}
		out[name] = v
	}
	return out
}

// buildOverrideLayers assembles the three-level precedence stack (lowest first):
//  1. (task.yaml base — not in this function, it is the base passed to applyOverrides)
//  2. setDefaults — from this TaskSet's spec.defaults
//  3. parentEntryPatch — parent's overrides.entries[key]  (merged with)
//     entryOverrides  — this entry's own overrides block  ← highest
//
// Removed from the old six-level stack (both now emit deprecation warnings):
//   - configDefaults (was level 2): migrate to dicode.yaml defaults:
//   - parentOverrides.Defaults (was level 4): use per-entry overrides.entries[key]
func buildOverrideLayers(setDefaults *Defaults, parentEntryOverride, entryOverrides *Overrides) []*Overrides {
	layers := make([]*Overrides, 0, 3)
	layers = append(layers, defaultsToOverrides(setDefaults))
	layers = append(layers, parentEntryOverride)
	layers = append(layers, entryOverrides) // entry overrides win (leaf wins)
	return layers
}

// withDataDir returns extras with DATADIR populated from the resolver unless
// the caller already supplied one (which lets tests override it). Clones
// before mutating: extraVars is shared across loop iterations.
func (r *Resolver) withDataDir(extras map[string]string) map[string]string {
	if _, ok := extras[task.VarDataDir]; ok || r.dataDir == "" {
		return extras
	}
	cloned := make(map[string]string, len(extras)+1)
	maps.Copy(cloned, extras)
	cloned[task.VarDataDir] = r.dataDir
	return cloned
}

// expandOverrideLayers substitutes ${VAR} in every layer, returning copies so
// the parsed taskset config keeps the operator's original ${DATADIR}/… text.
// Without this an override's permissions.fs path reaches the sandbox as a
// literal "${DATADIR}/x", which matches nothing and silently denies access.
func expandOverrideLayers(layers []*Overrides, taskDir string, extras map[string]string) []*Overrides {
	for i, l := range layers {
		layers[i] = task.ExpandOverrideLayer(l, taskDir, extras)
	}
	return layers
}

// defaultsNonEmpty reports whether d has at least one field set.
func defaultsNonEmpty(d *Defaults) bool {
	if d == nil {
		return false
	}
	return d.Timeout != 0 || d.Retry != nil || len(d.Env) > 0 || d.Trigger != nil
}

// joinNamespace joins namespace segments with '/'.
func joinNamespace(ns, key string) string {
	if ns == "" {
		return key
	}
	return ns + "/" + key
}

// mergeOverrides merges b on top of a (b wins on conflict).
// Used to combine a parent entry patch with an entry's own overrides.
//
// Reserved chain-param keys (task.ReservedChainParamKey) are stripped from
// the merged Params list — they're rejected at config-load by
// validatePerEdgeOverrides / OnFailureChainSpec.Validate, so a well-formed
// caller never reaches this branch; the strip is defensive (survey §5.2).
func mergeOverrides(a, b *Overrides) *Overrides {
	if a == nil {
		return stripReservedParamKeys(b)
	}
	if b == nil {
		return stripReservedParamKeys(a)
	}
	out := *b // copy b; fill gaps from a

	if out.Trigger == nil {
		out.Trigger = a.Trigger
	}
	if out.Timeout == 0 {
		out.Timeout = a.Timeout
	}
	if out.Runtime == "" {
		out.Runtime = a.Runtime
	}
	if out.Enabled == nil {
		out.Enabled = a.Enabled
	}
	if out.Retry == nil {
		out.Retry = a.Retry
	}
	if out.Defaults == nil {
		out.Defaults = a.Defaults
	}
	// Env: merge by name (a first, b wins)
	if len(a.Env) > 0 || len(out.Env) > 0 {
		out.Env = mergeEnvEntries(a.Env, out.Env)
	}
	// Params: merge by name (b wins)
	if len(a.Params) > 0 {
		merged := make([]ParamOverride, len(a.Params))
		copy(merged, a.Params)
		mergeParamOverrides(&merged, b.Params)
		out.Params = merged
	}
	// Entries map: merge keys (b wins on conflict)
	if len(a.Entries) > 0 {
		entries := make(map[string]*Overrides, len(a.Entries)+len(out.Entries))
		for k, v := range a.Entries {
			entries[k] = v
		}
		for k, v := range out.Entries {
			entries[k] = v
		}
		out.Entries = entries
	}
	// Drop reserved chain-param keys (defensive — config-load validation
	// rejects them upstream).
	out.Params = filterReservedParamKeys(out.Params)
	return &out
}

// stripReservedParamKeys returns o with any reserved-key Params entries
// removed. Returns o unchanged (same pointer) when no entries are
// stripped, to keep the nil-input fast paths in mergeOverrides cheap.
func stripReservedParamKeys(o *Overrides) *Overrides {
	if o == nil || len(o.Params) == 0 {
		return o
	}
	filtered := filterReservedParamKeys(o.Params)
	if len(filtered) == len(o.Params) {
		return o
	}
	out := *o
	out.Params = filtered
	return &out
}

// filterReservedParamKeys returns ps with any reserved-key entries removed.
// Allocates a new slice only when at least one entry is dropped.
func filterReservedParamKeys(ps []ParamOverride) []ParamOverride {
	for _, p := range ps {
		if task.IsReservedChainParamKey(p.Name) {
			out := make([]ParamOverride, 0, len(ps))
			for _, p := range ps {
				if !task.IsReservedChainParamKey(p.Name) {
					out = append(out, p)
				}
			}
			return out
		}
	}
	return ps
}

// mergeParamOverrides merges src into dst by name (src wins on conflict).
func mergeParamOverrides(dst *[]ParamOverride, src []ParamOverride) {
	for _, s := range src {
		found := false
		for i := range *dst {
			if (*dst)[i].Name == s.Name {
				(*dst)[i] = s
				found = true
				break
			}
		}
		if !found {
			*dst = append(*dst, s)
		}
	}
}
