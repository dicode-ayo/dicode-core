package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/dicode/dicode/pkg/ipc"
)

// cmdRelay dispatches `dicode relay <subcommand>`.
func cmdRelay(c *ipc.ControlClient, args []string) error {
	switch args[0] {
	case "login":
		return cmdRelayLogin(c, args[1:])
	case "trust-broker":
		return cmdRelayTrustBroker(c, args[1:])
	default:
		return fmt.Errorf("unknown relay subcommand %q (try: login, trust-broker)", args[0])
	}
}

func cmdRelayTrustBroker(c *ipc.ControlClient, args []string) error {
	force := false
	for _, a := range args {
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
}

// cmdRelayLogin runs the interactive or non-interactive daemon claim flow.
// It never logs the claim token and never touches the daemon's private key —
// all crypto runs inside the daemon via cli.relay.login.
func cmdRelayLogin(c *ipc.ControlClient, args []string) error {
	fs := flag.NewFlagSet("relay login", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	tokenFlag := fs.String("token", "", "claim token issued by the relay dashboard")
	labelFlag := fs.String("label", "", "human-readable daemon label")
	baseURLFlag := fs.String("base-url", "", "override relay base URL (dev/self-hosted)")
	noBrowser := fs.Bool("no-browser", false, "do not try to open the dashboard in a browser")
	if err := fs.Parse(args); err != nil {
		return err
	}

	token := *tokenFlag
	if token == "" {
		dashURL := buildDashboardURL(*baseURLFlag)
		fmt.Printf("Open %s in your browser and copy the claim token.\n", dashURL)
		if !*noBrowser {
			if err := openInBrowser(dashURL); err != nil {
				fmt.Fprintf(os.Stderr, "dicode: could not open browser automatically: %v\n", err)
			}
		}
		fmt.Print("Paste the claim token: ")
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read claim token: %w", err)
		}
		token = strings.TrimSpace(line)
		if token == "" {
			return fmt.Errorf("claim token required")
		}
	}

	resp, err := c.Send(ipc.Request{
		Method:     "cli.relay.login",
		ClaimToken: token,
		Label:      *labelFlag,
		BaseURL:    *baseURLFlag,
	})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	var result ipc.RelayLoginResult
	if err := remarshal(resp.Result, &result); err != nil {
		return err
	}
	if result.GithubLogin != "" {
		fmt.Printf("Daemon %s claimed as @%s\n", shortUUID(result.UUID), result.GithubLogin)
	} else {
		fmt.Printf("Daemon %s claimed\n", shortUUID(result.UUID))
	}
	return nil
}

// buildDashboardURL assembles the URL that shows the claim token. The base
// URL override is optional; the relay has a sensible default.
func buildDashboardURL(override string) string {
	base := strings.TrimRight(override, "/")
	if base == "" {
		base = "https://relay.dicode.app"
	}
	return base + "/dashboard/claim"
}

// openInBrowser launches a cross-platform URL handler. Falls back silently
// when no opener is available — the URL is always printed first anyway.
func openInBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

// shortUUID returns an abbreviated uuid for terminal output.
func shortUUID(u string) string {
	if len(u) <= 12 {
		return u
	}
	return u[:12] + "…"
}
