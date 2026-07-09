package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"

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

// follow walks a suspend wizard from an already-suspended run: it fetches the
// run's resume form, prompts for it, submits, then waits on the continuation
// and prints its logs. If the continuation suspends again it repeats; when it
// reaches a terminal state it prints the final status and return value.
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

		// Re-prompt the whole step on object-level schema failure rather than
		// aborting the flow. Per-field coercion already rejects bad types, so
		// this catches cross-field constraints the field loop can't see.
		var inputJSON []byte
		for {
			values, perr := promptResumeInput(entries, required, reader, s.prompt)
			if perr != nil {
				return perr
			}
			inputJSON, err = json.Marshal(values)
			if err != nil {
				return err
			}
			if verr := schemavalidate.Validate(info.Schema, inputJSON); verr != nil {
				fmt.Fprintf(s.prompt, "  %v\n", verr)
				continue
			}
			break
		}

		res, err := s.client.Resume(runID, inputJSON)
		if err != nil {
			return err
		}
		contID := res.RunID
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

// followSuspended drives the interactive wizard against a live daemon. Each
// step's logs stream to stdout as it settles, so on SIGINT the accumulated
// output is already printed; the handler only notes the interrupt and exits 130
// (the shell convention). It deliberately issues no further control request —
// the main goroutine is usually parked in WaitRun on the same connection, and
// ControlClient.Send is not safe for concurrent use.
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
