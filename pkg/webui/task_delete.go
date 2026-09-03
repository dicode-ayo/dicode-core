package webui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dicode/dicode/internal/pathguard"
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
	canonRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return "", fmt.Errorf("resolve source %q root: %w", sourceName, err)
	}
	if canonRoot == string(filepath.Separator) {
		// A source rooted at the filesystem root would make every path
		// "contained"; nothing legitimate is configured that way.
		return "", fmt.Errorf("source %q root resolves to the filesystem root; refusing to remove", sourceName)
	}
	canonTask, err := pathguard.ResolveExisting(taskDir)
	if err != nil {
		return "", fmt.Errorf("resolve task directory %q: %w", taskDir, err)
	}
	if canonTask == canonRoot {
		return "", fmt.Errorf("task directory equals source root %q; refusing to remove", canonRoot)
	}
	if within, werr := pathguard.Within(canonRoot, canonTask); werr != nil || !within {
		return "", fmt.Errorf("task directory %q is not under source %q root %q; refusing to remove", canonTask, sourceName, canonRoot)
	}
	return canonTask, nil
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
	// Drop the taskset entry first: an entry left pointing at a removed
	// directory resolves as a load failure on every sync, so the task would
	// stay visible as broken instead of gone. RemoveTaskEntry matches the ref
	// against taskDir, which must still exist for that comparison.
	if _, _, err := m.removeTasksetEntry(taskID, sourceName, taskDir, src); err != nil {
		return ipc.TaskDeleteOutcome{}, err
	}
	if err := os.RemoveAll(taskDir); err != nil {
		return ipc.TaskDeleteOutcome{}, fmt.Errorf("remove task directory: %w", err)
	}
	m.log.Info("task deleted from local source",
		zap.String("task", taskID), zap.String("source", sourceName), zap.String("dir", taskDir))
	return ipc.TaskDeleteOutcome{Source: sourceName, Mode: "local"}, nil
}

// removeTasksetEntry drops the deleted task's key from the source's root
// taskset file. Only a task listed directly by that file is handled: a deeper
// id names an entry inside a nested taskset, which the nested file owns.
//
// It returns the taskset file's absolute path (even when nothing needed
// removing, as long as one governs this source) and whether an entry was
// actually removed, so a git-mode caller can stage the file alongside the
// deleted task directory in the same commit.
func (m *SourceManager) removeTasksetEntry(taskID, sourceName, taskDir string, src *taskset.Source) (tsPath string, removed bool, err error) {
	key := strings.TrimPrefix(taskID, sourceName+"/")
	if key == "" || key == taskID || strings.Contains(key, "/") {
		return "", false, nil
	}
	tsPath = src.RootTaskSetPath()
	if tsPath == "" {
		return "", false, nil
	}
	removed, err = taskset.RemoveTaskEntry(tsPath, key, taskDir)
	if err != nil {
		return tsPath, false, fmt.Errorf("remove taskset entry: %w", err)
	}
	if removed {
		m.log.Info("taskset entry removed",
			zap.String("task", taskID), zap.String("source", sourceName), zap.String("taskset", tsPath))
	}
	return tsPath, removed, nil
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
	// The removal has to be cut from what the source actually runs, which on a
	// pinned source is the tagged commit and never the default branch.
	base := ref.Branch
	if base == "" {
		base = ref.Tag
	}
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
			_ = src.SetDevMode(ctx, false, taskset.DevModeOpts{RunID: runID})
		}
	}()

	clonePath := filepath.Clean(src.RepoPath())
	var cloneTaskDir string
	cloneTaskDir, err = containedTaskDir(clonePath, filepath.Join(clonePath, rel), sourceName)
	if err != nil {
		return ipc.TaskDeleteOutcome{}, err
	}

	// Drop the taskset entry before removing the directory: RemoveTaskEntry
	// matches the ref against taskDir, which must still exist for that
	// comparison (mirrors deleteLocal). Left dangling, the merged commit would
	// carry an entry pointing at a directory that no longer exists, which
	// resolves as a load failure on every sync from then on.
	var tsPath string
	var tsEntryRemoved bool
	tsPath, tsEntryRemoved, err = m.removeTasksetEntry(taskID, sourceName, cloneTaskDir, src)
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

	// Stage the removed task path plus the taskset file when the entry removal
	// touched it: go-git records a deleted worktree file as a staged removal
	// when its tracked path is Add-ed. An empty Files would stage every change
	// in the clone, including unrelated artifacts.
	files := []string{rel}
	if tsEntryRemoved {
		var tsRel string
		tsRel, err = filepath.Rel(clonePath, tsPath)
		if err != nil || strings.HasPrefix(tsRel, "..") {
			return ipc.TaskDeleteOutcome{}, fmt.Errorf("taskset file %q is outside clone %q", tsPath, clonePath)
		}
		files = append(files, tsRel)
	}

	if _, err = gitSource.CommitPush(ctx, clonePath, gitSource.CommitPushOptions{
		Message:      fmt.Sprintf("Delete task %s", taskID),
		Branch:       branch,
		BranchPrefix: deleteBranchPrefix,
		Files:        files,
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
