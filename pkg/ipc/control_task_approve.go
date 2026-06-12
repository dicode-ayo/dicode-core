package ipc

import (
	"errors"

	"go.uber.org/zap"
)

// TaskApproveResult is the cli.task.approve response.
type TaskApproveResult struct {
	TaskID   string `json:"taskID"`
	Approved bool   `json:"approved"`
}

// SetTaskApprover wires the approval gate's Approve for cli.task.approve
// dispatch. The control socket is a trusted local channel (0600 socket +
// peer-UID / pre-shared-token handshake), so no further auth is layered on.
// Nil leaves the method returning a clear error (tests without the gate).
func (cs *ControlServer) SetTaskApprover(a func(taskID string) error) { cs.taskApprover = a }

// handleTaskApprove approves a task held pending by the approval gate. The
// gate records the observed content hash in dicode.lock and arms the task's
// triggers; a non-pending task id is an error.
func (cs *ControlServer) handleTaskApprove(req Request) (TaskApproveResult, error) {
	if req.TaskID == "" {
		return TaskApproveResult{}, errors.New("taskID required")
	}
	if cs.taskApprover == nil {
		return TaskApproveResult{}, errors.New("approval gate not configured")
	}
	if err := cs.taskApprover(req.TaskID); err != nil {
		return TaskApproveResult{TaskID: req.TaskID}, err
	}
	cs.log.Info("task approved via control socket", zap.String("task", req.TaskID))
	return TaskApproveResult{TaskID: req.TaskID, Approved: true}, nil
}
