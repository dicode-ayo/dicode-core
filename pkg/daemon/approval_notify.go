package daemon

import "go.uber.org/zap"

// approvalNotifier turns an approval-gate "task went pending" transition into
// operator notification: it always broadcasts the WebUI approval:pending
// event, and, when a notify_task is configured, fires it with the approve
// details so the operator's delivery task (slack/email/ntfy) can reach them.
//
// The notify_task is itself subject to the approval gate. The operator must
// point notify_task at a builtin or trusted task; an untrusted notify_task
// would sit pending and never fire (the fire guard vetoes it). That is a
// no-op, not a deadlock — fire just returns a veto error, which is logged.
type approvalNotifier struct {
	notifyTask string
	broadcast  func(taskID, hash string)
	mintLink   func(taskID string) (string, error)
	fire       func(taskID string, params map[string]string) error
	log        *zap.Logger
}

// notify runs the broadcast + (optional) notify-task fire for one pending
// transition. It never returns an error: notification is best-effort and must
// not affect gate or reconciler behavior. Safe to call from a goroutine.
func (n approvalNotifier) notify(taskID, hash string) {
	if n.broadcast != nil {
		n.broadcast(taskID, hash)
	}
	if n.notifyTask == "" {
		// Gate already logged the remediation hint (WARN) on hold.
		return
	}
	// Mint the single-use approve link. On failure, still notify with an empty
	// URL so the operator at least learns a task went pending.
	var approveURL string
	if n.mintLink != nil {
		url, err := n.mintLink(taskID)
		if err != nil && n.log != nil {
			n.log.Debug("approval notify: mint approve link failed; notifying without URL",
				zap.String("task", taskID), zap.Error(err))
		}
		approveURL = url
	}
	if n.fire == nil {
		return
	}
	if err := n.fire(n.notifyTask, map[string]string{
		"task_id":     taskID,
		"hash":        hash,
		"approve_url": approveURL,
	}); err != nil && n.log != nil {
		// The single-use token lives in approveURL — keep it out of the log.
		n.log.Warn("approval notify task failed to fire",
			zap.String("task", taskID),
			zap.String("notify_task", n.notifyTask),
			zap.Error(err))
	}
}
