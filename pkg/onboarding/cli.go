package onboarding

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const defaultPort = 8080

// RunCLI walks the user through the wizard on stdin/stdout via a linear
// series of prompts. home is used to render default paths
// (~/dicode-tasks, ~/.dicode). port, if non-zero, overrides the default
// 8080 prompt value in the advanced section — useful when the daemon
// was started with an explicit --port flag for multi-instance setups.
func RunCLI(in io.Reader, out io.Writer, home string, port int) (Result, error) {
	scanner := bufio.NewScanner(in)

	res := Result{
		TaskSetsEnabled: make(map[string]bool, len(TaskSetPresets)),
		LocalTasksDir:   home + "/dicode-tasks",
		DataDir:         home + "/.dicode",
		Port:            portOr(port, defaultPort),
		Passphrase:      GeneratePassphrase(),
	}

	fmt.Fprintln(out, "dicode first-run setup.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Curated task collections (press enter to keep the default):")

	for _, p := range TaskSetPresets {
		fmt.Fprintf(out, "  Enable %s — %s\n", p.Label, p.Desc)
		def := "Y"
		if !p.DefaultOn {
			def = "N"
		}
		fmt.Fprintf(out, "  [%s/%s]: ", def, strings.ToLower(alt(def)))
		line := readLine(scanner)
		res.TaskSetsEnabled[p.Name] = parseYesNo(line, p.DefaultOn)
	}

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
