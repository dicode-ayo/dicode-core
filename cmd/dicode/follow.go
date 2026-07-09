package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/schemavalidate"
	"golang.org/x/term"
)

// stdinIsInteractive reports whether stdin is a terminal. Interactive resume
// prompting engages only when true, so piped/redirected input keeps the
// scriptable one-shot behavior.
func stdinIsInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// parseInteractiveFlag pulls the --non-interactive opt-out (alias --batch) out
// of a subcommand's argument list, returning whether it was present and the
// remaining positional args (task/run id and field=value pairs) in order.
func parseInteractiveFlag(args []string) (nonInteractive bool, rest []string) {
	for _, a := range args {
		switch a {
		case "--non-interactive", "--batch":
			nonInteractive = true
		default:
			rest = append(rest, a)
		}
	}
	return nonInteractive, rest
}

// resumeFieldNames returns the property names to hint at for a non-interactive
// resume: the required set if the schema declares one, otherwise every declared
// property, both in declaration order.
func resumeFieldNames(entries []resumePropEntry, required map[string]bool) []string {
	var req, all []string
	for _, e := range entries {
		all = append(all, e.Name)
		if required[e.Name] {
			req = append(req, e.Name)
		}
	}
	if len(req) > 0 {
		return req
	}
	return all
}

// followEngages decides whether a suspended run drives the interactive wizard.
// It requires a TTY, no --non-interactive opt-out, and no inline field=value
// values for the step; otherwise the caller keeps the scriptable one-shot path.
// --non-interactive forces one-shot regardless of the TTY, so agents/CI running
// inside an allocated PTY never block on a prompt.
func followEngages(nonInteractive, interactive, haveInlineValues bool) bool {
	return interactive && !nonInteractive && !haveInlineValues
}

// followClient is the daemon surface the interactive follow loop needs. The
// production impl wraps *ipc.ControlClient; tests inject a fake to exercise the
// schema→prompt→submit→follow core without a socket.
type followClient interface {
	// ResumeGet returns the JSON Schema of the form a suspended run awaits.
	ResumeGet(runID string) (ipc.ResumeInfo, error)
	// Resume submits the collected input and returns the continuation run id.
	Resume(runID string, input []byte) (ipc.ResumeResult, error)
	// WaitRun blocks until runID reaches a terminal or suspended state.
	WaitRun(runID string) (ipc.RunResult, error)
	// Logs returns the accumulated log lines of a run.
	Logs(runID string) ([]ipc.LogEntry, error)
}

// controlFollowClient adapts *ipc.ControlClient to followClient, unwrapping the
// response envelope's Error field into a Go error at each call.
type controlFollowClient struct{ c *ipc.ControlClient }

func (f controlFollowClient) ResumeGet(runID string) (ipc.ResumeInfo, error) {
	resp, err := f.c.Send(ipc.Request{Method: "cli.resume.get", RunID: runID})
	if err != nil {
		return ipc.ResumeInfo{}, err
	}
	if resp.Error != "" {
		return ipc.ResumeInfo{}, fmt.Errorf("%s", resp.Error)
	}
	var info ipc.ResumeInfo
	if err := remarshal(resp.Result, &info); err != nil {
		return ipc.ResumeInfo{}, err
	}
	return info, nil
}

func (f controlFollowClient) Resume(runID string, input []byte) (ipc.ResumeResult, error) {
	resp, err := f.c.Send(ipc.Request{Method: "cli.resume", RunID: runID, Params: input})
	if err != nil {
		return ipc.ResumeResult{}, err
	}
	if resp.Error != "" {
		return ipc.ResumeResult{}, fmt.Errorf("%s", resp.Error)
	}
	var res ipc.ResumeResult
	if err := remarshal(resp.Result, &res); err != nil {
		return ipc.ResumeResult{}, err
	}
	return res, nil
}

func (f controlFollowClient) WaitRun(runID string) (ipc.RunResult, error) {
	resp, err := f.c.Send(ipc.Request{Method: "cli.run.wait", RunID: runID})
	if err != nil {
		return ipc.RunResult{}, err
	}
	if resp.Error != "" {
		return ipc.RunResult{}, fmt.Errorf("%s", resp.Error)
	}
	var res ipc.RunResult
	if err := remarshal(resp.Result, &res); err != nil {
		return ipc.RunResult{}, err
	}
	return res, nil
}

func (f controlFollowClient) Logs(runID string) ([]ipc.LogEntry, error) {
	resp, err := f.c.Send(ipc.Request{Method: "cli.logs", RunID: runID})
	if err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	var entries []ipc.LogEntry
	if err := remarshal(resp.Result, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// followSession carries the injectable I/O and daemon client for the wizard
// follow loop.
type followSession struct {
	client followClient
	in     io.Reader // answers (stdin)
	prompt io.Writer // prompts + step banners (stderr, keeps stdout clean)
	out    io.Writer // logs + final result (stdout)
}

// resumeSchemaMeta extracts a schema's top-level title and description, used to
// frame a bare-approval confirmation prompt.
func resumeSchemaMeta(schemaJSON []byte) (title, description string) {
	if len(bytes.TrimSpace(schemaJSON)) == 0 {
		return "", ""
	}
	var m struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(schemaJSON, &m)
	return m.Title, m.Description
}

// printOneShotSuspended prints the scriptable one-shot view of a still-suspended
// run: its id and the fields to supply. Used when interactive consent is not
// available (EOF / redirected stdin), so a step is never auto-resumed.
func (s *followSession) printOneShotSuspended(runID string, entries []resumePropEntry, required map[string]bool) {
	fmt.Fprintf(s.out, "run %s: suspended\n", runID)
	if fields := resumeFieldNames(entries, required); len(fields) > 0 {
		fmt.Fprintf(s.out, "fields: %s\n", strings.Join(fields, ", "))
	}
	fmt.Fprintf(s.out, "resume with: dicode resume %s <field=value ...>\n", runID)
}

// collectStepInput gathers the resume input for one wizard step. Its three
// outcomes: submit==true carries the JSON to resume with; submit==false with a
// nil error means consent was unavailable (EOF at a confirmation gate) and the
// caller should fall back to the one-shot view; a non-nil error aborts.
//
// A schema with no promptable properties is a bare-approval form. Auto-
// submitting {} there would silently resume the run the instant it is seen — and
// loop forever if the continuation suspends the same way — so an explicit
// confirmation (a single Enter) is required; EOF is not consent. When {} cannot
// even satisfy the schema (a required field absent from properties, minProperties,
// an unsatisfiable allOf), no prompt could ever fix it, so it aborts rather than
// spins. For promptable schemas, an object-level validation failure re-prompts,
// but only while a re-prompt can change the input: identical input failing twice
// (e.g. optional fields left blank at EOF) aborts instead of hot-looping.
func (s *followSession) collectStepInput(reader *bufio.Reader, schema []byte, entries []resumePropEntry, required map[string]bool) ([]byte, bool, error) {
	if len(entries) == 0 {
		empty := []byte("{}")
		if verr := schemavalidate.Validate(schema, empty); verr != nil {
			return nil, false, fmt.Errorf("cannot resume: %w", verr)
		}
		if title, desc := resumeSchemaMeta(schema); title != "" || desc != "" {
			if title != "" {
				fmt.Fprintln(s.prompt, title)
			}
			if desc != "" {
				fmt.Fprintln(s.prompt, desc)
			}
		}
		fmt.Fprint(s.prompt, "Approve and continue? [Enter to confirm, Ctrl+C to abort]: ")
		_, rerr := reader.ReadString('\n')
		if errors.Is(rerr, io.EOF) {
			return nil, false, nil
		}
		if rerr != nil {
			return nil, false, rerr
		}
		return empty, true, nil
	}

	var last []byte
	for {
		values, perr := promptResumeInput(entries, required, reader, s.prompt)
		if perr != nil {
			return nil, false, perr
		}
		inputJSON, err := json.Marshal(values)
		if err != nil {
			return nil, false, err
		}
		if verr := schemavalidate.Validate(schema, inputJSON); verr != nil {
			if last != nil && bytes.Equal(last, inputJSON) {
				return nil, false, fmt.Errorf("cannot resume: %w", verr)
			}
			last = inputJSON
			fmt.Fprintf(s.prompt, "  %v\n", verr)
			continue
		}
		return inputJSON, true, nil
	}
}

// follow walks a suspend wizard from an already-suspended run: it fetches the
// run's resume form, collects input for it, submits, then waits on the
// continuation and prints its logs. If the continuation suspends again it
// repeats; when it reaches a terminal state it prints the final status and
// return value. A daemon task's continuation never settles, so it is resumed
// one-shot (print the continuation id) rather than followed.
func (s *followSession) follow(suspendedRunID string) error {
	// One reader across every step: a per-step bufio.Reader would read-ahead and
	// drop the next step's buffered answers. promptResumeInput re-wraps this with
	// bufio.NewReader, which returns an existing *bufio.Reader unchanged.
	reader := bufio.NewReader(s.in)
	runID := suspendedRunID
	for {
		info, err := s.client.ResumeGet(runID)
		if err != nil {
			return err
		}
		entries, required, err := parseResumeProps(info.Schema)
		if err != nil {
			return fmt.Errorf("read resume schema: %w", err)
		}

		inputJSON, submit, err := s.collectStepInput(reader, info.Schema, entries, required)
		if err != nil {
			return err
		}
		if !submit {
			// No interactive consent (EOF at a confirmation gate): never auto-
			// resume — leave the run suspended and show the one-shot view.
			s.printOneShotSuspended(runID, entries, required)
			return nil
		}

		res, err := s.client.Resume(runID, inputJSON)
		if err != nil {
			return err
		}
		contID := res.RunID

		// A daemon continuation is long-lived and never settles; blocking on it
		// would hang the CLI. Resume took effect — surface the continuation id
		// one-shot and stop, matching the non-interactive resume output.
		if info.Daemon {
			fmt.Fprintf(s.out, "resumed: continuation run %s\n", contID)
			fmt.Fprintf(s.out, "follow: dicode logs %s\n", contID)
			return nil
		}

		fmt.Fprintf(s.prompt, "resumed: following continuation run %s\n", contID)

		result, err := s.client.WaitRun(contID)
		if err != nil {
			return err
		}
		s.printLogs(contID)

		if result.Status == registry.StatusSuspended {
			fmt.Fprintf(s.prompt, "\nrun %s suspended again — next step:\n", contID)
			runID = contID
			continue
		}

		fmt.Fprintf(s.out, "run %s: %s\n", contID, result.Status)
		if result.ReturnValue != nil {
			out, _ := json.MarshalIndent(result.ReturnValue, "", "  ")
			fmt.Fprintln(s.out, string(out))
		}
		if result.Status == registry.StatusFailure {
			return fmt.Errorf("task failed")
		}
		return nil
	}
}

// printLogs fetches and prints a run's log lines, matching cmdLogs's format. A
// fetch error is noted on the prompt stream but does not abort the follow.
func (s *followSession) printLogs(runID string) {
	entries, err := s.client.Logs(runID)
	if err != nil {
		fmt.Fprintf(s.prompt, "dicode: fetch logs: %v\n", err)
		return
	}
	for _, e := range entries {
		fmt.Fprintf(s.out, "%s [%s] %s\n", e.Timestamp, e.Level, e.Message)
	}
}

// followSuspended drives the interactive wizard against a live daemon. Logs of
// steps that have already settled are on stdout; a step still in flight when
// SIGINT arrives has not printed its logs, and the handler deliberately issues
// no control request to fetch them — the main goroutine is usually parked in
// WaitRun on the same connection, and ControlClient.Send is not safe for
// concurrent use. The handler notes the interrupt and exits 130 (the shell
// convention); the daemon cancels the in-flight run when the connection drops.
func followSuspended(c *ipc.ControlClient, runID string) error {
	s := &followSession{
		client: controlFollowClient{c: c},
		in:     os.Stdin,
		prompt: os.Stderr,
		out:    os.Stdout,
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() {
		if _, ok := <-sigCh; !ok {
			return
		}
		fmt.Fprintln(s.prompt, "\ninterrupted")
		os.Exit(130)
	}()
	return s.follow(runID)
}
