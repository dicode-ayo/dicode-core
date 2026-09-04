package ipc

import (
	"errors"

	"go.uber.org/zap"
)

// TaskApproveResult is the cli.task.approve response.
type TaskApproveResult struct {
	TaskID   string `json:"taskID"`
	Approved bool   `json:"approved"`
	// Enabled is the approved task's resolved enabled flag, so the CLI can
	// report accurately whether approval actually armed any triggers: a
	// disabled task (task.yaml or a taskset override) arms none regardless
	// of approval state (#822). Defaults to true when no lookup is wired
	// (SetTaskEnabled not called, or a lookup miss), matching the
	// historical unconditional "triggers armed" wording.
	Enabled bool `json:"enabled"`
}

// SetTaskApprover wires the approval gate's Approve for cli.task.approve
// dispatch. The control socket is a trusted local channel (0600 socket +
// peer-UID / pre-shared-token handshake), so no further auth is layered on.
// Nil leaves the method returning a clear error (tests without the gate).
func (cs *ControlServer) SetTaskApprover(a func(taskID string) error) { cs.taskApprover = a }

// SetTaskEnabled wires a lookup for a just-approved task's resolved enabled
// flag — see the taskEnabled field doc.
func (cs *ControlServer) SetTaskEnabled(f func(taskID string) (enabled, ok bool)) {
	cs.taskEnabled = f
}

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
	enabled := true
	if cs.taskEnabled != nil {
		if e, ok := cs.taskEnabled(req.TaskID); ok {
			enabled = e
		}
	}
	return TaskApproveResult{TaskID: req.TaskID, Approved: true, Enabled: enabled}, nil
}
