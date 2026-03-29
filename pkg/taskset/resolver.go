package taskset

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// repoKey is the deduplication key for a git repository clone.
type repoKey struct {
	URL    string
	Branch string
}

// Resolver resolves a TaskSet tree into a flat list of ResolvedTasks.
// It deduplicates git clones so that N entries referencing the same (url, branch)
// pair share a single local clone directory.
type Resolver struct {
	dataDir string
	devMode bool
	log     *zap.Logger

	mu     sync.Mutex
	clones map[repoKey]string // (url, branch) → absolute local dir
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

// Resolve walks the TaskSet rooted at tsRef with the given namespace prefix.
// configDefaults comes from the source's kind:Config file (precedence level 2).
// parentOverrides are injected by the parent TaskSet entry (levels 4–5).
func (r *Resolver) Resolve(ctx context.Context, namespace string, tsRef *Ref, configDefaults *Defaults, parentOverrides *Overrides) ([]*ResolvedTask, error) {
	tsPath, err := r.resolveRef(ctx, tsRef)
	if err != nil {
		return nil, fmt.Errorf("resolve ref for namespace %q: %w", namespace, err)
	}

	ts, err := LoadTaskSet(tsPath)
	if err != nil {
		return nil, err
	}

	return r.resolveBody(ctx, namespace, tsPath, ts, configDefaults, parentOverrides)
}

func (r *Resolver) resolveBody(
	ctx context.Context,
	namespace, tsPath string,
	ts *TaskSetSpec,
	configDefaults *Defaults,
	parentOverrides *Overrides,
) ([]*ResolvedTask, error) {
	var results []*ResolvedTask

	for key, entry := range ts.Spec.Entries {
		fullID := joinNamespace(namespace, key)

		// Per-entry patch injected by the parent (via parent.overrides.entries).
		var parentEntryOverride *Overrides
		if parentOverrides != nil && parentOverrides.Entries != nil {
			parentEntryOverride = parentOverrides.Entries[key]
		}

		// Determine enabled state; entry override wins over parent.
		enabled := true
		if entry.Overrides != nil && entry.Overrides.Enabled != nil {
			enabled = *entry.Overrides.Enabled
		}
		if parentEntryOverride != nil && parentEntryOverride.Enabled != nil {
			enabled = *parentEntryOverride.Enabled
		}

		if entry.Inline != nil {
			if !enabled {
				continue
			}
			layers := buildOverrideLayers(configDefaults, ts.Spec.Defaults, parentOverrides, parentEntryOverride, entry.Overrides)
			resolved := applyOverrides(entry.Inline, layers...)
			resolved.ID = fullID
			resolved.TaskDir = filepath.Dir(tsPath)
			results = append(results, &ResolvedTask{
				Spec:    resolved,
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

		localPath, err := r.resolveRef(ctx, ref)
		if err != nil {
			r.log.Warn("taskset: failed to resolve ref",
				zap.String("entry", fullID), zap.Error(err))
			continue
		}

		kind, err := DetectKind(localPath)
		if err != nil {
			r.log.Warn("taskset: failed to detect kind",
				zap.String("path", localPath), zap.Error(err))
			continue
		}

		switch kind {
		case KindTask:
			if !enabled {
				continue
			}
			taskDir := filepath.Dir(localPath)
			spec, err := task.LoadDir(taskDir)
			if err != nil {
				r.log.Warn("taskset: failed to load task",
					zap.String("entry", fullID), zap.Error(err))
				continue
			}
			layers := buildOverrideLayers(configDefaults, ts.Spec.Defaults, parentOverrides, parentEntryOverride, entry.Overrides)
			resolved := applyOverrides(spec, layers...)
			resolved.ID = fullID
			results = append(results, &ResolvedTask{
				Spec:    resolved,
				ID:      fullID,
				TaskDir: taskDir,
			})

		case KindTaskSet:
			// Build the overrides context for the nested set.
			// entry.Overrides carries both defaults (level 4) and per-entry patches (level 5).
			nestedOverrides := entry.Overrides
			if parentEntryOverride != nil {
				nestedOverrides = mergeOverrides(parentEntryOverride, nestedOverrides)
			}
			nested, err := r.resolveNestedRef(ctx, fullID, localPath, configDefaults, nestedOverrides)
			if err != nil {
				r.log.Warn("taskset: failed to resolve nested taskset",
					zap.String("entry", fullID), zap.Error(err))
				continue
			}
			results = append(results, nested...)

		default:
			r.log.Warn("taskset: unknown kind, skipping",
				zap.String("entry", fullID), zap.String("kind", string(kind)))
		}
	}

	return results, nil
}

func (r *Resolver) resolveNestedRef(ctx context.Context, namespace, tsPath string, configDefaults *Defaults, overrides *Overrides) ([]*ResolvedTask, error) {
	ts, err := LoadTaskSet(tsPath)
	if err != nil {
		return nil, err
	}
	return r.resolveBody(ctx, namespace, tsPath, ts, configDefaults, overrides)
}

// resolveRef returns the absolute local path to the yaml file pointed to by ref.
// For git refs this may trigger a clone or pull.
func (r *Resolver) resolveRef(ctx context.Context, ref *Ref) (string, error) {
	if !ref.IsGit() {
		return ref.Path, nil
	}

	branch := ref.effectiveBranch()
	localDir, err := r.ensureClone(ctx, ref.URL, branch, ref.effectivePoll(), ref.Auth.TokenEnv)
	if err != nil {
		return "", err
	}
	return filepath.Join(localDir, ref.Path), nil
}

// ensureClone returns the local dir for (url, branch), cloning if necessary.
// Concurrent calls for the same key are serialised via the mutex.
func (r *Resolver) ensureClone(ctx context.Context, url, branch string, _ time.Duration, tokenEnv string) (string, error) {
	key := repoKey{URL: url, Branch: branch}

	r.mu.Lock()
	if dir, ok := r.clones[key]; ok {
		r.mu.Unlock()
		return dir, nil
	}
	r.mu.Unlock()

	// Deterministic directory name from url+branch so re-adding the same pair
	// reuses the existing clone on disk.
	h := sha256.Sum256([]byte(url + "@" + branch))
	dir := filepath.Join(r.dataDir, "repos", fmt.Sprintf("ts-%x", h[:8]))

	if err := cloneOrPull(ctx, dir, url, branch, tokenEnv); err != nil {
		return "", fmt.Errorf("clone %s@%s: %w", url, branch, err)
	}

	r.mu.Lock()
	r.clones[key] = dir
	r.mu.Unlock()

	return dir, nil
}

// ClonedRepos returns a snapshot of all (url, branch) → localDir mappings.
// Used by tests and diagnostics.
func (r *Resolver) ClonedRepos() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.clones))
	for k, v := range r.clones {
		out[k.URL+"@"+k.Branch] = v
	}
	return out
}

// buildOverrideLayers assembles the six-level precedence stack (lowest first):
//  1. (task.yaml base — not in this function, it is the base passed to applyOverrides)
//  2. configDefaults — from kind:Config file
//  3. setDefaults — from this TaskSet's spec.defaults
//  4. parentDefaults — pushed in by the parent's overrides.defaults
//  5. parentEntryPatch — parent's overrides.entries[key]
//  6. entryOverrides — this entry's own overrides block  ← highest
func buildOverrideLayers(configDefaults, setDefaults *Defaults, parentOverrides, parentEntryOverride, entryOverrides *Overrides) []*Overrides {
	layers := make([]*Overrides, 0, 5)
	layers = append(layers, defaultsToOverrides(configDefaults))
	layers = append(layers, defaultsToOverrides(setDefaults))
	if parentOverrides != nil {
		layers = append(layers, defaultsToOverrides(parentOverrides.Defaults))
	}
	layers = append(layers, parentEntryOverride)
	layers = append(layers, entryOverrides) // entry overrides win (leaf wins)
	return layers
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
func mergeOverrides(a, b *Overrides) *Overrides {
	if a == nil {
		return b
	}
	if b == nil {
		return a
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
	// Env: merge by key (a first, b wins)
	if len(a.Env) > 0 || len(out.Env) > 0 {
		out.Env = mergeEnv(a.Env, out.Env)
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
	return &out
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
