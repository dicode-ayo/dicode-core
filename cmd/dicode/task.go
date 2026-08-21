package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/dicode/dicode/pkg/ipc"
)

// Output convention mirrors `dicode ai`: stdout carries the single piped
// value (task id or PR url), stderr carries metadata (session id, webui URL,
// progress). Pipelines can consume stdout cleanly while the operator still
// sees the session details on the terminal.

// cmdTaskCreate scaffolds a new task. With --ai it chains straight into an
// edit session in one round-trip.
//
//	dicode task create <name> [--source <name>] [--ai "<prompt>"]
func cmdTaskCreate(c *ipc.ControlClient, args []string) error {
	var source, prompt string
	var positional []string
	parseFlags := true
	for i := 0; i < len(args); i++ {
		a := args[i]
		if parseFlags && a == "--" {
			parseFlags = false
			continue
		}
		if !parseFlags {
			positional = append(positional, a)
			continue
		}
		switch {
		case a == "--source":
			if i+1 >= len(args) {
				return fmt.Errorf("--source requires a value")
			}
			source = args[i+1]
			i++
		case a == "--ai":
			if i+1 >= len(args) {
				return fmt.Errorf("--ai requires a value")
			}
			prompt = args[i+1]
			i++
		case a == "--help" || a == "-h":
			fmt.Fprintln(os.Stderr, `Usage: dicode task create <name> [--source NAME] [--ai "PROMPT"]`)
			return nil
		case strings.HasPrefix(a, "--source="):
			source = strings.TrimPrefix(a, "--source=")
		case strings.HasPrefix(a, "--ai="):
			prompt = strings.TrimPrefix(a, "--ai=")
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) == 0 {
		return fmt.Errorf(`usage: dicode task create <name> [--source NAME] [--ai "PROMPT"]`)
	}
	name := positional[0]

	resp, err := c.Send(ipc.Request{
		Method:   "cli.task.create",
		TaskName: name,
		Source:   source,
		Prompt:   prompt,
	})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	var res ipc.TaskCreateResult
	if err := remarshal(resp.Result, &res); err != nil {
		return err
	}

	// On the --ai path, the session id + webui url are metadata (stderr) and
	// the reply is the conversational output (stderr too, so stdout stays the
	// piped task id). Plain create just prints the task id.
	if res.SessionID != "" {
		fmt.Fprintf(os.Stderr, "session: %s\n", res.SessionID)
	}
	if res.WebUIURL != "" {
		fmt.Fprintf(os.Stderr, "open: %s\n", res.WebUIURL)
	}
	// A suspended AI turn hasn't actually finished — don't print an empty
	// Reply (there won't be one) framed as if the turn completed. The
	// scaffolded file did land, though, so the task id still goes to
	// stdout below; only the "success" framing on stderr changes (#568
	// finding 3). Unreachable via the shipped buildin/task-create (which
	// only ever uses the non-suspending oneShotTurn) but the underlying
	// ai-agent task is generic, so a future/custom override that suspends
	// must still surface cleanly here instead of printing nothing.
	if res.Suspended {
		fmt.Fprintf(os.Stderr, "run %s suspended awaiting further input — run `dicode resume %s` to continue\n", res.RunID, res.RunID)
	} else if res.Reply != "" {
		fmt.Fprintln(os.Stderr, res.Reply)
	}
	printAuthoringPostCondition(res.FilesChanged)
	// The task id goes to stdout either way — the scaffold landed and the
	// task is registered, so a pipeline still needs it — but a turn that
	// wrote nothing must not exit 0 behind a reply claiming otherwise (#755).
	fmt.Println(res.TaskID)
	if res.WroteNothing {
		return wroteNothingError(res.TaskID, res.RunID)
	}
	return nil
}

// cmdTaskEdit opens or resumes an AI edit session.
//
//	dicode task edit <task-id> "<prompt>" [--session <id>]
func cmdTaskEdit(c *ipc.ControlClient, args []string) error {
	var sessionID string
	var positional []string
	parseFlags := true
	for i := 0; i < len(args); i++ {
		a := args[i]
		if parseFlags && a == "--" {
			parseFlags = false
			continue
		}
		if !parseFlags {
			positional = append(positional, a)
			continue
		}
		switch {
		case a == "--session":
			if i+1 >= len(args) {
				return fmt.Errorf("--session requires a value")
			}
			sessionID = args[i+1]
			i++
		case a == "--help" || a == "-h":
			fmt.Fprintln(os.Stderr, `Usage: dicode task edit <task-id> "<prompt>" [--session ID]`)
			return nil
		case strings.HasPrefix(a, "--session="):
			sessionID = strings.TrimPrefix(a, "--session=")
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) == 0 {
		return fmt.Errorf(`usage: dicode task edit <task-id> "<prompt>" [--session ID]`)
	}
	taskID := positional[0]
	prompt := strings.Join(positional[1:], " ")

	resp, err := c.Send(ipc.Request{
		Method:    "cli.task.edit",
		TaskID:    taskID,
		Prompt:    prompt,
		SessionID: sessionID,
	})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	var res ipc.TaskEditResult
	if err := remarshal(resp.Result, &res); err != nil {
		return err
	}

	if res.SessionID != "" {
		fmt.Fprintf(os.Stderr, "session: %s\n", res.SessionID)
	}
	if res.WebUIURL != "" {
		fmt.Fprintf(os.Stderr, "open: %s\n", res.WebUIURL)
	}
	// A suspended AI turn hasn't actually finished — surface that clearly on
	// stderr with a resume hint rather than falling through to print an
	// empty Reply as if the turn succeeded (#568 finding 3). Unreachable via
	// the shipped buildin/task-create (which only ever uses the
	// non-suspending oneShotTurn) but the underlying ai-agent task is
	// generic, so a future/custom override that suspends must still surface
	// cleanly instead of hanging or silently succeeding empty. This isn't
	// the full interactive chat loop (`followSuspended`) — task edit isn't
	// an interactive chat UX — just a clear informational message + hint.
	if res.Suspended {
		fmt.Fprintf(os.Stderr, "run %s suspended awaiting further input — run `dicode resume %s` to continue\n", res.RunID, res.RunID)
		return nil
	}
	// Reply is the piped (stdout) value, filled whenever <prompt> fired a real
	// AI turn (#568). Only print when present: a blank prompt is a plain
	// (non-AI) edit that opens/resumes the session with no turn to reply.
	if res.Reply != "" {
		fmt.Println(res.Reply)
	}
	printAuthoringPostCondition(res.FilesChanged)
	if res.WroteNothing {
		return wroteNothingError(res.TaskID, res.RunID)
	}
	return nil
}

// printAuthoringPostCondition reports what the AI turn actually changed on
// disk (#755). Metadata, so stderr — stdout stays the reply. Silent when the
// post-condition wasn't evaluated (no turn fired, the run suspended, or the
// task directory couldn't be snapshotted).
func printAuthoringPostCondition(files []string) {
	if len(files) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "wrote: %s\n", strings.Join(files, ", "))
}

// wroteNothingError is the non-zero exit for a turn that completed while
// leaving the task directory untouched (#755). The reply has already been
// printed: it is the agent's own account of its work, and this is the only
// thing in the chain that checked that account against disk.
func wroteNothingError(taskID, runID string) error {
	return fmt.Errorf("the AI turn changed no files in %s — nothing was written to disk, whatever the reply above says (run %s)", taskID, runID)
}

// cmdTaskSave applies a session and closes it.
//
//	dicode task save <session-id>
func cmdTaskSave(c *ipc.ControlClient, args []string) error {
	sessionID, err := singleSessionArg(args, "save")
	if err != nil || sessionID == "" {
		return err
	}

	resp, err := c.Send(ipc.Request{Method: "cli.task.save", SessionID: sessionID})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	var res ipc.TaskSaveResult
	if err := remarshal(resp.Result, &res); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "session: %s\n", sessionID)
	// stdout = the piped value: PR url for git sources, task id otherwise.
	switch {
	case res.PRURL != "":
		fmt.Println(res.PRURL)
	case res.TaskID != "":
		fmt.Println(res.TaskID)
	default:
		fmt.Println(sessionID)
	}
	return nil
}

// cmdTaskCancel discards a session.
//
//	dicode task cancel <session-id>
func cmdTaskCancel(c *ipc.ControlClient, args []string) error {
	sessionID, err := singleSessionArg(args, "cancel")
	if err != nil || sessionID == "" {
		return err
	}

	resp, err := c.Send(ipc.Request{Method: "cli.task.cancel", SessionID: sessionID})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}

	fmt.Fprintf(os.Stderr, "session: %s\n", sessionID)
	fmt.Printf("cancelled %s\n", sessionID)
	return nil
}

// singleSessionArg parses a single positional <session-id> with `--` sentinel
// support, shared by save and cancel.
func singleSessionArg(args []string, verb string) (string, error) {
	var positional []string
	parseFlags := true
	for _, a := range args {
		if parseFlags && a == "--" {
			parseFlags = false
			continue
		}
		if parseFlags && (a == "--help" || a == "-h") {
			fmt.Fprintf(os.Stderr, "Usage: dicode task %s <session-id>\n", verb)
			return "", nil
		}
		positional = append(positional, a)
	}
	if len(positional) == 0 {
		return "", fmt.Errorf("usage: dicode task %s <session-id>", verb)
	}
	return positional[0], nil
}
