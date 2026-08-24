package onboarding

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const defaultPort = 8080

// CLIOptions parameterizes the prompt sequence RunCLIWith walks through.
//
// Seed supplies both the starting values and the defaults offered at each
// prompt, so callers with incompatible path conventions share one
// implementation: the daemon's first-run wizard seeds home-relative
// absolute paths, while `dicode init` seeds ${CONFIGDIR}-relative ones
// that survive a git clone onto another machine.
type CLIOptions struct {
	Seed Result

	// PromptPassphrase adds a dashboard-passphrase prompt. It belongs to
	// callers whose seed passphrase is empty and must be able to stay that
	// way; a caller that generates one up front has nothing to ask.
	PromptPassphrase bool
}

// RunCLI walks the user through the first-run wizard on stdin/stdout via a
// linear series of prompts. home is used to render default paths
// (~/dicode-tasks, ~/.dicode). port, if non-zero, overrides the default
// 8080 prompt value in the advanced section — useful when the daemon
// was started with an explicit --port flag for multi-instance setups.
func RunCLI(in io.Reader, out io.Writer, home string, port int) (Result, error) {
	return RunCLIWith(in, out, CLIOptions{Seed: Result{
		LocalTasksDir: home + "/dicode-tasks",
		DataDir:       home + "/.dicode",
		Port:          portOr(port, defaultPort),
		Passphrase:    GeneratePassphrase(),
	}})
}

// RunCLIWith is RunCLI with caller-supplied defaults. A preset absent from
// Seed.TaskSetsEnabled falls back to its own DefaultOn.
func RunCLIWith(in io.Reader, out io.Writer, opts CLIOptions) (Result, error) {
	scanner := bufio.NewScanner(in)

	res := opts.Seed
	res.Port = portOr(res.Port, defaultPort)
	// The returned Result owns its map; the caller's seed is never written
	// through, so an abandoned run leaves its defaults intact.
	enabled := make(map[string]bool, len(TaskSetPresets))

	fmt.Fprintln(out, "dicode first-run setup.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Curated task collections (press enter to keep the default):")

	for _, p := range TaskSetPresets {
		on, ok := opts.Seed.TaskSetsEnabled[p.Name]
		if !ok {
			on = p.DefaultOn
		}
		fmt.Fprintf(out, "  Enable %s — %s\n", p.Label, p.Desc)
		def := "Y"
		if !on {
			def = "N"
		}
		fmt.Fprintf(out, "  [%s/%s]: ", def, strings.ToLower(alt(def)))
		line := readLine(scanner)
		enabled[p.Name] = parseYesNo(line, on)
	}
	res.TaskSetsEnabled = enabled

	fmt.Fprintln(out)
	fmt.Fprintf(out, "Local tasks directory [%s] (or 'skip' to omit): ", res.LocalTasksDir)
	localResp := readLine(scanner)
	switch strings.ToLower(strings.TrimSpace(localResp)) {
	case "":
		// keep default
	case "skip":
		res.LocalTasksDir = ""
	default:
		res.LocalTasksDir = strings.TrimSpace(localResp)
	}

	fmt.Fprintln(out)
	fmt.Fprint(out, "Configure advanced options (data dir, port)? [y/N]: ")
	adv := readLine(scanner)
	if parseYesNo(adv, false) {
		fmt.Fprintf(out, "  Data directory [%s]: ", res.DataDir)
		if line := readLine(scanner); strings.TrimSpace(line) != "" {
			res.DataDir = strings.TrimSpace(line)
		}
		fmt.Fprintf(out, "  HTTP port [%d]: ", res.Port)
		if line := readLine(scanner); strings.TrimSpace(line) != "" {
			if n, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && n > 0 {
				res.Port = n
			}
		}
	}

	if opts.PromptPassphrase {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Dashboard passphrase. Leave empty and one is generated on first")
		fmt.Fprintln(out, "daemon start, hashed into the local database and printed once —")
		fmt.Fprintln(out, "anything you type here is stored in dicode.yaml as plaintext.")
		// Echoed, not masked: suppressing echo needs the real terminal fd,
		// which this io.Reader seam does not carry.
		fmt.Fprint(out, "  Passphrase [auto-generate]: ")
		if line := strings.TrimSpace(readLine(scanner)); line != "" {
			res.Passphrase = line
		}
	}

	return res, nil
}

func readLine(s *bufio.Scanner) string {
	if !s.Scan() {
		return ""
	}
	return s.Text()
}

// parseYesNo returns true for y/Y/yes, false for n/N/no, and defaultOn for
// anything else (empty line, whitespace).
func parseYesNo(s string, defaultOn bool) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	default:
		return defaultOn
	}
}

func alt(s string) string {
	if s == "Y" {
		return "n"
	}
	return "y"
}

// portOr returns override when it names a valid port, otherwise fallback.
func portOr(override, fallback int) int {
	if override > 0 && override <= 65535 {
		return override
	}
	return fallback
}
