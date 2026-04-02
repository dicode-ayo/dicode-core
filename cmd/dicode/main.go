// dicode is the dicode CLI.
//
// It connects to a running dicoded daemon over the control socket and dispatches
// commands. If the daemon is not running it is started automatically.
//
// Usage:
//
//	dicode [flags] <command> [args...]
//
// Commands:
//
//	run <task-id> [key=value ...]   trigger a task run and wait for the result
//	list                            list all registered tasks
//	logs <run-id>                   fetch log lines for a run
//	status [task-id]                daemon health or latest run for a task
//	secrets list                    list secret keys
//	secrets set <key> <value>       store a secret
//	secrets delete <key>            delete a secret
//	version                         print version and exit
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dicode/dicode/pkg/ipc"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	if os.Args[1] == "version" {
		fmt.Printf("dicode %s\n", version)
		return
	}

	dataDir := defaultDataDir()
	socketPath := filepath.Join(dataDir, "daemon.sock")
	tokenPath := filepath.Join(dataDir, "daemon.token")

	if err := ensureDaemon(socketPath); err != nil {
		fmt.Fprintf(os.Stderr, "dicode: could not start daemon: %v\n", err)
		os.Exit(1)
	}

	c, err := ipc.Dial(socketPath, tokenPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dicode: connect to daemon: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()

	if err := dispatch(c, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "dicode: %v\n", err)
		os.Exit(1)
	}
}

func dispatch(c *ipc.ControlClient, args []string) error {
	switch args[0] {
	case "list":
		return cmdList(c)
	case "run":
		if len(args) < 2 {
			return fmt.Errorf("usage: dicode run <task-id> [key=value ...]")
		}
		return cmdRun(c, args[1], args[2:])
	case "logs":
		if len(args) < 2 {
			return fmt.Errorf("usage: dicode logs <run-id>")
		}
		return cmdLogs(c, args[1])
	case "status":
		taskID := ""
		if len(args) >= 2 {
			taskID = args[1]
		}
		return cmdStatus(c, taskID)
	case "secrets":
		if len(args) < 2 {
			return fmt.Errorf("usage: dicode secrets <list|set|delete> [args...]")
		}
		return cmdSecrets(c, args[1:])
	default:
		return fmt.Errorf("unknown command %q — run 'dicode' for usage", args[0])
	}
}

func cmdList(c *ipc.ControlClient) error {
	resp, err := c.Send(ipc.Request{Method: "cli.list"})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	var tasks []ipc.TaskSummary
	if err := remarshal(resp.Result, &tasks); err != nil {
		return err
	}
	if len(tasks) == 0 {
		fmt.Println("no tasks registered")
		return nil
	}
	fmt.Printf("%-30s %-12s %-10s %s\n", "ID", "TRIGGER", "LAST STATUS", "NAME")
	for _, t := range tasks {
		fmt.Printf("%-30s %-12s %-10s %s\n", t.ID, t.Trigger, orDash(t.LastStatus), t.Name)
	}
	return nil
}

func cmdRun(c *ipc.ControlClient, taskID string, kvArgs []string) error {
	params := map[string]string{}
	for _, kv := range kvArgs {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid param %q — expected key=value", kv)
		}
		params[parts[0]] = parts[1]
	}
	paramsJSON, _ := json.Marshal(params)
	resp, err := c.Send(ipc.Request{
		Method: "cli.run",
		TaskID: taskID,
		Params: paramsJSON,
	})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	var result ipc.RunResult
	if err := remarshal(resp.Result, &result); err != nil {
		return err
	}
	fmt.Printf("run %s: %s\n", result.RunID, result.Status)
	if result.ReturnValue != nil {
		out, _ := json.MarshalIndent(result.ReturnValue, "", "  ")
		fmt.Println(string(out))
	}
	return nil
}

func cmdLogs(c *ipc.ControlClient, runID string) error {
	resp, err := c.Send(ipc.Request{Method: "cli.logs", RunID: runID})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	var entries []ipc.LogEntry
	if err := remarshal(resp.Result, &entries); err != nil {
		return err
	}
	for _, e := range entries {
		fmt.Printf("%s [%s] %s\n", e.Timestamp, e.Level, e.Message)
	}
	return nil
}

func cmdStatus(c *ipc.ControlClient, taskID string) error {
	resp, err := c.Send(ipc.Request{Method: "cli.status", TaskID: taskID})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	out, _ := json.MarshalIndent(resp.Result, "", "  ")
	fmt.Println(string(out))
	return nil
}

func cmdSecrets(c *ipc.ControlClient, args []string) error {
	switch args[0] {
	case "list":
		resp, err := c.Send(ipc.Request{Method: "cli.secrets.list"})
		if err != nil {
			return err
		}
		if resp.Error != "" {
			return fmt.Errorf("%s", resp.Error)
		}
		var keys []string
		if err := remarshal(resp.Result, &keys); err != nil {
			return err
		}
		for _, k := range keys {
			fmt.Println(k)
		}
	case "set":
		if len(args) < 3 {
			return fmt.Errorf("usage: dicode secrets set <key> <value>")
		}
		resp, err := c.Send(ipc.Request{Method: "cli.secrets.set", Key: args[1], StringValue: args[2]})
		if err != nil {
			return err
		}
		if resp.Error != "" {
			return fmt.Errorf("%s", resp.Error)
		}
		fmt.Printf("secret %q set\n", args[1])
	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: dicode secrets delete <key>")
		}
		resp, err := c.Send(ipc.Request{Method: "cli.secrets.delete", Key: args[1]})
		if err != nil {
			return err
		}
		if resp.Error != "" {
			return fmt.Errorf("%s", resp.Error)
		}
		fmt.Printf("secret %q deleted\n", args[1])
	default:
		return fmt.Errorf("unknown secrets subcommand %q", args[0])
	}
	return nil
}

// ensureDaemon starts dicoded in the background if the socket is not reachable.
func ensureDaemon(socketPath string) error {
	if isDaemonRunning(socketPath) {
		return nil
	}
	// Remove a stale socket file so the new daemon can bind cleanly.
	_ = os.Remove(socketPath)

	// Locate the dicoded binary next to the dicode binary.
	self, err := os.Executable()
	if err != nil {
		return err
	}
	dicoded := filepath.Join(filepath.Dir(self), "dicoded")
	if _, err := os.Stat(dicoded); err != nil {
		// Fall back to PATH.
		dicoded, err = exec.LookPath("dicoded")
		if err != nil {
			return fmt.Errorf("dicoded binary not found; start the daemon manually")
		}
	}

	// Log daemon stderr to dataDir/daemon.log so startup failures are diagnosable.
	logPath := filepath.Join(filepath.Dir(socketPath), "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		logFile = nil // non-fatal: proceed without log capture
	}

	cmd := exec.Command(dicoded)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			logFile.Close()
		}
		return fmt.Errorf("start dicoded: %w", err)
	}
	if logFile != nil {
		go func() { _ = cmd.Wait(); logFile.Close() }()
	} else {
		go func() { _ = cmd.Wait() }()
	}

	// Poll until the socket is live (up to 8 seconds).
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if isDaemonRunning(socketPath) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not start within 8 seconds (check %s)", logPath)
}

// isDaemonRunning returns true if the socket exists and accepts connections.
func isDaemonRunning(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func defaultDataDir() string {
	if d := os.Getenv("DICODE_DATA_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "dicode: cannot determine home directory: %v\n", err)
		os.Exit(1)
	}
	return filepath.Join(home, ".dicode")
}

func remarshal(v any, dst any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: dicode <command> [args...]

Commands:
  run <task-id> [key=value ...]   trigger a task and wait for the result
  list                            list registered tasks
  logs <run-id>                   show logs for a run
  status [task-id]                daemon health or task's latest run
  secrets list                    list secret keys
  secrets set <key> <value>       store a secret
  secrets delete <key>            delete a secret
  version                         print version
`)
}
