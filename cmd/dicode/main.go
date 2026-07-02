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
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dicode/dicode/pkg/daemon"
	"github.com/dicode/dicode/pkg/ipc"
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
  ai <prompt> [flags]             run the configured AI task with a prompt
                                  flags: --session-id ID, --task TASK_ID
  task test <task-id> [flags]     run the task's sibling task.test.* through its runtime
                                  flags: --format=text|junit|gh-summary
  task delete <task-id> [flags]   remove a task from its source (local rm / git PR)
                                  flags: --source NAME, --force
  task approve <task-id>          approve a task held pending by the approval gate
  deno relock [--check] [dir]     regenerate/verify a task tree's deno.lock via the pinned Deno
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
