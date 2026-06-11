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
// so the caller opens a PR via buildin/git-pr. The clone is intentionally left
// in place for the PR task to operate in; the dev-clones-cleanup buildin task
// sweeps it afterwards.
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

// deleteLocal removes the task directory under a local source. It refuses to
// remove a directory that does not sit under the source's resolved root — a
// defense against a stale or attacker-supplied TaskDir escaping the source.
func (m *SourceManager) deleteLocal(taskID, sourceName string, spec *task.Spec, src *taskset.Source) (ipc.TaskDeleteOutcome, error) {
	root := src.RepoPath()
	if root == "" {
		return ipc.TaskDeleteOutcome{}, fmt.Errorf("source %q root path not resolved", sourceName)
	}
	taskDir := filepath.Clean(spec.TaskDir)
	root = filepath.Clean(root)
	if taskDir != root && !strings.HasPrefix(taskDir+string(filepath.Separator), root+string(filepath.Separator)) {
		return ipc.TaskDeleteOutcome{}, fmt.Errorf("task directory %q is not under source %q root %q; refusing to remove", taskDir, sourceName, root)
	}
	if taskDir == root {
		return ipc.TaskDeleteOutcome{}, fmt.Errorf("task directory equals source root %q; refusing to remove", root)
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
func (m *SourceManager) deleteGit(ctx context.Context, taskID, sourceName string, spec *task.Spec, src *taskset.Source, ref *taskset.Ref) (ipc.TaskDeleteOutcome, error) {
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

	clonePath := filepath.Clean(src.RepoPath())
	cloneTaskDir := filepath.Join(clonePath, rel)
	if !strings.HasPrefix(filepath.Clean(cloneTaskDir)+string(filepath.Separator), clonePath+string(filepath.Separator)) {
		return ipc.TaskDeleteOutcome{}, fmt.Errorf("clone task path escapes clone root: %q", cloneTaskDir)
	}
	if err := os.RemoveAll(cloneTaskDir); err != nil {
		return ipc.TaskDeleteOutcome{}, fmt.Errorf("remove task in clone: %w", err)
	}

	authToken := ""
	if ref.Auth.TokenEnv != "" {
		authToken = os.Getenv(ref.Auth.TokenEnv)
	}

	// All:true stages the deletion (go-git treats a removed worktree file as a
	// staged removal), so no explicit `git rm` index call is needed.
	if _, err := gitSource.CommitPush(ctx, clonePath, gitSource.CommitPushOptions{
		Message:      fmt.Sprintf("Delete task %s", taskID),
		Branch:       branch,
		BranchPrefix: deleteBranchPrefix,
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
