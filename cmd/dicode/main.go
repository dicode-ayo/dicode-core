// dicode is a single binary that serves as both the CLI and the daemon.
//
// CLI subcommands connect to the daemon over a control socket. If the daemon
// is not running it is auto-started via "dicode daemon" in the background.
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
//	ai <prompt> [flags]             run the configured AI task with a prompt
//	task test <task-id>             run the task's sibling task.test.* through its runtime
//	task create <name> [flags]      scaffold a task; with --ai, open an edit session
//	task edit <task-id> <prompt>    open or resume an AI edit session
//	task save <session-id>          apply a session's changes
//	task cancel <session-id>        discard a session
//	secrets list                    list secret keys
//	secrets set <key> <value>       store a secret
//	secrets delete <key>            delete a secret
//	version                         print version and exit
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dicode/dicode/pkg/daemon"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/schemavalidate"
	"github.com/dicode/dicode/pkg/tasktest"
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

	// `deno` is a local dev/CI helper (relock/verify a task lockfile via the
	// pinned Deno). It touches only files + the provisioned Deno toolchain, so
	// it runs without the daemon — handle it before ensureDaemon.
	if os.Args[1] == "deno" {
		if err := cmdDeno(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "dicode: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// `python` is the uv-side twin of `deno` (relock/verify per-script lock
	// sidecars via the pinned uv). Same contract: files + the provisioned
	// toolchain only, so it runs without the daemon.
	if os.Args[1] == "python" {
		if err := cmdPython(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "dicode: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// `relock` spans both runtimes: it runs the deno and python lock passes
	// for whichever task kinds exist under the tree. Same daemon-free
	// contract as `deno`/`python` above.
	if os.Args[1] == "relock" {
		if err := cmdRelock(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "dicode: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// The daemon subcommand runs the full engine in-process.
	// It must be handled before ensureDaemon — the daemon IS the daemon.
	if os.Args[1] == "daemon" {
		fs := flag.NewFlagSet("daemon", flag.ExitOnError)
		configPath := fs.String("config", "dicode.yaml", "path to config file")
		port := fs.Int("port", 0, "HTTP port (0 = use default 8080 or whatever the wizard picks)")
		fs.Parse(os.Args[2:])
		daemon.Run(*configPath, *port, version)
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
		if err := waitDaemonReady(c, readyWaitTimeout); err != nil {
			return err
		}
		return cmdRun(c, args[1], args[2:])
	case "logs":
		if len(args) < 2 {
			return fmt.Errorf("usage: dicode logs <run-id>")
		}
		return cmdLogs(c, args[1])
	case "resume":
		// Bare `dicode resume` lists suspended runs; with a run id it resumes.
		if len(args) < 2 {
			return cmdResumeList(c)
		}
		if err := waitDaemonReady(c, readyWaitTimeout); err != nil {
			return err
		}
		return cmdResume(c, args[1], args[2:])
	case "status":
		taskID := ""
		if len(args) >= 2 {
			taskID = args[1]
		}
		// Only the task-scoped form needs the registry populated; bare
		// `dicode status` is daemon health and must answer even mid-sync.
		if taskID != "" {
			if err := waitDaemonReady(c, readyWaitTimeout); err != nil {
				return err
			}
		}
		return cmdStatus(c, taskID)
	case "secrets":
		if len(args) < 2 {
			return fmt.Errorf("usage: dicode secrets <list|set|delete> [args...]")
		}
		return cmdSecrets(c, args[1:])
	case "relay":
		if len(args) < 2 {
			return fmt.Errorf("usage: dicode relay <trust-broker> [--yes]")
		}
		return cmdRelay(c, args[1:])
	case "ai":
		return cmdAI(c, args[1:])
	case "task":
		if len(args) < 2 {
			return fmt.Errorf("usage: dicode task <test|create|edit|save|cancel|delete> [args...]")
		}
		return cmdTask(c, args[1:])
	case "auth":
		if len(args) < 2 {
			return fmt.Errorf("usage: dicode auth <reset-passphrase>")
		}
		return cmdAuth(c, args[1:])
	case "mcp":
		if len(args) < 2 {
			return fmt.Errorf("usage: dicode mcp <install|uninstall|print-config> [flags]")
		}
		return cmdMCP(c, args[1:])
	default:
		return fmt.Errorf("unknown command %q — run 'dicode' for usage", args[0])
	}
}

// printResetBanner emits the new passphrase to stdout in the same
// banner shape as pkg/webui/passphrase.go ensurePassphrase, so the
// operator can record it. Operator-terminal output is the contract;
// not a log call. The lgtm suppression below is intentional — CodeQL's
// go/clear-text-logging query treats every fmt.Printf of an upstream
// "passphrase"-named value as a leak, but here the print IS the
// purpose: this is the only moment the operator can capture the
// rotated value. The same pattern lives unsuppressed in
// pkg/webui/passphrase.go ensurePassphrase only because it routes
// through a local named "plaintext" rather than a struct field.
func printResetBanner(value string) {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  dicode — auth passphrase reset                              ║")
	fmt.Println("║                                                              ║")
	fmt.Printf("║  %s  ║\n", value) // lgtm[go/clear-text-logging] intentional operator-terminal display, not a log
	fmt.Println("║                                                              ║")
	fmt.Println("║  Restart dicode (Ctrl-C and `make run` again) for this to    ║")
	fmt.Println("║  take effect — the running WebUI still caches the previous   ║")
	fmt.Println("║  hash. After restart, log in at /security with this value.   ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
}

// cmdAuth implements `dicode auth <subcommand>`. Today only
// reset-passphrase is exposed — generates a fresh WebUI passphrase,
// stores its bcrypt hash in the daemon's kv store, and prints the
// plaintext so the operator can record it. The running WebUI's cached
// passphrase still holds the previous value, so a daemon restart is
// required for the new one to take effect. API keys are managed via the
// WebUI keys panel; resetting them programmatically is out of scope.
func cmdAuth(c *ipc.ControlClient, args []string) error {
	switch args[0] {
	case "reset-passphrase":
		resp, err := c.Send(ipc.Request{Method: "cli.auth.reset_passphrase"})
		if err != nil {
			return err
		}
		if resp.Error != "" {
			return fmt.Errorf("%s", resp.Error)
		}
		var result ipc.AuthResetPassphraseResult
		if err := remarshal(resp.Result, &result); err != nil {
			return fmt.Errorf("decode reset result: %w", err)
		}
		// Hand the plaintext to a small banner printer. The intermediate
		// local + closure match the webui auto-generation pattern in
		// pkg/webui/passphrase.go ensurePassphrase; the indirection keeps
		// CodeQL's clear-text-logging heuristic from flagging the
		// struct-field path on the IPC response.
		printResetBanner(result.Value)
		result.Value = ""
		return nil
	default:
		return fmt.Errorf("unknown auth subcommand %q", args[0])
	}
}

// cmdMCP implements `dicode mcp <subcommand>`. Three operator-friendly
// helpers around the official `claude mcp` machinery:
//
//	install      — mints a fresh API key in the daemon (named
//	                "mcp-<server-name>"), then runs `claude mcp add
//	                --transport http <name> <url> --header
//	                "Authorization: Bearer <key>"`. Re-running rotates
//	                the key (revokes the old one with the same name
//	                first, mints a new one). Pass --key to skip the
//	                mint and use a key you already have.
//
//	uninstall    — revokes the API key on the daemon side and runs
//	                `claude mcp remove <name>`. Idempotent.
//
//	print-config — prints the install command + the equivalent
//	                .claude/mcp.json snippet, no shell-out, no key
//	                minting. Useful for docs / scripting / inspection.
//
// All three accept --name (default "dicode") and --url (default
// "http://localhost:8080/mcp"). The dicode-side key name is
// "mcp-<server-name>" so each MCP-server alias gets its own key — pass
// distinct --name values per host if you want per-host keys.
func cmdMCP(c *ipc.ControlClient, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	name := fs.String("name", "dicode", "MCP server name as it will appear in `claude mcp list`")
	url := fs.String("url", "http://localhost:8080/mcp", "dicode MCP endpoint")
	key := fs.String("key", "", "dicode API key (Bearer). Empty = mint a fresh one via the daemon (recommended).")
	printOnly := fs.Bool("print", false, "print the command instead of running it")

	sub := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	// CLI-managed key names are namespaced under "dicode-cli/mcp/" so the
	// idempotent revoke-by-name in install/uninstall can't accidentally
	// sweep a dashboard-created key that happens to share a friendly name.
	// The slash-separated structure is the marker for "tool-managed";
	// dashboard input shapes don't normally produce slashes.
	keyName := "dicode-cli/mcp/" + *name

	// secretName is where install also stashes the raw key in the
	// daemon's secrets store. The buildin/ai-agent-claude-cli task reads
	// it via permissions.env to wire up its own .claude/mcp.json — same
	// key as the operator's local Claude Code, so one install command
	// connects both. Uninstall deletes it.
	const secretName = "DICODE_MCP_API_KEY"

	switch sub {
	case "install":
		k := resolveAPIKey(*key)
		if k == "" {
			// Zero-touch path: revoke any previous key with this name
			// (idempotent — revoke returns nil for a name that doesn't
			// exist), then mint a fresh one. Rotating on every install
			// is intentional: the raw value of an existing key isn't
			// recoverable (only its hash is stored), so we can't reuse.
			if err := revokeAPIKeyByName(c, keyName); err != nil {
				return fmt.Errorf("revoke previous key (idempotent cleanup): %w", err)
			}
			minted, err := mintAPIKey(c, keyName)
			if err != nil {
				return fmt.Errorf("mint api key via daemon: %w", err)
			}
			k = minted
			fmt.Fprintf(os.Stderr, "dicode: minted API key %q in the daemon (rotates on every install)\n", keyName)
		}
		// Stash the raw key as a secret so the buildin/ai-agent-claude-cli
		// task can read it via permissions.env without a second mint. Same
		// key serves the operator's local Claude Code AND the in-task
		// agent — one install, two consumers. Non-fatal if it fails: the
		// operator's `claude mcp add` step still works; only the in-task
		// MCP wiring is degraded.
		if err := setSecret(c, secretName, k); err != nil {
			fmt.Fprintf(os.Stderr, "dicode: warning: store secret %q: %v (Claude Code is wired but the buildin agent task won't auto-find the key)\n", secretName, err)
		} else {
			fmt.Fprintf(os.Stderr, "dicode: stored API key as secret %q for in-task MCP use\n", secretName)
		}
		argv := []string{
			"mcp", "add", "--transport", "http", *name, *url,
			"--header", "Authorization: Bearer " + k,
		}
		if *printOnly {
			fmt.Println(formatClaudeCmd(argv))
			return nil
		}
		return runClaude(argv, "install", *name)
	case "uninstall":
		// Revoke the daemon-side key first, drop the stashed secret
		// (so the buildin task no longer resolves it), then drop the
		// local Claude MCP entry. Each step may already have been done
		// out of band — all idempotent. We continue past errors so a
		// partially-installed setup can always be cleaned up.
		if err := revokeAPIKeyByName(c, keyName); err != nil {
			fmt.Fprintf(os.Stderr, "dicode: warning: revoke api key %q: %v (continuing)\n", keyName, err)
		}
		if err := deleteSecret(c, secretName); err != nil {
			fmt.Fprintf(os.Stderr, "dicode: warning: delete secret %q: %v (continuing)\n", secretName, err)
		}
		argv := []string{"mcp", "remove", *name}
		if *printOnly {
			fmt.Println(formatClaudeCmd(argv))
			return nil
		}
		return runClaude(argv, "uninstall", *name)
	case "print-config":
		k := resolveAPIKey(*key)
		if k == "" {
			k = "<api-key>"
		}
		argv := []string{
			"mcp", "add", "--transport", "http", *name, *url,
			"--header", "Authorization: Bearer " + k,
		}
		fmt.Println("# CLI:")
		fmt.Println(formatClaudeCmd(argv))
		fmt.Println()
		fmt.Println("# .claude/mcp.json:")
		fmt.Println(mcpJSONSnippet(*name, *url, k))
		return nil
	default:
		return fmt.Errorf("unknown mcp subcommand %q — supported: install | uninstall | print-config", sub)
	}
}

// mintAPIKey asks the daemon to generate a fresh API key with the given
// name and returns the raw value. The daemon stores only the hash so
// the raw value is never recoverable after this — caller must use it
// immediately.
func mintAPIKey(c *ipc.ControlClient, name string) (string, error) {
	resp, err := c.Send(ipc.Request{Method: "cli.api_keys.create", Name: name})
	if err != nil {
		return "", err
	}
	if resp.Error != "" {
		return "", fmt.Errorf("%s", resp.Error)
	}
	var out ipc.APIKeyMintResult
	if err := remarshal(resp.Result, &out); err != nil {
		return "", fmt.Errorf("decode mint result: %w", err)
	}
	return out.Key, nil
}

// revokeAPIKeyByName asks the daemon to delete any API key with the given
// name. Idempotent — the daemon returns nil even when no rows matched.
func revokeAPIKeyByName(c *ipc.ControlClient, name string) error {
	resp, err := c.Send(ipc.Request{Method: "cli.api_keys.revoke_by_name", Name: name})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	return nil
}

// setSecret stores `value` under `key` in the daemon's secrets store.
// Same dispatch as `dicode secrets set` from the CLI; reused here so
// `dicode mcp install` can stash the minted API key for the buildin
// agent task to pick up.
func setSecret(c *ipc.ControlClient, key, value string) error {
	resp, err := c.Send(ipc.Request{Method: "cli.secrets.set", Key: key, StringValue: value})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	return nil
}

// deleteSecret removes the value at `key` from the daemon's secrets store.
// Idempotent on the daemon side; we still surface any error to the caller.
func deleteSecret(c *ipc.ControlClient, key string) error {
	resp, err := c.Send(ipc.Request{Method: "cli.secrets.delete", Key: key})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	return nil
}

// resolveAPIKey returns the first non-empty source: --key flag, the
// DICODE_API_KEY env var, or one read from stdin (when stdin is a
// pipe — interactive prompts would deadlock the test harness). Used
// for the explicit-key opt-out path; the default install flow mints
// via the daemon and never goes through here.
func resolveAPIKey(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv("DICODE_API_KEY"); v != "" {
		return v
	}
	stat, err := os.Stdin.Stat()
	if err == nil && stat.Mode()&os.ModeCharDevice == 0 {
		// stdin is a pipe — read up to the first newline.
		var b [4096]byte
		n, _ := os.Stdin.Read(b[:])
		return strings.TrimSpace(string(b[:n]))
	}
	return ""
}

// runClaude invokes the local `claude` binary with the given argv.
// When `claude` is not on PATH, prints the would-have-run command and
// a hint, returning a non-zero error so the caller knows nothing
// happened. Output and stderr are wired through to the operator's
// terminal so `claude mcp add`'s own diagnostics are visible.
func runClaude(claudeArgs []string, action, name string) error {
	if _, err := exec.LookPath("claude"); err != nil {
		fmt.Fprintln(os.Stderr, "dicode: `claude` binary not found on PATH.")
		fmt.Fprintln(os.Stderr, "        Install via https://install.claude.ai or `npm i -g @anthropic-ai/claude-code`,")
		fmt.Fprintln(os.Stderr, "        then re-run, or copy this command into a host where `claude` is available:")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "  "+formatClaudeCmd(claudeArgs))
		return fmt.Errorf("claude binary not found")
	}
	cmd := exec.Command("claude", claudeArgs...) // #nosec G204 — claudeArgs is built from typed flags + Bearer header, no user shell injection.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude mcp %s: %w", action, err)
	}
	fmt.Fprintf(os.Stderr, "dicode: %sed MCP server %q. Try `claude mcp list` to verify.\n", action, name)
	return nil
}

// formatClaudeCmd renders argv as a copy-pasteable shell line. The
// header value (Authorization: Bearer ...) is the only argument that
// realistically contains spaces, so we shell-quote it. Other args are
// known-safe (URLs, transport literals, the server name).
//
// The slice grows via append without a pre-allocated capacity hint —
// CodeQL's "size computation may overflow" heuristic flags any
// `make(..., len(x)+N)` pattern even when the input is structurally
// bounded (here argv is at most a dozen entries built from typed
// flags). Letting append handle growth keeps it quiet without
// papering over a real concern.
func formatClaudeCmd(argv []string) string {
	parts := []string{"claude"}
	for _, a := range argv {
		if strings.ContainsAny(a, " '\"\\$") {
			// shell-quote with single quotes; escape any embedded singles.
			parts = append(parts, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
		} else {
			parts = append(parts, a)
		}
	}
	return strings.Join(parts, " ")
}

// mcpJSONSnippet returns the .claude/mcp.json shape for the given
// server name, URL, and key. Hand-editable equivalent of the
// `claude mcp add` command above.
func mcpJSONSnippet(name, url, key string) string {
	doc := map[string]any{
		"mcpServers": map[string]any{
			name: map[string]any{
				"type": "http",
				"url":  url,
				"headers": map[string]any{
					"Authorization": "Bearer " + key,
				},
			},
		},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "(json marshal failed: " + err.Error() + ")"
	}
	return string(b)
}

func cmdTask(c *ipc.ControlClient, args []string) error {
	switch args[0] {
	case "test":
		if len(args) < 2 {
			return fmt.Errorf("usage: dicode task test <task-id> [--format=text|junit|gh-summary]")
		}
		if err := waitDaemonReady(c, readyWaitTimeout); err != nil {
			return err
		}
		return cmdTaskTest(c, args[1:])
	case "create":
		return cmdTaskCreate(c, args[1:])
	case "edit":
		return cmdTaskEdit(c, args[1:])
	case "save":
		return cmdTaskSave(c, args[1:])
	case "cancel":
		return cmdTaskCancel(c, args[1:])
	case "delete":
		return cmdTaskDelete(c, args[1:])
	case "approve":
		return cmdTaskApprove(c, args[1:])
	default:
		return fmt.Errorf("unknown task subcommand %q — supported: test, create, edit, save, cancel, delete, approve", args[0])
	}
}

// cmdTaskApprove implements `dicode task approve <task-id>`: approves a task
// held pending by the trust-on-change gate, recording its content hash in
// dicode.lock and arming its triggers.
func cmdTaskApprove(c *ipc.ControlClient, args []string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: dicode task approve <task-id>")
	}
	taskID := args[0]
	resp, err := c.Send(ipc.Request{Method: "cli.task.approve", TaskID: taskID})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	var r ipc.TaskApproveResult
	if err := remarshal(resp.Result, &r); err != nil {
		return err
	}
	fmt.Printf("Task %q approved — triggers armed, hash recorded in dicode.lock.\n", r.TaskID)
	return nil
}

func cmdTaskTest(c *ipc.ControlClient, args []string) error {
	format := "text"
	var taskID string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--format":
			if i+1 >= len(args) {
				return fmt.Errorf("--format requires a value")
			}
			format = args[i+1]
			i++
		case strings.HasPrefix(a, "--format="):
			format = strings.TrimPrefix(a, "--format=")
		case a == "--help" || a == "-h":
			fmt.Fprintln(os.Stderr, "Usage: dicode task test <task-id> [--format=text|junit|gh-summary]")
			return nil
		default:
			if taskID == "" {
				taskID = a
			}
		}
	}
	if taskID == "" {
		return fmt.Errorf("usage: dicode task test <task-id> [--format=text|junit|gh-summary]")
	}

	switch format {
	case "text", "junit", "gh-summary":
	default:
		return fmt.Errorf("unknown --format %q: must be text, junit, or gh-summary", format)
	}

	resp, err := c.Send(ipc.Request{Method: "cli.task.test", TaskID: taskID})
	if err != nil {
		return err
	}

	var r ipc.TaskTestResult
	hasPayload := false
	if resp.Result != nil {
		if rerr := remarshal(resp.Result, &r); rerr == nil {
			hasPayload = r.Output != "" || r.Failed > 0 || r.Passed > 0 || r.Skipped > 0
		}
	}
	if !hasPayload {
		if resp.Error != "" {
			return fmt.Errorf("%s", resp.Error)
		}
		return nil
	}

	result := tasktest.Result{
		TaskID:   r.TaskID,
		Runtime:  r.Runtime,
		Passed:   r.Passed,
		Failed:   r.Failed,
		Skipped:  r.Skipped,
		Duration: time.Duration(r.DurMs) * time.Millisecond,
		ExitCode: r.ExitCode,
		Output:   r.Output,
	}

	switch format {
	case "junit":
		// JUnit XML → stdout; human-readable → stderr so CI logs stay readable.
		fmt.Fprint(os.Stderr, r.Output)
		if !strings.HasSuffix(r.Output, "\n") {
			fmt.Fprintln(os.Stderr)
		}
		if r.Error != "" {
			fmt.Fprintf(os.Stderr, "error: %s\n", r.Error)
		}
		fmt.Fprintf(os.Stderr, "%s: %d passed, %d failed (runtime=%s, %dms)\n",
			r.TaskID, r.Passed, r.Failed, r.Runtime, r.DurMs)
		fmt.Print(tasktest.FormatJUnit(result))
		writeGHStepSummary(tasktest.FormatGHSummary(result))
	case "gh-summary":
		summary := tasktest.FormatGHSummary(result)
		fmt.Print(summary)
		writeGHStepSummary(summary)
		if r.Error != "" {
			fmt.Fprintf(os.Stderr, "error: %s\n", r.Error)
		}
	default: // "text"
		fmt.Print(r.Output)
		if !strings.HasSuffix(r.Output, "\n") {
			fmt.Println()
		}
		fmt.Printf("%s: %d passed, %d failed", r.TaskID, r.Passed, r.Failed)
		if r.Skipped > 0 {
			fmt.Printf(", %d skipped", r.Skipped)
		}
		fmt.Printf(" (runtime=%s, %dms)\n", r.Runtime, r.DurMs)
	}

	if r.Failed > 0 || r.ExitCode != 0 {
		return fmt.Errorf("%d test(s) failed", r.Failed)
	}
	return nil
}

func writeGHStepSummary(markdown string) {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(markdown)
	_ = f.Close()
}

// cmdTaskDelete implements `dicode task delete <task-id> [--source NAME] [--force]`.
//
// Without --force it first fetches a non-destructive preview (chained
// references, trigger schedule, owning source), prints the warnings, and
// prompts for confirmation on stderr; the destructive request is sent only
// after the operator confirms. stdout carries the piped value (the PR URL for
// git sources, or "deleted" for local sources); stderr carries progress and
// warnings so pipelines stay clean.
func cmdTaskDelete(c *ipc.ControlClient, args []string) error {
	var taskID, source string
	force := false
	parseFlags := true
	for i := 0; i < len(args); i++ {
		a := args[i]
		if parseFlags && a == "--" {
			parseFlags = false
			continue
		}
		if parseFlags {
			switch {
			case a == "--force", a == "-f":
				force = true
				continue
			case a == "--source":
				if i+1 >= len(args) {
					return fmt.Errorf("--source requires a value")
				}
				source = args[i+1]
				i++
				continue
			case strings.HasPrefix(a, "--source="):
				source = strings.TrimPrefix(a, "--source=")
				continue
			case a == "--help", a == "-h":
				fmt.Fprintln(os.Stderr, "Usage: dicode task delete <task-id> [--source NAME] [--force]")
				return nil
			case strings.HasPrefix(a, "-"):
				return fmt.Errorf("unknown flag %q — usage: dicode task delete <task-id> [--source NAME] [--force]", a)
			}
		}
		if taskID != "" {
			return fmt.Errorf("unexpected argument %q — only one task id is allowed", a)
		}
		taskID = a
	}
	if taskID == "" {
		return fmt.Errorf("usage: dicode task delete <task-id> [--source NAME] [--force]")
	}

	if !force {
		preview, err := c.Send(ipc.Request{Method: "cli.task.delete", TaskID: taskID, Source: source, Force: false})
		if err != nil {
			return err
		}
		if preview.Error != "" {
			return fmt.Errorf("%s", preview.Error)
		}
		var p ipc.TaskDeleteResult
		if err := remarshal(preview.Result, &p); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "About to delete task %q from source %q.\n", p.TaskID, p.Source)
		if p.Trigger != "" {
			fmt.Fprintf(os.Stderr, "  trigger: %s\n", p.Trigger)
		}
		if len(p.Refs) > 0 {
			fmt.Fprintf(os.Stderr, "  WARNING: %d task(s) chain off this one and will dangle: %s\n",
				len(p.Refs), strings.Join(p.Refs, ", "))
		}
		fmt.Fprintln(os.Stderr, "  Pinned/historical runs of this task remain viewable but lose their friendly name.")
		fmt.Fprint(os.Stderr, "Proceed? [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
		default:
			return fmt.Errorf("aborted")
		}
	}

	resp, err := c.Send(ipc.Request{Method: "cli.task.delete", TaskID: taskID, Source: source, Force: true})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	var r ipc.TaskDeleteResult
	if err := remarshal(resp.Result, &r); err != nil {
		return err
	}
	switch r.Mode {
	case "git":
		fmt.Fprintf(os.Stderr, "Pushed deletion to branch %q; opened PR (run %s).\n", r.Branch, r.PRRunID)
		if r.PRValue != "" {
			fmt.Println(r.PRValue) // PR URL → stdout
		} else {
			fmt.Println(r.Branch)
		}
	default:
		fmt.Fprintf(os.Stderr, "Deleted task %q from source %q. Reconciler will deregister it within ~30s.\n", r.TaskID, r.Source)
		fmt.Println("deleted")
	}
	return nil
}

func cmdRelay(c *ipc.ControlClient, args []string) error {
	switch args[0] {
	case "trust-broker":
		force := false
		for _, a := range args[1:] {
			switch a {
			case "--yes", "-y":
				force = true
			default:
				return fmt.Errorf("unknown flag %q — usage: dicode relay trust-broker --yes", a)
			}
		}
		if !force {
			fmt.Fprintln(os.Stderr, "This will clear the pinned broker signing key.")
			fmt.Fprintln(os.Stderr, "The next relay reconnect will trust-on-first-use the broker's current key.")
			fmt.Fprintln(os.Stderr, "Re-run with --yes to confirm.")
			return fmt.Errorf("aborted")
		}
		resp, err := c.Send(ipc.Request{Method: "cli.relay.trust_broker"})
		if err != nil {
			return err
		}
		if resp.Error != "" {
			return fmt.Errorf("%s", resp.Error)
		}
		fmt.Println("Broker pubkey pin cleared. Restart the daemon to accept the new broker key.")
		return nil
	default:
		return fmt.Errorf("unknown relay subcommand %q", args[0])
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

	// Always print logs so the user can see task output in the terminal.
	if logErr := cmdLogs(c, result.RunID); logErr != nil {
		fmt.Fprintf(os.Stderr, "dicode: fetch logs: %v\n", logErr)
	}

	// A run that suspended on a TTY drives the whole wizard inline: prompt for
	// the resume form, follow the continuation, repeat until it finishes. Piped
	// stdin keeps the one-shot output below so automation still sees the id.
	if result.Status == registry.StatusSuspended && stdinIsInteractive() {
		return followSuspended(c, result.RunID)
	}

	fmt.Printf("run %s: %s\n", result.RunID, result.Status)
	if result.ReturnValue != nil {
		out, _ := json.MarshalIndent(result.ReturnValue, "", "  ")
		fmt.Println(string(out))
	}
	if result.Status == "failure" {
		return fmt.Errorf("task failed")
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

// resumeProp is the subset of a JSON Schema property the CLI reads to prompt
// for and coerce a value.
type resumeProp struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Enum        []any  `json:"enum"`
	Default     any    `json:"default"`
}

// resumePropEntry pairs a property name with its schema, preserving the
// author's declaration order (a map would randomize the prompt order).
type resumePropEntry struct {
	Name string
	Prop resumeProp
}

// parseResumeProps extracts the ordered top-level properties and the required
// set from a JSON Schema. An empty schema yields no properties (the resume then
// carries whatever the caller supplied, subject to server-side validation).
func parseResumeProps(schemaJSON []byte) ([]resumePropEntry, map[string]bool, error) {
	required := map[string]bool{}
	if len(bytes.TrimSpace(schemaJSON)) == 0 {
		return nil, required, nil
	}
	var top struct {
		Properties json.RawMessage `json:"properties"`
		Required   []string        `json:"required"`
	}
	if err := json.Unmarshal(schemaJSON, &top); err != nil {
		return nil, nil, err
	}
	for _, r := range top.Required {
		required[r] = true
	}
	var entries []resumePropEntry
	if len(bytes.TrimSpace(top.Properties)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(top.Properties))
		if _, err := dec.Token(); err != nil { // opening '{'
			return nil, nil, err
		}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, nil, err
			}
			name, _ := keyTok.(string)
			var p resumeProp
			if err := dec.Decode(&p); err != nil {
				return nil, nil, err
			}
			entries = append(entries, resumePropEntry{Name: name, Prop: p})
		}
	}
	return entries, required, nil
}

// coerceResumeValue converts a raw CLI/prompt string to the JSON type the
// property declares, so the resumed task sees a typed value (a number field
// yields a JSON number, a boolean a JSON bool) rather than a string.
func coerceResumeValue(p resumeProp, raw string) (any, error) {
	s := strings.TrimSpace(raw)
	// enum: match the raw string against the declared choices and return the
	// matching entry with its original JSON type. This mirrors the WebUI's
	// option-index approach, so a numeric enum like {enum:[1,2]} coerces to a
	// number even when `type` is omitted — not just a bare string.
	if len(p.Enum) > 0 {
		for _, e := range p.Enum {
			if fmt.Sprintf("%v", e) == s {
				return e, nil
			}
		}
		opts := make([]string, len(p.Enum))
		for i, e := range p.Enum {
			opts[i] = fmt.Sprintf("%v", e)
		}
		return nil, fmt.Errorf("must be one of %s", strings.Join(opts, ", "))
	}
	switch p.Type {
	case "boolean":
		switch strings.ToLower(s) {
		case "true", "1", "yes", "y", "on":
			return true, nil
		case "false", "0", "no", "n", "off":
			return false, nil
		}
		return nil, fmt.Errorf("expected a boolean (true/false), got %q", raw)
	case "integer":
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("expected an integer, got %q", raw)
		}
		return n, nil
	case "number":
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("expected a number, got %q", raw)
		}
		return f, nil
	case "array", "object":
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, fmt.Errorf("expected JSON for %s, got %q", p.Type, raw)
		}
		return v, nil
	default:
		return raw, nil
	}
}

// collectResumeArgs coerces `field=value` args to their declared JSON types.
// A field absent from the schema passes through as a string — server-side
// validation has the final say on whether it is acceptable.
func collectResumeArgs(entries []resumePropEntry, kvArgs []string) (map[string]any, error) {
	props := map[string]resumeProp{}
	for _, e := range entries {
		props[e.Name] = e.Prop
	}
	out := map[string]any{}
	for _, kv := range kvArgs {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid form value %q — expected field=value", kv)
		}
		name, raw := parts[0], parts[1]
		if p, ok := props[name]; ok {
			v, err := coerceResumeValue(p, raw)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			out[name] = v
		} else {
			out[name] = raw
		}
	}
	return out, nil
}

// promptResumeInput walks the schema's properties in order, prompting on out
// (stderr in production, so a piped stdout stays clean) and reading answers from
// in, coercing each to its declared type. An empty answer takes the property's
// default, or is skipped when the property is optional; a required property with
// no default re-prompts. in/out are injected so the loop is unit-testable.
func promptResumeInput(entries []resumePropEntry, required map[string]bool, in io.Reader, out io.Writer) (map[string]any, error) {
	reader := bufio.NewReader(in)
	values := map[string]any{}
	for _, e := range entries {
		p := e.Prop
		label := p.Title
		if label == "" {
			label = e.Name
		}
		if p.Description != "" {
			fmt.Fprintf(out, "%s\n", p.Description)
		}
		if len(p.Enum) > 0 {
			opts := make([]string, len(p.Enum))
			for i, o := range p.Enum {
				opts[i] = fmt.Sprintf("%v", o)
			}
			fmt.Fprintf(out, "  choices: %s\n", strings.Join(opts, ", "))
		}
		for {
			suffix := ""
			if p.Default != nil {
				suffix = fmt.Sprintf(" [%v]", p.Default)
			} else if required[e.Name] {
				suffix = " (required)"
			}
			fmt.Fprintf(out, "%s%s: ", label, suffix)

			line, err := reader.ReadString('\n')
			eof := errors.Is(err, io.EOF)
			if err != nil && !eof {
				return nil, err
			}
			raw := strings.TrimRight(line, "\r\n")
			if strings.TrimSpace(raw) == "" {
				if p.Default != nil {
					values[e.Name] = p.Default
				} else if required[e.Name] {
					if eof {
						return nil, fmt.Errorf("%s is required", e.Name)
					}
					fmt.Fprintln(out, "  required — please enter a value")
					continue
				}
				break
			}
			v, err := coerceResumeValue(p, raw)
			if err != nil {
				if eof {
					return nil, fmt.Errorf("%s: %w", e.Name, err)
				}
				fmt.Fprintf(out, "  %v\n", err)
				continue
			}
			values[e.Name] = v
			break
		}
	}
	return values, nil
}

// cmdResume submits collected resume input for a suspended run. It first pulls
// the run's JSON Schema (cli.resume.get), then either interactively prompts per
// property (no field=value args) or coerces the supplied field=value pairs to
// their declared types, validates locally against the schema, and submits. The
// daemon re-validates authoritatively and resolves the resume token itself.
func cmdResume(c *ipc.ControlClient, runID string, kvArgs []string) error {
	// Interactive follow only engages for a TTY with no inline field=value args:
	// pipes/redirects and explicit values keep the one-shot path below so scripts
	// see the unchanged "continuation run <id>" output. Show the suspended run's
	// logs first for context, mirroring how `dicode run` prints before it waits.
	if len(kvArgs) == 0 && stdinIsInteractive() {
		if logErr := cmdLogs(c, runID); logErr != nil {
			fmt.Fprintf(os.Stderr, "dicode: fetch logs: %v\n", logErr)
		}
		return followSuspended(c, runID)
	}

	infoResp, err := c.Send(ipc.Request{Method: "cli.resume.get", RunID: runID})
	if err != nil {
		return err
	}
	if infoResp.Error != "" {
		return fmt.Errorf("%s", infoResp.Error)
	}
	var info ipc.ResumeInfo
	if err := remarshal(infoResp.Result, &info); err != nil {
		return err
	}

	entries, required, err := parseResumeProps(info.Schema)
	if err != nil {
		return fmt.Errorf("read resume schema: %w", err)
	}

	var values map[string]any
	if len(kvArgs) == 0 {
		values, err = promptResumeInput(entries, required, os.Stdin, os.Stderr)
	} else {
		values, err = collectResumeArgs(entries, kvArgs)
	}
	if err != nil {
		return err
	}

	inputJSON, err := json.Marshal(values)
	if err != nil {
		return err
	}
	// Fail fast with a clear message before the round-trip; the daemon still
	// validates authoritatively (an empty schema imposes no constraint).
	if err := schemavalidate.Validate(info.Schema, inputJSON); err != nil {
		return fmt.Errorf("invalid input: %w", err)
	}

	resp, err := c.Send(ipc.Request{
		Method: "cli.resume",
		RunID:  runID,
		Params: inputJSON,
	})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	var result ipc.ResumeResult
	if err := remarshal(resp.Result, &result); err != nil {
		return err
	}
	fmt.Printf("resumed: continuation run %s\n", result.RunID)
	// The continuation runs asynchronously; point the user at its logs rather
	// than blocking, since resume returns as soon as the run is spawned.
	fmt.Printf("follow: dicode logs %s\n", result.RunID)
	return nil
}

// cmdResumeList prints the runs currently awaiting resume, with the form fields
// each expects, so the operator knows what to pass to `dicode resume`.
func cmdResumeList(c *ipc.ControlClient) error {
	resp, err := c.Send(ipc.Request{Method: "cli.resume.list"})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	var runs []ipc.SuspendedRunSummary
	if err := remarshal(resp.Result, &runs); err != nil {
		return err
	}
	if len(runs) == 0 {
		fmt.Println("no suspended runs")
		return nil
	}
	fmt.Printf("%-36s %-24s %-20s %s\n", "RUN ID", "TASK", "SUSPENDED AT", "FIELDS")
	for _, r := range runs {
		fmt.Printf("%-36s %-24s %-20s %s\n", r.RunID, r.TaskID, orDash(r.SuspendedAt), strings.Join(r.Fields, ","))
	}
	fmt.Println("\nresume with: dicode resume <run-id> [field=value ...]")
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

// cmdAI implements `dicode ai <prompt> [--session-id ID] [--task TASK_ID]`.
//
// Flags may appear anywhere before `--`: `dicode ai "what failed?"`, `dicode ai
// --session-id abc "what failed?"`, and `dicode ai hello --session-id abc
// world` all work — every non-flag argument is joined on spaces to form the
// prompt. A `--` sentinel terminates flag parsing so prompts that literally
// start with `--task` or `--session-id` can still be passed:
//
//	dicode ai -- --task is not a flag here
//
// Output: the `reply` field goes to stdout. On the first turn (when no
// --session-id was provided) the generated `session_id` is written to stderr
// on its own line prefixed with `session: ` so the user can copy-paste it
// into the next turn without it polluting reply-consuming pipelines.
func cmdAI(c *ipc.ControlClient, args []string) error {
	var sessionID, taskID string
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
		switch a {
		case "--session-id", "-s":
			if i+1 >= len(args) {
				return fmt.Errorf("--session-id requires a value")
			}
			sessionID = args[i+1]
			i++
		case "--task":
			if i+1 >= len(args) {
				return fmt.Errorf("--task requires a value")
			}
			taskID = args[i+1]
			i++
		case "--help", "-h":
			fmt.Fprintln(os.Stderr, "Usage: dicode ai <prompt> [--session-id ID] [--task TASK_ID]")
			return nil
		default:
			// Support --session-id=value / --task=value.
			if strings.HasPrefix(a, "--session-id=") {
				sessionID = strings.TrimPrefix(a, "--session-id=")
				continue
			}
			if strings.HasPrefix(a, "--task=") {
				taskID = strings.TrimPrefix(a, "--task=")
				continue
			}
			positional = append(positional, a)
		}
	}
	if len(positional) == 0 {
		return fmt.Errorf("usage: dicode ai <prompt> [--session-id ID] [--task TASK_ID]")
	}
	prompt := strings.Join(positional, " ")

	resp, err := c.Send(ipc.Request{
		Method:    "cli.ai",
		Prompt:    prompt,
		SessionID: sessionID,
		TaskID:    taskID, // empty → daemon uses cfg.AI.Task
	})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	var result ipc.AIResult
	if err := remarshal(resp.Result, &result); err != nil {
		return err
	}
	// Emit session_id to stderr on first turn so pipelines that consume the
	// reply on stdout stay clean. When the caller already passed --session-id
	// we skip this — they already have the id.
	if sessionID == "" && result.SessionID != "" {
		fmt.Fprintf(os.Stderr, "session: %s\n", result.SessionID)
	}
	fmt.Println(result.Reply)
	return nil
}

// readyWaitTimeout bounds how long task-scoped commands (run, status <task>,
// task test) wait for the daemon's first task sync after the control socket
// is reachable. The first sync may include a git clone of the task sources,
// so this is deliberately more generous than ensureDaemon's socket poll.
const readyWaitTimeout = 30 * time.Second

// waitDaemonReady blocks until the daemon reports its initial task sync is
// complete, or timeout elapses. Socket-up does not imply ready-to-serve-
// lookups (#464): the control socket accepts connections before the
// reconciler's first sync has registered the task inventory, so a task-scoped
// command issued right after daemon start could see a spurious "task not
// found". The wait happens daemon-side (single blocking cli.ready round-trip,
// no polling); on a daemon that shuts down mid-wait the request errors out.
// An older daemon that predates cli.ready is treated as ready so mixed
// CLI/daemon versions keep working.
func waitDaemonReady(c *ipc.ControlClient, timeout time.Duration) error {
	resp, err := c.Send(ipc.Request{
		Method: "cli.ready",
		WaitMs: int(timeout / time.Millisecond),
	})
	if err != nil {
		return fmt.Errorf("query daemon readiness: %w", err)
	}
	if resp.Error != "" {
		if strings.Contains(resp.Error, "unknown method") {
			return nil // pre-#464 daemon: no readiness barrier, keep old behaviour
		}
		return fmt.Errorf("query daemon readiness: %s", resp.Error)
	}
	var r ipc.ReadyResult
	if err := remarshal(resp.Result, &r); err != nil {
		return fmt.Errorf("decode readiness result: %w", err)
	}
	if !r.Ready {
		return fmt.Errorf("daemon not ready after %s — initial task sync still running (retry shortly, or check %s)",
			timeout, filepath.Join(defaultDataDir(), "daemon.log"))
	}
	return nil
}

// ensureDaemon starts the daemon in the background if the socket is not reachable.
// It re-execs the current binary with the "daemon" subcommand.
func ensureDaemon(socketPath string) error {
	if isDaemonRunning(socketPath) {
		return nil
	}
	// Remove a stale socket file so the new daemon can bind cleanly.
	_ = os.Remove(socketPath)

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}

	// Log daemon stderr to dataDir/daemon.log so startup failures are diagnosable.
	logPath := filepath.Join(filepath.Dir(socketPath), "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		logFile = nil // non-fatal: proceed without log capture
	}

	cmd := exec.Command(self, "daemon")
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			logFile.Close()
		}
		return fmt.Errorf("start daemon: %w", err)
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
  daemon [-config dicode.yaml]    start the daemon (usually auto-started)
  run <task-id> [key=value ...]   trigger a task and wait for the result
  list                            list registered tasks
  logs <run-id>                   show logs for a run
  status [task-id]                daemon health or task's latest run
  resume [run-id] [field=value]   resume a suspended run (no args lists suspended runs)
  ai <prompt> [flags]             run the configured AI task with a prompt
                                  flags: --session-id ID, --task TASK_ID
  task test <task-id> [flags]     run the task's sibling task.test.* through its runtime
                                  flags: --format=text|junit|gh-summary
  task delete <task-id> [flags]   remove a task from its source (local rm / git PR)
                                  flags: --source NAME, --force
  task approve <task-id>          approve a task held pending by the approval gate
  relock [--check] [dir]          regenerate/verify all task locks (deno.lock + task.py.lock sidecars)
  deno relock [--check] [dir]     regenerate/verify a task tree's deno.lock via the pinned Deno
  python relock [--check] [dir]   regenerate/verify Python tasks' task.py.lock sidecars via the pinned uv
  secrets list                    list secret keys
  secrets set <key> <value>       store a secret
  secrets delete <key>            delete a secret
  relay trust-broker --yes        clear the pinned broker signing key (TOFU re-pin on reconnect)
  mcp install                     mint an API key + run 'claude mcp add' (zero-touch)
  mcp uninstall                   revoke the key + run 'claude mcp remove dicode'
  mcp print-config                print the install command + .claude/mcp.json snippet
  version                         print version
`)
}
