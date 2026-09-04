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
	// of approval state (#822).
	//
	// A pointer, not a plain bool: this repo supports a version-mixed
	// CLI/daemon pair during an upgrade (see waitDaemonReady), and a daemon
	// built before this field existed omits "enabled" from the JSON
	// entirely. Decoding that into a plain bool would silently read Go's
	// zero value false — reporting every approval from an old daemon as
	// "nothing armed", which is wrong; the old daemon armed triggers
	// normally, it just didn't report this field. A *bool decodes an absent
	// key as nil, which the CLI (cmd/dicode/main.go) treats as "unknown,
	// assume enabled" — the historical pre-this-field behavior — while an
	// explicit false still displays as disabled.
	//
	// The daemon SERVER (handleTaskApprove below) always sets this to a
	// non-nil value on success; nil is only ever produced by decoding an
	// older daemon's response.
	Enabled *bool `json:"enabled"`
}

// SetTaskApprover wires the approval gate's ApproveReporting for
// cli.task.approve dispatch. The control socket is a trusted local channel
// (0600 socket + peer-UID / pre-shared-token handshake), so no further auth
// is layered on. Nil leaves the method returning a clear error (tests
// without the gate).
func (cs *ControlServer) SetTaskApprover(a func(taskID string) (enabled bool, err error)) {
	cs.taskApprover = a
}

// handleTaskApprove approves a task held pending by the approval gate. The
// gate records the observed content hash in dicode.lock and arms the task's
// triggers; a non-pending task id is an error. The approver's returned
// enabled flag — captured atomically as part of the same approval operation,
// not a later separate lookup — is what Enabled reports; see taskApprover's
// field doc for why that atomicity matters.
func (cs *ControlServer) handleTaskApprove(req Request) (TaskApproveResult, error) {
	if req.TaskID == "" {
		return TaskApproveResult{}, errors.New("taskID required")
	}
	if cs.taskApprover == nil {
		return TaskApproveResult{}, errors.New("approval gate not configured")
	}
	enabled, err := cs.taskApprover(req.TaskID)
	if err != nil {
		return TaskApproveResult{TaskID: req.TaskID}, err
	}
	cs.log.Info("task approved via control socket", zap.String("task", req.TaskID))
	return TaskApproveResult{TaskID: req.TaskID, Approved: true, Enabled: &enabled}, nil
}
