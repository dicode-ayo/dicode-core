package tasktest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dicode/dicode/pkg/deno"
	"github.com/dicode/dicode/pkg/task"
)

// stubLookup is a one-entry SpecLookup standing in for the registry.
type stubLookup map[string]*task.Spec

func (s stubLookup) Get(id string) (*task.Spec, bool) {
	spec, ok := s[id]
	return spec, ok
}

// fixtureSpec writes testSrc as the task.test.ts of a fresh temp dir and
// returns a Deno spec over it declaring params.
func fixtureSpec(t *testing.T, id string, params task.Params, testSrc string) *task.Spec {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "task.test.ts"), []byte(testSrc), 0644); err != nil {
		t.Fatal(err)
	}
	return &task.Spec{ID: id, TaskDir: dir, Runtime: task.RuntimeDeno, Params: params}
}

// requiredParam is the shape a test run must tolerate: required, no default,
// and no way for the caller to supply it.
var requiredParam = task.Params{{Name: "content", Type: "string", Required: true}}

// TestRunByID_RequiredParamUnsupplied asserts a task declaring a required
// param with no default is testable: the test file mocks its own params and
// the validated map never reaches the runner.
func TestRunByID_RequiredParamUnsupplied(t *testing.T) {
	if testing.Short() {
		t.Skip("requires deno subprocess")
	}
	if _, err := deno.EnsureDeno(deno.DefaultVersion); err != nil {
		t.Skipf("deno provisioning failed (offline?): %v", err)
	}

	spec := fixtureSpec(t, "examples/required-param-fixture", requiredParam, `Deno.test("mocks its own params", () => {
  const params = { content: "mocked" };
  if (params.content !== "mocked") throw new Error("unreachable");
});
`)
	reg := stubLookup{spec.ID: spec}

	res, _, err := RunByID(context.Background(), reg, spec.ID, nil, 0)
	if err != nil {
		t.Fatalf("RunByID: %v\noutput:\n%s", err, res.Output)
	}
	if res.Passed != 1 || res.Failed != 0 {
		t.Errorf("Passed=%d Failed=%d; want 1/0\noutput:\n%s", res.Passed, res.Failed, res.Output)
	}
}

// TestRunByID_UnknownParamRejected pins the closed schema: junk a caller did
// supply is an error, raised before any subprocess starts.
func TestRunByID_UnknownParamRejected(t *testing.T) {
	spec := fixtureSpec(t, "examples/required-param-fixture", requiredParam, "")
	reg := stubLookup{spec.ID: spec}

	_, _, err := RunByID(context.Background(), reg, spec.ID, map[string]any{"typo": "x"}, 0)
	var perr *ErrParamsInvalid
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v (%T), want *ErrParamsInvalid", err, err)
	}
	if len(perr.FieldErrors) != 1 || perr.FieldErrors[0].Field != "typo" {
		t.Errorf("FieldErrors = %+v, want a single error on \"typo\"", perr.FieldErrors)
	}
}

// TestRunByID_TypeMismatchRejected pins the other rule RunByID still applies:
// a supplied value that doesn't coerce to its declared type.
func TestRunByID_TypeMismatchRejected(t *testing.T) {
	spec := fixtureSpec(t, "examples/typed-param-fixture", task.Params{{Name: "limit", Type: "number"}}, "")
	reg := stubLookup{spec.ID: spec}

	_, _, err := RunByID(context.Background(), reg, spec.ID, map[string]any{"limit": "not-a-number"}, 0)
	var perr *ErrParamsInvalid
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v (%T), want *ErrParamsInvalid", err, err)
	}
	if len(perr.FieldErrors) != 1 || perr.FieldErrors[0].Field != "limit" {
		t.Errorf("FieldErrors = %+v, want a single error on \"limit\"", perr.FieldErrors)
	}
}
