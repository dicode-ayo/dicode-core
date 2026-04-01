package server

import "context"

// RunResult holds the result of a completed run, returned by WaitRun.
type RunResult struct {
	RunID       string      `json:"runID"`
	Status      string      `json:"status"`
	ReturnValue interface{} `json:"returnValue"`
}

// EngineRunner allows the socket server to fire and await task runs.
// Implemented by the trigger engine; passed to New() to avoid import cycles.
type EngineRunner interface {
	FireManual(ctx context.Context, taskID string, params map[string]string) (string, error)
	WaitRun(ctx context.Context, runID string) (RunResult, error)
}
