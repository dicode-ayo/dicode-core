package daemon

import (
	"github.com/dicode/dicode/pkg/registry"
	"go.uber.org/zap"
)

// suspendNotifier turns a run's suspend (and a suspended conversation's end)
// into operator notification, mirroring approvalNotifier: when a notify_task is
// configured it fires that task so the operator's delivery task (slack/email/
// ntfy) can ping them to come answer the paused agent.
//
// It subscribes to the engine's run-finished hook, which fires for every
// terminal-ish status including `suspended`. Notification is best-effort and
// must never affect engine behavior — onRunFinished never blocks the hook.
type suspendNotifier struct {
	notifyTask string
	// resumeURL builds the WebUI link an operator follows to answer a suspended
	// run. May be nil (the notification still carries run_id/task_id).
	resumeURL func(runID string) string
	fire      func(taskID string, params map[string]string) error
	log       *zap.Logger
}

// onRunFinished is the engine run-finished hook. It must be non-blocking; the
// fire itself is dispatched on a goroutine.
func (n suspendNotifier) onRunFinished(taskID, runID, status, triggerSource string, _ int64) {
	if n.notifyTask == "" || taskID == n.notifyTask {
		// Disabled, or this is the notify task's own run — never notify about
		// that (and it breaks a fire→finish→fire loop).
		return
	}

	switch status {
	case string(registry.StatusSuspended):
		resume := ""
		if n.resumeURL != nil {
			resume = n.resumeURL(runID)
		}
		body := "Task " + taskID + " is paused for your input."
		if resume != "" {
			body += " Resume: " + resume
		}
		n.dispatch(taskID, runID, map[string]string{
			// Rendered fields for buildin/notifications (title + body required);
			// structured fields for custom delivery tasks.
			"title": "dicode: an agent needs your reply", "body": body, "priority": "default",
			"event": "suspended", "run_id": runID, "task_id": taskID, "resume_url": resume,
		})

	case string(registry.StatusSuccess), string(registry.StatusCancelled), string(registry.StatusFailure):
		// A conversation ended only when THIS terminal run is a resume
		// continuation. Chain children and pipeline stages also inherit
		// root != self, so the trigger source — unique to resumed runs — is the
		// precise discriminator (and avoids a per-run registry lookup).
		if triggerSource != string(registry.TriggerResume) {
			return
		}
		n.dispatch(taskID, runID, map[string]string{
			"title": "dicode: conversation ended", "body": "Task " + taskID + " finished (" + status + ").", "priority": "low",
			"event": "ended", "run_id": runID, "task_id": taskID, "status": status,
		})
	}
}

func (n suspendNotifier) dispatch(taskID, runID string, params map[string]string) {
	if n.fire == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil && n.log != nil {
				n.log.Error("suspend notify hook panicked", zap.String("run", runID), zap.Any("panic", r))
			}
		}()
		if err := n.fire(n.notifyTask, params); err != nil && n.log != nil {
			// Best-effort and default-on (buildin/notifications), which legitimately
			// fails on a headless daemon with no desktop — Debug, not Warn, so
			// it doesn't spam a server's log every suspend.
			n.log.Debug("suspend notify task did not fire",
				zap.String("run", runID),
				zap.String("task", taskID),
				zap.String("notify_task", n.notifyTask),
				zap.Error(err))
		}
	}()
}
