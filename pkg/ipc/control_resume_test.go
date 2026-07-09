package ipc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"go.uber.org/zap"
)

// fakeResumer records the token and input it was called with and returns a
// canned result — standing in for the daemon's engine-backed adapter.
type fakeResumer struct {
	gotToken string
	gotInput []byte
	newRunID string
	err      error
	called   bool
}

func (f *fakeResumer) ResumeRun(_ context.Context, token string, input []byte) (string, error) {
	f.called = true
	f.gotToken = token
	f.gotInput = input
	return f.newRunID, f.err
}

// newResumeReg returns an in-memory registry holding one run, optionally
// suspended with the given token and JSON Schema.
func newResumeReg(t *testing.T) (*registry.Registry, db.DB) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return registry.New(d), d
}

func suspendRun(t *testing.T, reg *registry.Registry, runID, token string, schema []byte) {
	t.Helper()
	ctx := context.Background()
	if _, err := reg.StartRunWithID(ctx, runID, "task-x", "", string(registry.TriggerManual), registry.RunKindTask); err != nil {
		t.Fatalf("StartRunWithID: %v", err)
	}
	deadline := time.Now().Add(time.Hour).UnixMilli()
	if _, err := reg.SuspendRun(ctx, runID, []byte(`{"step":1}`), schema, token, time.Now().UnixMilli(), deadline, nil); err != nil {
		t.Fatalf("SuspendRun: %v", err)
	}
}

const approveSchema = `{"type":"object","properties":{"approve":{"type":"string"}},"required":["approve"]}`

func TestResume_Success_CallsResumerWithParsedInput(t *testing.T) {
	reg, _ := newResumeReg(t)
	suspendRun(t, reg, "run-1", "tok-abc", []byte(approveSchema))

	fr := &fakeResumer{newRunID: "run-2"}
	cs := &ControlServer{reg: reg, resumer: fr, log: zap.NewNop()}

	res, err := cs.dispatch(context.Background(), Request{
		ID: "1", Method: "cli.resume", RunID: "run-1",
		Params: json.RawMessage(`{"approve":"yes"}`),
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	out, ok := res.(ResumeResult)
	if !ok {
		t.Fatalf("result type %T", res)
	}
	if out.RunID != "run-2" {
		t.Fatalf("RunID = %q, want run-2", out.RunID)
	}
	if !fr.called {
		t.Fatal("resumer was not called")
	}
	// The token must be resolved server-side from the run, never supplied by
	// the client.
	if fr.gotToken != "tok-abc" {
		t.Fatalf("token = %q, want tok-abc (resolved from run)", fr.gotToken)
	}
	if string(fr.gotInput) != `{"approve":"yes"}` {
		t.Fatalf("input = %s, want the parsed key=value JSON", fr.gotInput)
	}
}

func TestResume_EmptyParamsBecomesEmptyObject(t *testing.T) {
	reg, _ := newResumeReg(t)
	suspendRun(t, reg, "run-1", "tok-abc", nil)

	fr := &fakeResumer{newRunID: "run-2"}
	cs := &ControlServer{reg: reg, resumer: fr, log: zap.NewNop()}

	if _, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.resume", RunID: "run-1"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if string(fr.gotInput) != `{}` {
		t.Fatalf("input = %s, want {}", fr.gotInput)
	}
}

func TestResume_InvalidInputRejectedBeforeResumer(t *testing.T) {
	reg, _ := newResumeReg(t)
	suspendRun(t, reg, "run-1", "tok-abc", []byte(approveSchema))

	fr := &fakeResumer{newRunID: "run-2"}
	cs := &ControlServer{reg: reg, resumer: fr, log: zap.NewNop()}

	// Missing the required "approve" property → schema validation must reject.
	_, err := cs.dispatch(context.Background(), Request{
		ID: "1", Method: "cli.resume", RunID: "run-1",
		Params: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("expected validation error for missing required field")
	}
	if !strings.Contains(err.Error(), "approve") {
		t.Fatalf("error should name the missing field; got %v", err)
	}
	if fr.called {
		t.Fatal("resumer must not be called when validation fails")
	}
}

func TestResumeGet_ReturnsSchema(t *testing.T) {
	reg, _ := newResumeReg(t)
	suspendRun(t, reg, "run-1", "tok-abc", []byte(approveSchema))

	cs := &ControlServer{reg: reg, log: zap.NewNop()}
	res, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.resume.get", RunID: "run-1"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	info, ok := res.(ResumeInfo)
	if !ok {
		t.Fatalf("result type %T", res)
	}
	if info.RunID != "run-1" || info.TaskID != "task-x" {
		t.Fatalf("info = %+v", info)
	}
	if !strings.Contains(string(info.Schema), "approve") {
		t.Fatalf("schema = %s, want it to carry the approve property", info.Schema)
	}
}

func TestResume_NotSuspended(t *testing.T) {
	reg, _ := newResumeReg(t)
	// A plain (non-suspended) run.
	if _, err := reg.StartRunWithID(context.Background(), "run-1", "task-x", "", string(registry.TriggerManual), registry.RunKindTask); err != nil {
		t.Fatalf("StartRunWithID: %v", err)
	}
	fr := &fakeResumer{}
	cs := &ControlServer{reg: reg, resumer: fr, log: zap.NewNop()}

	_, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.resume", RunID: "run-1"})
	if err == nil {
		t.Fatal("expected error for a non-suspended run")
	}
	if fr.called {
		t.Fatal("resumer must not be called for a non-suspended run")
	}
}

func TestResume_EngineErrorsMapToMessages(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"not found", ErrResumeTokenNotFound, "resume token not found"},
		{"already resumed", ErrResumeNotSuspended, "already been resumed"},
		{"expired", ErrResumeExpired, "expired"},
		{"pending", ErrResumePending, "awaiting approval"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, _ := newResumeReg(t)
			suspendRun(t, reg, "run-1", "tok-abc", nil)
			fr := &fakeResumer{err: tc.err}
			cs := &ControlServer{reg: reg, resumer: fr, log: zap.NewNop()}

			_, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.resume", RunID: "run-1"})
			if err == nil {
				t.Fatalf("expected error for %v", tc.err)
			}
			if got := err.Error(); !strings.Contains(got, tc.want) {
				t.Fatalf("message = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

func TestResume_MissingRunID(t *testing.T) {
	fr := &fakeResumer{}
	cs := &ControlServer{resumer: fr, log: zap.NewNop()}
	if _, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.resume"}); err == nil {
		t.Fatal("expected error without runID")
	}
	if fr.called {
		t.Fatal("resumer must not be called without a run id")
	}
}

func TestResume_NotConfigured(t *testing.T) {
	cs := &ControlServer{log: zap.NewNop()}
	if _, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.resume", RunID: "run-1"}); err == nil {
		t.Fatal("expected error when resumer is not wired")
	}
}

func TestResumeList_ReportsSuspendedRunsWithFields(t *testing.T) {
	reg, _ := newResumeReg(t)
	suspendRun(t, reg, "run-1", "tok-1", []byte(`{"type":"object","properties":{"approve":{"type":"string"},"note":{"type":"string"}},"required":["approve","note"]}`))

	cs := &ControlServer{reg: reg, log: zap.NewNop()}
	res, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.resume.list"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	list, ok := res.([]SuspendedRunSummary)
	if !ok {
		t.Fatalf("result type %T", res)
	}
	if len(list) != 1 {
		t.Fatalf("len = %d, want 1", len(list))
	}
	got := list[0]
	if got.RunID != "run-1" || got.TaskID != "task-x" {
		t.Fatalf("summary = %+v", got)
	}
	if len(got.Fields) != 2 || got.Fields[0] != "approve" || got.Fields[1] != "note" {
		t.Fatalf("fields = %v, want [approve note]", got.Fields)
	}
	if got.SuspendedAt == "" {
		t.Fatal("SuspendedAt should be populated")
	}
}

func TestResumeList_Empty(t *testing.T) {
	reg, _ := newResumeReg(t)
	cs := &ControlServer{reg: reg, log: zap.NewNop()}
	res, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.resume.list"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	list, ok := res.([]SuspendedRunSummary)
	if !ok {
		t.Fatalf("result type %T", res)
	}
	if len(list) != 0 {
		t.Fatalf("len = %d, want 0", len(list))
	}
}
