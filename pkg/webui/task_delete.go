package webui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dicode/dicode/pkg/ipc"
	gitSource "github.com/dicode/dicode/pkg/source/git"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"go.uber.org/zap"
)

// deleteBranchPrefix is the branch namespace deletions are pushed to. The PR
// the buildin/git-pr task opens targets the source's tracked branch.
const deleteBranchPrefix = "delete/"

// deleteCommitAuthor names the author on deletion commits. Operator-facing
// attribution is fixed here because the control socket has no per-user
// identity to thread through.
var deleteCommitAuthor = gitSource.Signature{Name: "dicode", Email: "dicode@localhost"}

// ResolveTaskSource maps a task id to its owning source. It implements
// ipc.TaskDeleter. The source is the first path segment of the task id unless
// sourceOverride forces a specific source (in which case the task id must be
// namespaced under it). The returned isGit reports whether the source is a git
// ref (vs a local-path ref).
func (m *SourceManager) ResolveTaskSource(taskID, sourceOverride string) (string, bool, error) {
	name := sourceOverride
	if name == "" {
		seg, _, ok := strings.Cut(taskID, "/")
		if !ok || seg == "" {
			return "", false, fmt.Errorf("cannot resolve source for task %q: not namespaced — pass --source", taskID)
		}
		name = seg
	} else if !strings.HasPrefix(taskID, sourceOverride+"/") {
		return "", false, fmt.Errorf("task %q is not under source %q", taskID, sourceOverride)
	}

	m.mu.RLock()
	_, isTaskset := m.tasksets[name]
	m.mu.RUnlock()
	if !isTaskset {
		return "", false, fmt.Errorf("source %q not found or not a taskset source", name)
	}

	m.rLockCfg()
	entry := m.cfg.Spec.Entries[name]
	m.rUnlockCfg()
	if entry == nil || entry.Ref == nil {
		return "", false, fmt.Errorf("source %q has no ref", name)
	}
	return name, entry.Ref.IsGit(), nil
}

// DeleteTaskFromSource removes a task from its source. It implements
// ipc.TaskDeleter.
//
// Local sources: the task directory is removed in place; the reconciler
// deregisters the task on its next sync.
//
// Git sources: the repo is cloned (dev-mode clone-mode), the task directory is
// removed from the clone, and the removal is committed and pushed to
// delete/<sanitized-id>. The returned outcome carries the branch + clone path
// so the caller opens a PR via buildin/git-pr. The source is left in clone-mode
// so the PR task can run `gh` inside the clone; the caller reverts it via
// DisableSourceDevMode once the PR step finishes.
func (m *SourceManager) DeleteTaskFromSource(ctx context.Context, taskID, sourceName string, spec *task.Spec) (ipc.TaskDeleteOutcome, error) {
	if spec == nil || spec.TaskDir == "" {
		return ipc.TaskDeleteOutcome{}, fmt.Errorf("task %q has no on-disk directory", taskID)
	}

	m.mu.RLock()
	src, ok := m.tasksets[sourceName]
	m.mu.RUnlock()
	if !ok {
		return ipc.TaskDeleteOutcome{}, fmt.Errorf("source %q not found", sourceName)
	}

	m.rLockCfg()
	entry := m.cfg.Spec.Entries[sourceName]
	m.rUnlockCfg()
	if entry == nil || entry.Ref == nil {
		return ipc.TaskDeleteOutcome{}, fmt.Errorf("source %q has no ref", sourceName)
	}

	if !entry.Ref.IsGit() {
		return m.deleteLocal(taskID, sourceName, spec, src)
	}
	return m.deleteGit(ctx, taskID, sourceName, spec, src, entry.Ref)
}

// containedTaskDir canonicalises root and taskDir (resolving symlinks on every
// existing path component) and verifies taskDir sits strictly under root. A
// symlinked source root, intermediate component, or task dir cannot let the
// lexical path pass while os.RemoveAll follows the link outside root. It returns
// the canonical task-dir path to remove. The task dir need not exist (nothing
// to remove); the root must.
func containedTaskDir(root, taskDir, sourceName string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("source %q root path not resolved", sourceName)
	}
	if taskDir == "" {
		return "", fmt.Errorf("task %q has no on-disk directory", sourceName)
	}
	canonRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return "", fmt.Errorf("resolve source %q root: %w", sourceName, err)
	}
	canonTask, err := resolveExisting(filepath.Clean(taskDir))
	if err != nil {
		return "", fmt.Errorf("resolve task directory %q: %w", taskDir, err)
	}
	if canonTask == canonRoot {
		return "", fmt.Errorf("task directory equals source root %q; refusing to remove", canonRoot)
	}
	if !strings.HasPrefix(canonTask+string(filepath.Separator), canonRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("task directory %q is not under source %q root %q; refusing to remove", canonTask, sourceName, canonRoot)
	}
	return canonTask, nil
}

// resolveExisting canonicalises p with EvalSymlinks, walking up to the nearest
// existing ancestor when p itself does not exist and re-appending the missing
// tail. This canonicalises every real (and therefore symlink-bearing) component
// while tolerating an already-absent task dir.
func resolveExisting(p string) (string, error) {
	resolved, err := filepath.EvalSymlinks(p)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(p)
	if parent == p {
		return "", err
	}
	resolvedParent, perr := resolveExisting(parent)
	if perr != nil {
		return "", perr
	}
	return filepath.Join(resolvedParent, filepath.Base(p)), nil
}

// deleteLocal removes the task directory under a local source. It refuses to
// remove a directory that does not sit under the source's canonical root —
// defends against a stale or attacker-supplied TaskDir (or a symlink) escaping
// the source.
func (m *SourceManager) deleteLocal(taskID, sourceName string, spec *task.Spec, src *taskset.Source) (ipc.TaskDeleteOutcome, error) {
	taskDir, err := containedTaskDir(src.RepoPath(), spec.TaskDir, sourceName)
	if err != nil {
		return ipc.TaskDeleteOutcome{}, err
	}
	if err := os.RemoveAll(taskDir); err != nil {
		return ipc.TaskDeleteOutcome{}, fmt.Errorf("remove task directory: %w", err)
	}
	m.log.Info("task deleted from local source",
		zap.String("task", taskID), zap.String("source", sourceName), zap.String("dir", taskDir))
	return ipc.TaskDeleteOutcome{Source: sourceName, Mode: "local"}, nil
}

// deleteGit clones the source repo, removes the task directory, and pushes the
// removal to delete/<sanitized-id>.
func (m *SourceManager) deleteGit(ctx context.Context, taskID, sourceName string, spec *task.Spec, src *taskset.Source, ref *taskset.Ref) (out ipc.TaskDeleteOutcome, err error) {
	// Repo-relative path of the task, computed against the PRIMARY repo path
	// before clone-mode swaps RepoPath() to the clone dir.
	primaryRoot := filepath.Clean(src.RepoPath())
	taskDir := filepath.Clean(spec.TaskDir)
	rel, err := filepath.Rel(primaryRoot, taskDir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ipc.TaskDeleteOutcome{}, fmt.Errorf("cannot locate task %q within source repo (%q vs %q)", taskID, taskDir, primaryRoot)
	}

	runID := sanitizeRunID(taskID)
	branch := deleteBranchPrefix + taskID
	base := ref.Branch
	if base == "" {
		base = "main"
	}

	if err := src.SetDevMode(ctx, true, taskset.DevModeOpts{Branch: branch, Base: base, RunID: runID}); err != nil {
		return ipc.TaskDeleteOutcome{}, fmt.Errorf("clone source for delete: %w", err)
	}
	// On a successful push the source stays in clone-mode: handleTaskDelete runs
	// the PR task inside the clone, then disables it. On any error here we revert
	// clone-mode ourselves so the source isn't wedged.
	defer func() {
		if err != nil {
			_ = src.SetDevMode(ctx, false, taskset.DevModeOpts{})
		}
	}()

	clonePath := filepath.Clean(src.RepoPath())
	var cloneTaskDir string
	cloneTaskDir, err = containedTaskDir(clonePath, filepath.Join(clonePath, rel), sourceName)
	if err != nil {
		return ipc.TaskDeleteOutcome{}, err
	}
	if err = os.RemoveAll(cloneTaskDir); err != nil {
		return ipc.TaskDeleteOutcome{}, fmt.Errorf("remove task in clone: %w", err)
	}

	authToken := ""
	if ref.Auth.TokenEnv != "" {
		authToken = os.Getenv(ref.Auth.TokenEnv)
	}

	// Stage only the removed task path: go-git records a deleted worktree file as
	// a staged removal when its tracked path is Add-ed. An empty Files would stage
	// every change in the clone, including unrelated artifacts.
	if _, err = gitSource.CommitPush(ctx, clonePath, gitSource.CommitPushOptions{
		Message:      fmt.Sprintf("Delete task %s", taskID),
		Branch:       branch,
		BranchPrefix: deleteBranchPrefix,
		Files:        []string{rel},
		Author:       deleteCommitAuthor,
		AuthToken:    authToken,
	}); err != nil {
		return ipc.TaskDeleteOutcome{}, fmt.Errorf("commit+push deletion: %w", err)
	}

	m.log.Info("task deletion pushed",
		zap.String("task", taskID), zap.String("source", sourceName), zap.String("branch", branch))
	return ipc.TaskDeleteOutcome{
		Source:    sourceName,
		Mode:      "git",
		Branch:    branch,
		Base:      base,
		ClonePath: clonePath,
	}, nil
}

// DisableSourceDevMode reverts a source out of clone-mode. It implements
// ipc.TaskDeleter. Disabling removes the local clone but leaves the pushed
// remote branch intact. Idempotent for sources not in clone-mode.
func (m *SourceManager) DisableSourceDevMode(ctx context.Context, sourceName string) error {
	m.mu.RLock()
	src, ok := m.tasksets[sourceName]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("source %q not found", sourceName)
	}
	return src.SetDevMode(ctx, false, taskset.DevModeOpts{})
}

// sanitizeRunID maps a task id to a valid dev-clone run id (taskset.ValidateRunID:
// [A-Za-z0-9_-]{1,64}). Path separators and other disallowed characters become
// underscores; the result is truncated to 64 characters.
func sanitizeRunID(taskID string) string {
	var b strings.Builder
	for _, r := range taskID {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9', r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	s := b.String()
	if s == "" {
		s = "delete"
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}
