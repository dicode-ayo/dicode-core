package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/schemavalidate"
	"go.uber.org/zap"
)

// Resume errors the control server classifies into CLI messages. pkg/ipc cannot
// import pkg/trigger (trigger imports ipc), so the daemon's Resumer adapter
// translates trigger's typed sentinels into these before they reach
// handleResume.
var (
	ErrResumeTokenNotFound = errors.New("resume token not found")
	ErrResumeNotSuspended  = errors.New("run is not suspended")
	ErrResumeExpired       = errors.New("resume deadline expired")
	ErrResumePending       = errors.New("resume blocked: task not admitted")
)

// Resumer spawns the continuation run for a suspended run given its resume
// token. *trigger.Engine.ResumeRun satisfies the shape; the daemon wraps it in
// an adapter that maps trigger's typed errors onto the ErrResume* sentinels
// above so this package needs no dependency on pkg/trigger.
type Resumer interface {
	ResumeRun(ctx context.Context, token string, input []byte) (newRunID string, err error)
}

// SetResumer wires the resume backend for cli.resume dispatch. Nil leaves the
// method returning a clear error (tests / configurations without a running
// engine).
func (cs *ControlServer) SetResumer(r Resumer) { cs.resumer = r }

// ResumeResult is the cli.resume response — the continuation run's ID.
type ResumeResult struct {
	RunID string `json:"runID"`
}

// ResumeInfo is the cli.resume.get response: a suspended run's JSON Schema so
// the CLI can prompt per property, coerce values to their declared types, and
// validate before submitting.
type ResumeInfo struct {
	RunID  string          `json:"runID"`
	TaskID string          `json:"taskID"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

// SuspendedRunSummary is one row in the cli.resume.list response. Fields is the
// list of property names the task's JSON Schema requires, so the CLI can hint
// which key=value pairs to supply.
type SuspendedRunSummary struct {
	RunID       string   `json:"runID"`
	TaskID      string   `json:"taskID"`
	SuspendedAt string   `json:"suspendedAt"` // RFC3339 or ""
	Deadline    string   `json:"deadline"`    // RFC3339 or ""
	Fields      []string `json:"fields"`
}

// handleResume resolves a suspended run server-side and spawns its continuation.
// The client supplies only the run id and the collected key=value input — never
// the token: the token is read from the stored run, mirroring the webui's
// authorization model where the session (here, the trusted control socket) is
// the authority and the resume handle stays server-side. The input is validated
// against the run's stored JSON Schema before the continuation is spawned.
func (cs *ControlServer) handleResume(ctx context.Context, req Request) (ResumeResult, error) {
	if cs.resumer == nil {
		return ResumeResult{}, errors.New("resume not available (engine not wired)")
	}
	if req.RunID == "" {
		return ResumeResult{}, errors.New("runID required")
	}
	run, err := cs.reg.GetRun(ctx, req.RunID)
	if err != nil {
		return ResumeResult{}, fmt.Errorf("run %q not found", req.RunID)
	}
	if run.Status != registry.StatusSuspended || run.ResumeToken == "" {
		return ResumeResult{}, errors.New("run is not suspended")
	}

	// The key=value pairs arrive as a JSON object in Params; pass through as the
	// opaque resume input the task reads as ctx.resume_input.
	input := []byte(req.Params)
	if len(input) == 0 {
		input = []byte("{}")
	}

	if err := schemavalidate.Validate(run.ResumeSchema, input); err != nil {
		return ResumeResult{}, fmt.Errorf("invalid resume input: %w", err)
	}

	newRunID, err := cs.resumer.ResumeRun(ctx, run.ResumeToken, input)
	if err != nil {
		switch {
		case errors.Is(err, ErrResumeTokenNotFound):
			return ResumeResult{}, errors.New("resume token not found — the suspended run may have been cleaned up")
		case errors.Is(err, ErrResumeNotSuspended):
			return ResumeResult{}, errors.New("run is not suspended — it may have already been resumed")
		case errors.Is(err, ErrResumeExpired):
			return ResumeResult{}, errors.New("resume deadline expired — this run can no longer be resumed")
		case errors.Is(err, ErrResumePending):
			return ResumeResult{}, errors.New("task is awaiting approval — resume once it is approved (dicode task approve <task-id>)")
		}
		return ResumeResult{}, err
	}
	cs.log.Info("run resumed via control socket",
		zap.String("suspended_run", req.RunID), zap.String("continuation_run", newRunID))
	return ResumeResult{RunID: newRunID}, nil
}

// handleResumeGet returns a suspended run's JSON Schema so the CLI can build its
// prompt and coerce/validate the input locally before submitting.
func (cs *ControlServer) handleResumeGet(ctx context.Context, req Request) (ResumeInfo, error) {
	if req.RunID == "" {
		return ResumeInfo{}, errors.New("runID required")
	}
	run, err := cs.reg.GetRun(ctx, req.RunID)
	if err != nil {
		return ResumeInfo{}, fmt.Errorf("run %q not found", req.RunID)
	}
	if run.Status != registry.StatusSuspended || run.ResumeToken == "" {
		return ResumeInfo{}, errors.New("run is not suspended")
	}
	info := ResumeInfo{RunID: run.ID, TaskID: run.TaskID}
	if len(run.ResumeSchema) > 0 {
		info.Schema = json.RawMessage(run.ResumeSchema)
	}
	return info, nil
}

// handleResumeList returns the runs currently awaiting resume so the CLI can
// show the operator what's resumable without hunting through per-task logs.
func (cs *ControlServer) handleResumeList(ctx context.Context) ([]SuspendedRunSummary, error) {
	runs, err := cs.reg.ListSuspendedRuns(ctx, 100)
	if err != nil {
		return nil, err
	}
	out := make([]SuspendedRunSummary, 0, len(runs))
	for _, r := range runs {
		s := SuspendedRunSummary{
			RunID:  r.ID,
			TaskID: r.TaskID,
			Fields: schemaRequiredFields(r.ResumeSchema),
		}
		if r.SuspendedAt > 0 {
			s.SuspendedAt = time.UnixMilli(r.SuspendedAt).UTC().Format(time.RFC3339)
		}
		if r.ResumeDeadline > 0 {
			s.Deadline = time.UnixMilli(r.ResumeDeadline).UTC().Format(time.RFC3339)
		}
		out = append(out, s)
	}
	return out, nil
}

// schemaRequiredFields extracts the required property names from a persisted
// JSON Schema. When the schema declares no required list, it falls back to all
// declared property names so the listing still hints at what to supply. A
// malformed or empty schema yields no names — the listing degrades to run id +
// task rather than failing.
func schemaRequiredFields(schemaJSON []byte) []string {
	if len(schemaJSON) == 0 {
		return nil
	}
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		return nil
	}
	if len(schema.Required) > 0 {
		return schema.Required
	}
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	return names
}
