package ipc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/dicode/dicode/pkg/task"
)

// buildinSourcePrefix is the namespace prefix of the shipped buildin taskset.
// Tasks under it are part of the binary's reference taskset and cannot be
// removed via `dicode task delete` — operators override them through the
// taskset entry mechanism instead.
const buildinSourcePrefix = "buildin/"

// TaskDeleteOutcome reports what a TaskDeleter did. For local sources only
// Mode and Source are set. For git sources the task was committed onto Branch
// in a fresh dev-clone at ClonePath and pushed; the caller then fires the
// PR-opening task.
type TaskDeleteOutcome struct {
	Source    string
	Mode      string // "local" | "git"
	Branch    string
	Base      string
	ClonePath string
}

// TaskDeleter removes a task from its owning source. It is implemented by
// webui.SourceManager (which owns source state, repo paths, and dev-clones).
// Defined here so pkg/ipc need not import pkg/webui — the same decoupling
// pattern as APIKeyMinter and SourceDevModeSetter.
type TaskDeleter interface {
	// ResolveTaskSource returns the owning source name for a task and whether
	// that source is a git source. sourceOverride, when non-empty, forces the
	// resolution to that source (and validates the task belongs to it).
	ResolveTaskSource(taskID, sourceOverride string) (name string, isGit bool, err error)

	// DeleteTaskFromSource performs the removal. For a local source it removes
	// the task directory in place. For a git source it clones the repo, removes
	// the task directory, commits, and pushes to delete/<sanitized-task-id>;
	// the returned outcome carries the branch + clone path so the caller can
	// open a PR. spec.TaskDir locates the task on disk.
	DeleteTaskFromSource(ctx context.Context, taskID, sourceName string, spec *task.Spec) (TaskDeleteOutcome, error)
}

// SetTaskDeleter wires the task-deletion backend for cli.task.delete dispatch.
// Nil leaves the method returning a clear error (tests without webui).
func (cs *ControlServer) SetTaskDeleter(d TaskDeleter) { cs.taskDeleter = d }

// gitPRTaskID is the buildin task that opens the pull request for a git-source
// deletion — the same task the AI-authoring save flow and auto-fix loop use.
const gitPRTaskID = "buildin/git-pr"

// handleTaskDelete removes a task from its owning source — or, when Force is
// false, returns a preview so the CLI can render its confirmation prompt.
//
// Resolution → guards → deletion:
//  1. The task must exist in the registry (so we know its on-disk path).
//  2. Buildin tasks are undeletable.
//  3. Chained references (on_failure / chain triggers pointing at this task),
//     the trigger schedule, and the owning source are surfaced in the result
//     so the CLI can warn the operator. Stale run rows are unaffected —
//     apiGetRun falls back to the task id when the spec is gone.
//  4. The actual removal happens ONLY when Force is true. Local sources: the
//     directory is removed in place. Git sources: the removal is pushed to a
//     delete/<id> branch and buildin/git-pr opens a PR.
//
// The reconciler deregisters the task on its next sync (~30s).
func (cs *ControlServer) handleTaskDelete(ctx context.Context, req Request) (TaskDeleteResult, error) {
	if req.TaskID == "" {
		return TaskDeleteResult{}, errors.New("taskID required")
	}
	if strings.HasPrefix(req.TaskID, buildinSourcePrefix) {
		return TaskDeleteResult{}, fmt.Errorf(
			"task %q is a buildin task and cannot be deleted — override it via a taskset entry (set `enabled: false` or point the entry at your own ref) instead",
			req.TaskID)
	}
	if cs.taskDeleter == nil {
		return TaskDeleteResult{}, errors.New("task deletion not configured (no source manager)")
	}

	spec, ok := cs.reg.Get(req.TaskID)
	if !ok {
		return TaskDeleteResult{}, fmt.Errorf("task %q not found", req.TaskID)
	}

	sourceName, _, err := cs.taskDeleter.ResolveTaskSource(req.TaskID, req.Source)
	if err != nil {
		return TaskDeleteResult{}, err
	}

	refs := cs.chainReferrers(req.TaskID)

	// Preview: resolve + guards only, no destructive action. The CLI renders
	// the confirmation prompt from this and re-sends with Force=true.
	if !req.Force {
		return TaskDeleteResult{
			TaskID:  req.TaskID,
			Source:  sourceName,
			Mode:    "preview",
			Trigger: triggerLabel(spec),
			Refs:    refs,
		}, nil
	}

	outcome, err := cs.taskDeleter.DeleteTaskFromSource(ctx, req.TaskID, sourceName, spec)
	if err != nil {
		return TaskDeleteResult{Refs: refs}, err
	}

	res := TaskDeleteResult{
		TaskID: req.TaskID,
		Source: sourceName,
		Mode:   outcome.Mode,
		Branch: outcome.Branch,
		Refs:   refs,
	}

	if outcome.Mode != "git" {
		return res, nil
	}

	// Git source: open the PR via the same buildin task the authoring save
	// flow uses. A push without a PR would still be picked up by the
	// reconciler, but the operator needs the PR to actually merge the
	// removal — so a PR-task failure fails the whole delete.
	prParams := map[string]string{
		"source_id":  sourceName,
		"branch":     outcome.Branch,
		"base":       outcome.Base,
		"title":      fmt.Sprintf("Delete task %s", req.TaskID),
		"body":       fmt.Sprintf("Removes task `%s` from its source. Filed by `dicode task delete`.", req.TaskID),
		"clone_path": outcome.ClonePath,
	}
	runID, err := cs.engine.FireManual(ctx, gitPRTaskID, prParams)
	if err != nil {
		return res, fmt.Errorf("delete pushed to branch %q but opening the PR failed: %w", outcome.Branch, err)
	}
	res.PRRunID = runID
	runRes, err := cs.engine.WaitRun(ctx, runID)
	if err != nil {
		return res, fmt.Errorf("delete pushed to branch %q but the PR task did not complete: %w", outcome.Branch, err)
	}
	if runRes.Status != "success" {
		return res, fmt.Errorf("delete pushed to branch %q but the PR task finished %s (run %s)", outcome.Branch, runRes.Status, runID)
	}
	res.PRValue = prReturnString(runRes.ReturnValue)
	return res, nil
}

// chainReferrers returns the ids of registered tasks that reference target via
// a chain trigger (trigger.chain.from) or an on_failure_chain (on_failure_chain.task).
// The result is sorted and excludes target itself.
func (cs *ControlServer) chainReferrers(target string) []string {
	var refs []string
	for _, s := range cs.reg.All() {
		if s.ID == target {
			continue
		}
		if s.Trigger.Chain != nil && s.Trigger.Chain.From == target {
			refs = append(refs, s.ID)
			continue
		}
		if s.OnFailureChain != nil && s.OnFailureChain.Task == target {
			refs = append(refs, s.ID)
		}
	}
	sort.Strings(refs)
	return refs
}

// prReturnString renders the buildin/git-pr task's return value as a string —
// the PR URL when the task returns one. A bare string passes through; other
// shapes are stringified so the operator still sees something useful.
func prReturnString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		return fmt.Sprintf("%v", t)
	}
}
