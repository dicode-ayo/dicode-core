package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/ipc"
)

// fakeFollowClient scripts the daemon responses the follow loop consumes. Each
// suspended run id maps to the schema it awaits; resume returns the next id from
// resumeChain; waitResults maps a continuation id to its settled RunResult.
type fakeFollowClient struct {
	schemas     map[string][]byte          // runID -> resume JSON Schema
	daemon      map[string]bool            // runID -> task is a daemon body
	resumeChain map[string]string          // suspended runID -> continuation runID
	waitResults map[string]ipc.RunResult   // continuation runID -> settled result
	logs        map[string][]ipc.LogEntry  // runID -> log lines
	submitted   map[string]json.RawMessage // suspended runID -> input submitted
	waitCalls   int                        // number of WaitRun invocations
	cancelled   []string                   // runIDs passed to Cancel, in order
}

func (f *fakeFollowClient) ResumeGet(runID string) (ipc.ResumeInfo, error) {
	return ipc.ResumeInfo{RunID: runID, Schema: f.schemas[runID], Daemon: f.daemon[runID]}, nil
}

func (f *fakeFollowClient) Resume(runID string, input []byte) (ipc.ResumeResult, error) {
	if f.submitted == nil {
		f.submitted = map[string]json.RawMessage{}
	}
	f.submitted[runID] = append(json.RawMessage(nil), input...)
	return ipc.ResumeResult{RunID: f.resumeChain[runID]}, nil
}

func (f *fakeFollowClient) WaitRun(runID string) (ipc.RunResult, error) {
	f.waitCalls++
	return f.waitResults[runID], nil
}

func (f *fakeFollowClient) Cancel(runID string) error {
	f.cancelled = append(f.cancelled, runID)
	return nil
}

func (f *fakeFollowClient) Logs(runID string) ([]ipc.LogEntry, error) {
	return f.logs[runID], nil
}

const oneStepSchema = `{"type":"object","properties":{"approve":{"type":"string","title":"Approve?"}},"required":["approve"]}`

func TestParseWizardFlags(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantNI     bool
		wantFields []string
		wantRes    []string
		wantErr    bool
	}{
		{name: "none", args: []string{"task-x", "a=1"}, wantRes: []string{"task-x", "a=1"}},
		{name: "non-interactive", args: []string{"task-x", "--non-interactive", "a=1"}, wantNI: true, wantRes: []string{"task-x", "a=1"}},
		{name: "batch alias", args: []string{"--batch", "run-1"}, wantNI: true, wantRes: []string{"run-1"}},
		{name: "flag only", args: []string{"--non-interactive"}, wantNI: true},
		{name: "field spaced", args: []string{"wiz", "--field", "a=1", "--field", "b=2"}, wantFields: []string{"a=1", "b=2"}, wantRes: []string{"wiz"}},
		{name: "field equals", args: []string{"wiz", "--field=a=1"}, wantFields: []string{"a=1"}, wantRes: []string{"wiz"}},
		{name: "field mixed with params and ni", args: []string{"wiz", "p=q", "--field", "a=1", "--batch"}, wantNI: true, wantFields: []string{"a=1"}, wantRes: []string{"wiz", "p=q"}},
		{name: "field missing arg", args: []string{"wiz", "--field"}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotNI, gotFields, gotRes, err := parseWizardFlags(c.args)
			if c.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotNI != c.wantNI {
				t.Errorf("nonInteractive = %v, want %v", gotNI, c.wantNI)
			}
			if strings.Join(gotFields, ",") != strings.Join(c.wantFields, ",") {
				t.Errorf("fields = %v, want %v", gotFields, c.wantFields)
			}
			if strings.Join(gotRes, ",") != strings.Join(c.wantRes, ",") {
				t.Errorf("rest = %v, want %v", gotRes, c.wantRes)
			}
		})
	}
}

func TestNewPrefillPool(t *testing.T) {
	if _, err := newPrefillPool([]string{"noeq"}); err == nil {
		t.Error("expected error for a field without '='")
	}
	if _, err := newPrefillPool([]string{"=v"}); err == nil {
		t.Error("expected error for an empty field name")
	}
	if _, err := newPrefillPool([]string{"a=1", "a=2"}); err == nil {
		t.Error("expected error for a duplicate field name")
	}
	p, err := newPrefillPool([]string{"a=1", "b=x=y"})
	if err != nil {
		t.Fatalf("newPrefillPool: %v", err)
	}
	// Only the first '=' splits, so the value may itself contain '='.
	if raw, ok := p.take("b"); !ok || raw != "x=y" {
		t.Errorf("take(b) = %q,%v, want x=y,true", raw, ok)
	}
	// A consumed value is not handed out twice.
	if _, ok := p.take("b"); ok {
		t.Error("take(b) a second time must report consumed")
	}
	if got := p.unused(); len(got) != 1 || got[0] != "a" {
		t.Errorf("unused = %v, want [a]", got)
	}
}

func TestPrefillPool_NilSafe(t *testing.T) {
	var p *prefillPool
	if !p.empty() {
		t.Error("nil pool must be empty")
	}
	if _, ok := p.take("x"); ok {
		t.Error("nil pool must yield no value")
	}
	if p.unused() != nil {
		t.Error("nil pool must have no unused fields")
	}
}

// Pre-supplied answers auto-advance every step of a wizard under
// --non-interactive with no stdin, coercing each value to its declared type.
func TestFollow_PrefillAutoAdvances(t *testing.T) {
	client := &fakeFollowClient{
		schemas: map[string][]byte{
			"run-1": []byte(`{"type":"object","properties":{"project_name":{"type":"string"}},"required":["project_name"]}`),
			"run-2": []byte(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]}`),
		},
		resumeChain: map[string]string{"run-1": "run-2", "run-2": "run-3"},
		waitResults: map[string]ipc.RunResult{
			"run-2": {RunID: "run-2", Status: "suspended"},
			"run-3": {RunID: "run-3", Status: "success"},
		},
	}
	pool, _ := newPrefillPool([]string{"project_name=acme", "count=7"})
	var out, prompt bytes.Buffer
	s := &followSession{client: client, in: strings.NewReader(""), prompt: &prompt, out: &out, prefill: pool, nonInteractive: true}

	if err := s.follow("run-1"); err != nil {
		t.Fatalf("follow: %v", err)
	}
	if got := string(client.submitted["run-1"]); got != `{"project_name":"acme"}` {
		t.Errorf("step-1 submitted = %s", got)
	}
	// count coerced to a JSON number, not the raw string.
	if got := string(client.submitted["run-2"]); got != `{"count":7}` {
		t.Errorf("step-2 submitted = %s, want {\"count\":7}", got)
	}
	if !strings.Contains(out.String(), "run run-3: success") {
		t.Errorf("expected terminal success:\n%s", out.String())
	}
}

// A pre-supplied value is consumed at the first step that declares it; a later
// step sharing the field name does not inherit it.
func TestFollow_PrefillConsumedAtFirstMatch(t *testing.T) {
	shared := []byte(`{"type":"object","title":"Step","properties":{"value":{"type":"string"}},"required":["value"]}`)
	client := &fakeFollowClient{
		schemas:     map[string][]byte{"run-1": shared, "run-2": shared},
		resumeChain: map[string]string{"run-1": "run-2"},
		waitResults: map[string]ipc.RunResult{"run-2": {RunID: "run-2", Status: "suspended"}},
	}
	pool, _ := newPrefillPool([]string{"value=first"})
	var out, prompt bytes.Buffer
	s := &followSession{client: client, in: strings.NewReader(""), prompt: &prompt, out: &out, prefill: pool, nonInteractive: true}

	err := s.follow("run-1")
	if err == nil {
		t.Fatal("expected a missing-field error on the second step")
	}
	if got := string(client.submitted["run-1"]); got != `{"value":"first"}` {
		t.Errorf("step-1 submitted = %s, want the pre-supplied value", got)
	}
	if _, ok := client.submitted["run-2"]; ok {
		t.Error("step-2 must not inherit the first step's consumed value")
	}
	if !strings.Contains(err.Error(), "value") {
		t.Errorf("error should name the missing field: %v", err)
	}
}

// Non-interactive with an unfilled required field fails deterministically,
// naming the step and the field.
func TestFollow_PrefillNonInteractiveMissingErrors(t *testing.T) {
	client := &fakeFollowClient{
		schemas: map[string][]byte{
			"run-1": []byte(`{"type":"object","title":"New project","properties":{"project_name":{"type":"string"}},"required":["project_name"]}`),
		},
	}
	pool, _ := newPrefillPool([]string{"unrelated=x"})
	var out, prompt bytes.Buffer
	s := &followSession{client: client, in: strings.NewReader(""), prompt: &prompt, out: &out, prefill: pool, nonInteractive: true}

	err := s.follow("run-1")
	if err == nil {
		t.Fatal("expected a deterministic missing-field error")
	}
	if !strings.Contains(err.Error(), "project_name") || !strings.Contains(err.Error(), "New project") {
		t.Errorf("error should name the field and step: %v", err)
	}
}

// A bad type for a pre-supplied value fails before submission.
func TestFollow_PrefillTypeMismatchErrors(t *testing.T) {
	client := &fakeFollowClient{
		schemas: map[string][]byte{
			"run-1": []byte(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]}`),
		},
	}
	pool, _ := newPrefillPool([]string{"count=notanumber"})
	var out, prompt bytes.Buffer
	s := &followSession{client: client, in: strings.NewReader(""), prompt: &prompt, out: &out, prefill: pool, nonInteractive: true}

	if err := s.follow("run-1"); err == nil {
		t.Fatal("expected a coercion error for count=notanumber")
	}
	if _, ok := client.submitted["run-1"]; ok {
		t.Error("must not submit when a pre-supplied value fails coercion")
	}
}

// On a TTY, pre-supplied answers fill the steps they match and un-supplied
// steps still prompt.
func TestFollow_PrefillInteractiveFillsGaps(t *testing.T) {
	client := &fakeFollowClient{
		schemas: map[string][]byte{
			"run-1": []byte(`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`),
			"run-2": []byte(`{"type":"object","properties":{"b":{"type":"string"}},"required":["b"]}`),
		},
		resumeChain: map[string]string{"run-1": "run-2", "run-2": "run-3"},
		waitResults: map[string]ipc.RunResult{
			"run-2": {RunID: "run-2", Status: "suspended"},
			"run-3": {RunID: "run-3", Status: "success"},
		},
	}
	pool, _ := newPrefillPool([]string{"a=pre"})
	var out, prompt bytes.Buffer
	// a is pre-supplied (no prompt); b is typed at the prompt.
	s := &followSession{client: client, in: strings.NewReader("typed\n"), prompt: &prompt, out: &out, prefill: pool}

	if err := s.follow("run-1"); err != nil {
		t.Fatalf("follow: %v", err)
	}
	if got := string(client.submitted["run-1"]); got != `{"a":"pre"}` {
		t.Errorf("step-1 submitted = %s, want the pre-supplied value", got)
	}
	if got := string(client.submitted["run-2"]); got != `{"b":"typed"}` {
		t.Errorf("step-2 submitted = %s, want the prompted value", got)
	}
}

// On a TTY, pre-supplying a step's required field must not skip prompting for
// that step's optional fields.
func TestFollow_PrefillInteractiveStillPromptsOptional(t *testing.T) {
	client := &fakeFollowClient{
		schemas: map[string][]byte{
			"run-1": []byte(`{"type":"object","properties":{"a":{"type":"string"},"note":{"type":"string"}},"required":["a"]}`),
		},
		resumeChain: map[string]string{"run-1": "run-2"},
		waitResults: map[string]ipc.RunResult{"run-2": {RunID: "run-2", Status: "success"}},
	}
	pool, _ := newPrefillPool([]string{"a=pre"})
	var out, prompt bytes.Buffer
	// a is pre-supplied; the optional note is typed at the prompt.
	s := &followSession{client: client, in: strings.NewReader("hello\n"), prompt: &prompt, out: &out, prefill: pool}

	if err := s.follow("run-1"); err != nil {
		t.Fatalf("follow: %v", err)
	}
	got := string(client.submitted["run-1"])
	if !strings.Contains(got, `"a":"pre"`) || !strings.Contains(got, `"note":"hello"`) {
		t.Errorf("submitted = %s, want both the pre-supplied a and the prompted note", got)
	}
}

// Pre-supplied values never consumed (typo or a branch not taken) are reported
// after the wizard completes.
func TestFollow_PrefillUnusedWarned(t *testing.T) {
	client := &fakeFollowClient{
		schemas:     map[string][]byte{"run-1": []byte(`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`)},
		resumeChain: map[string]string{"run-1": "run-2"},
		waitResults: map[string]ipc.RunResult{"run-2": {RunID: "run-2", Status: "success"}},
	}
	pool, _ := newPrefillPool([]string{"a=x", "typo=y"})
	var out, prompt bytes.Buffer
	s := &followSession{client: client, in: strings.NewReader(""), prompt: &prompt, out: &out, prefill: pool, nonInteractive: true}

	if err := s.follow("run-1"); err != nil {
		t.Fatalf("follow: %v", err)
	}
	if !strings.Contains(prompt.String(), "typo") {
		t.Errorf("expected an unused-field warning naming typo:\n%s", prompt.String())
	}
}

func TestFollowEngages(t *testing.T) {
	cases := []struct {
		name                                    string
		nonInteractive, interactive, haveInline bool
		want                                    bool
	}{
		{"tty, opt-in, no inline", false, true, false, true},
		// --non-interactive forces one-shot even when the TTY check says interactive
		// (the agents/CI-on-a-PTY case the flag exists for).
		{"non-interactive overrides tty", true, true, false, false},
		{"not a tty", false, false, false, false},
		{"inline values force one-shot", false, true, true, false},
		{"non-interactive and inline", true, true, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := followEngages(c.nonInteractive, c.interactive, c.haveInline); got != c.want {
				t.Errorf("followEngages(%v,%v,%v) = %v, want %v",
					c.nonInteractive, c.interactive, c.haveInline, got, c.want)
			}
		})
	}
}

func TestFollow_SingleStepToSuccess(t *testing.T) {
	client := &fakeFollowClient{
		schemas:     map[string][]byte{"run-1": []byte(oneStepSchema)},
		resumeChain: map[string]string{"run-1": "run-2"},
		waitResults: map[string]ipc.RunResult{
			"run-2": {RunID: "run-2", Status: "success", ReturnValue: map[string]any{"ok": true}},
		},
		logs: map[string][]ipc.LogEntry{
			"run-2": {{Timestamp: "t0", Level: "info", Message: "done"}},
		},
	}
	var out, prompt bytes.Buffer
	s := &followSession{client: client, in: strings.NewReader("yes\n"), prompt: &prompt, out: &out}

	if err := s.follow("run-1"); err != nil {
		t.Fatalf("follow: %v", err)
	}

	// The typed answer was submitted for the suspended run.
	if got := string(client.submitted["run-1"]); got != `{"approve":"yes"}` {
		t.Errorf("submitted = %s, want {\"approve\":\"yes\"}", got)
	}
	stdout := out.String()
	if !strings.Contains(stdout, "run run-2: success") {
		t.Errorf("stdout missing final status:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"ok": true`) {
		t.Errorf("stdout missing return value:\n%s", stdout)
	}
	if !strings.Contains(stdout, "[info] done") {
		t.Errorf("stdout missing streamed logs:\n%s", stdout)
	}
}

func TestFollow_MultiStepWizard(t *testing.T) {
	client := &fakeFollowClient{
		schemas: map[string][]byte{
			"run-1": []byte(oneStepSchema),
			"run-2": []byte(`{"type":"object","properties":{"env":{"type":"string","enum":["staging","prod"]}},"required":["env"]}`),
		},
		resumeChain: map[string]string{"run-1": "run-2", "run-2": "run-3"},
		waitResults: map[string]ipc.RunResult{
			"run-2": {RunID: "run-2", Status: "suspended"},
			"run-3": {RunID: "run-3", Status: "success"},
		},
	}
	var out, prompt bytes.Buffer
	// First step: approve=go, second step: pick enum value prod.
	s := &followSession{client: client, in: strings.NewReader("go\nprod\n"), prompt: &prompt, out: &out}

	if err := s.follow("run-1"); err != nil {
		t.Fatalf("follow: %v", err)
	}
	if got := string(client.submitted["run-2"]); got != `{"env":"prod"}` {
		t.Errorf("step-2 submitted = %s, want {\"env\":\"prod\"}", got)
	}
	// The second step is reached (its enum answer was submitted above); the
	// interactive stream stays quiet between turns — the step's own banner
	// introduces it, no "suspended again" plumbing line.
	if strings.Contains(prompt.String(), "suspended again") {
		t.Errorf("did not expect the removed 'suspended again' plumbing line:\n%s", prompt.String())
	}
	if !strings.Contains(out.String(), "run run-3: success") {
		t.Errorf("expected terminal success on run-3:\n%s", out.String())
	}
}

func TestFollow_FailureReturnsError(t *testing.T) {
	client := &fakeFollowClient{
		schemas:     map[string][]byte{"run-1": []byte(oneStepSchema)},
		resumeChain: map[string]string{"run-1": "run-2"},
		waitResults: map[string]ipc.RunResult{"run-2": {RunID: "run-2", Status: "failure"}},
	}
	var out, prompt bytes.Buffer
	s := &followSession{client: client, in: strings.NewReader("yes\n"), prompt: &prompt, out: &out}

	if err := s.follow("run-1"); err == nil {
		t.Fatal("expected error when the continuation fails")
	}
	if !strings.Contains(out.String(), "run run-2: failure") {
		t.Errorf("stdout should still report the failing status:\n%s", out.String())
	}
}

func TestFollow_RepromptsUntilRequiredSupplied(t *testing.T) {
	client := &fakeFollowClient{
		schemas:     map[string][]byte{"run-1": []byte(oneStepSchema)},
		resumeChain: map[string]string{"run-1": "run-2"},
		waitResults: map[string]ipc.RunResult{"run-2": {RunID: "run-2", Status: "success"}},
	}
	var out, prompt bytes.Buffer
	// Blank line first (required -> reprompt), then a real value.
	s := &followSession{client: client, in: strings.NewReader("\nyes\n"), prompt: &prompt, out: &out}

	if err := s.follow("run-1"); err != nil {
		t.Fatalf("follow: %v", err)
	}
	if !strings.Contains(prompt.String(), "required — please enter a value") {
		t.Errorf("expected required-field reprompt:\n%s", prompt.String())
	}
	if got := string(client.submitted["run-1"]); got != `{"approve":"yes"}` {
		t.Errorf("submitted = %s, want the value entered after the reprompt", got)
	}
}

// A bare-approval schema (no properties) must NOT auto-submit; it requires an
// explicit Enter, after which {} is submitted.
func TestFollow_EmptyPropertiesRequiresConfirmation(t *testing.T) {
	const confirmSchema = `{"type":"object","title":"Confirm","description":"Proceed?"}`
	client := &fakeFollowClient{
		schemas:     map[string][]byte{"run-1": []byte(confirmSchema)},
		resumeChain: map[string]string{"run-1": "run-2"},
		waitResults: map[string]ipc.RunResult{"run-2": {RunID: "run-2", Status: "success"}},
	}
	var out, prompt bytes.Buffer
	// A single Enter confirms.
	s := &followSession{client: client, in: strings.NewReader("\n"), prompt: &prompt, out: &out}

	if err := s.follow("run-1"); err != nil {
		t.Fatalf("follow: %v", err)
	}
	if !strings.Contains(prompt.String(), "Approve and continue?") {
		t.Errorf("expected a confirmation prompt, got:\n%s", prompt.String())
	}
	if got := string(client.submitted["run-1"]); got != "{}" {
		t.Errorf("submitted = %q, want {} after explicit confirmation", got)
	}
}

// With no interactive consent (EOF / redirected stdin) a bare-approval schema
// must NOT auto-resume — no Resume call, one-shot suspended output instead.
func TestFollow_EmptyPropertiesEOFDoesNotAutoApprove(t *testing.T) {
	const confirmSchema = `{"type":"object","title":"Confirm"}`
	client := &fakeFollowClient{
		schemas:     map[string][]byte{"run-1": []byte(confirmSchema)},
		resumeChain: map[string]string{"run-1": "run-2"},
	}
	var out, prompt bytes.Buffer
	s := &followSession{client: client, in: strings.NewReader(""), prompt: &prompt, out: &out}

	if err := s.follow("run-1"); err != nil {
		t.Fatalf("follow: %v", err)
	}
	if _, ok := client.submitted["run-1"]; ok {
		t.Errorf("Resume must not be called without explicit consent; submitted=%v", client.submitted)
	}
	if !strings.Contains(out.String(), "run run-1: suspended") {
		t.Errorf("expected one-shot suspended output, got:\n%s", out.String())
	}
}

// An unsatisfiable schema (required field absent from properties → nothing to
// prompt, {} always invalid) must abort with an error, not hot-loop forever.
func TestFollow_UnsatisfiableSchemaAborts(t *testing.T) {
	const unsat = `{"type":"object","required":["x"]}`
	client := &fakeFollowClient{
		schemas: map[string][]byte{"run-1": []byte(unsat)},
	}
	var out, prompt bytes.Buffer
	s := &followSession{client: client, in: strings.NewReader(""), prompt: &prompt, out: &out}

	err := s.follow("run-1")
	if err == nil {
		t.Fatal("expected an error for an unsatisfiable schema, not a spin/submit")
	}
	if _, ok := client.submitted["run-1"]; ok {
		t.Errorf("must not submit for an unsatisfiable schema; submitted=%v", client.submitted)
	}
}

// Optional-only fields left blank at EOF marshal to the same {} twice; the
// second identical failure must abort rather than hot-loop.
func TestFollow_RepromptTerminatesOnIdenticalInput(t *testing.T) {
	const minProps = `{"type":"object","properties":{"note":{"type":"string"}},"minProperties":1}`
	client := &fakeFollowClient{
		schemas: map[string][]byte{"run-1": []byte(minProps)},
	}
	var out, prompt bytes.Buffer
	s := &followSession{client: client, in: strings.NewReader(""), prompt: &prompt, out: &out}

	if err := s.follow("run-1"); err == nil {
		t.Fatal("expected abort when the identical (empty) input fails twice")
	}
}

// A daemon task's continuation never settles, so it must be resumed one-shot
// (continuation id printed) without ever calling WaitRun.
func TestFollow_DaemonContinuationOneShot(t *testing.T) {
	client := &fakeFollowClient{
		schemas:     map[string][]byte{"run-1": []byte(oneStepSchema)},
		daemon:      map[string]bool{"run-1": true},
		resumeChain: map[string]string{"run-1": "run-2"},
	}
	var out, prompt bytes.Buffer
	s := &followSession{client: client, in: strings.NewReader("yes\n"), prompt: &prompt, out: &out}

	if err := s.follow("run-1"); err != nil {
		t.Fatalf("follow: %v", err)
	}
	if client.waitCalls != 0 {
		t.Errorf("WaitRun must not be called for a daemon continuation, got %d calls", client.waitCalls)
	}
	if !strings.Contains(out.String(), "resumed: continuation run run-2") {
		t.Errorf("expected one-shot continuation output, got:\n%s", out.String())
	}
}

// ── interactive-UX helpers: no-op off a TTY ─────────────────────────────────

// startSpinner(w, false) must never write to w — the gate for piped/CI
// output and every bytes.Buffer-backed test above.
func TestStartSpinner_NoopWhenInactive(t *testing.T) {
	var buf bytes.Buffer
	stop := startSpinner(&buf, false)
	stop()
	stop() // must tolerate a repeat call without panicking
	if buf.Len() != 0 {
		t.Fatalf("inactive spinner wrote %q, want nothing", buf.String())
	}
}

// An active spinner animates frames and leaves the line cleared (ending in a
// bare \r) once stopped, so whatever prints next starts clean.
func TestStartSpinner_ActiveAnimatesAndClearsOnStop(t *testing.T) {
	var buf bytes.Buffer
	stop := startSpinner(&buf, true)
	time.Sleep(3 * spinnerInterval)
	stop()
	stop() // idempotent
	got := buf.String()
	if !strings.Contains(got, "working") {
		t.Fatalf("active spinner wrote no frames: %q", got)
	}
	if !strings.HasSuffix(got, "\r") {
		t.Fatalf("stop() must leave the line cleared (trailing \\r), got %q", got)
	}
}

// flushStdin must not panic regardless of TTY state; under `go test` stdin is
// not a TTY, so this also exercises the no-op path directly.
func TestFlushStdin_NoopWhenStdinNotATTY(t *testing.T) {
	flushStdin()
}

// waitRunInterruptible must degrade to a plain WaitRun when neither stdin nor
// stderr is a TTY: no spinner bytes on prompt, no Cancel call, and the result
// passes through unchanged. This is the path every fakeFollowClient-backed
// test above runs (go test's stdin/stderr are not TTYs).
func TestWaitRunInterruptible_NonTTY_NoSpinnerNoCancel(t *testing.T) {
	client := &fakeFollowClient{
		waitResults: map[string]ipc.RunResult{"run-1": {RunID: "run-1", Status: "success"}},
	}
	var prompt bytes.Buffer
	s := &followSession{client: client, prompt: &prompt}

	res, err := s.waitRunInterruptible("run-1")
	if err != nil {
		t.Fatalf("waitRunInterruptible: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("status = %q, want success", res.Status)
	}
	if prompt.Len() != 0 {
		t.Fatalf("non-TTY prompt got spinner bytes: %q", prompt.String())
	}
	if len(client.cancelled) != 0 {
		t.Fatalf("Cancel must not be called absent a real interrupt, got %v", client.cancelled)
	}
}
