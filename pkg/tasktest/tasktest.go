// Package tasktest runs a task's sibling test file (task.test.ts / .js / .mjs)
// through the appropriate runtime and returns a structured result.
//
// Phase 1 supports the Deno runtime only — see issue #159 for Python,
// Docker, and Podman parity.
package tasktest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dicode/dicode/pkg/deno"
	"github.com/dicode/dicode/pkg/task"
)

// Result summarises a task test run. Output is captured combined stdout +
// stderr so callers (CLI, MCP) can display it verbatim; the integer fields
// are parsed from Deno's summary line so machine callers don't have to
// re-parse the text.
type Result struct {
	TaskID     string        `json:"taskID"`
	Runtime    string        `json:"runtime"`
	Passed     int           `json:"passed"`
	Failed     int           `json:"failed"`
	Skipped    int           `json:"skipped"`
	Duration   time.Duration `json:"durationNs"`
	ExitCode   int           `json:"exitCode"`
	Output     string        `json:"output"`
	TestFile   string        `json:"testFile"`
	Error      string        `json:"error,omitempty"`
}

// ErrNoTestFile signals that the task has no sibling test file.
var ErrNoTestFile = fmt.Errorf("task has no test file")

// ErrUnsupportedRuntime signals a task whose runtime this package doesn't
// yet cover. Phase 1 handles Deno only.
type ErrUnsupportedRuntime struct{ Runtime string }

func (e *ErrUnsupportedRuntime) Error() string {
	return fmt.Sprintf("tasktest: runtime %q not yet supported (see #159)", e.Runtime)
}

// Run discovers the test file adjacent to spec.TaskDir, runs it through
// the matching runtime, and returns a Result summarising the outcome.
// Runtime errors (spawn, parse) surface as a non-nil error AND a partial
// Result — callers can show whichever is useful.
func Run(ctx context.Context, spec *task.Spec) (Result, error) {
	if spec == nil || spec.TaskDir == "" {
		return Result{}, fmt.Errorf("tasktest: spec or TaskDir is empty")
	}

	testFile, err := findTestFile(spec)
	if err != nil {
		return Result{TaskID: spec.ID}, err
	}

	switch spec.Runtime {
	// The "" and "js" aliases are defensive — pkg/task.applyDefaults
	// normalizes both to RuntimeDeno at spec-load time, so the registry
	// never hands us a non-normalized value. Keeping the aliases matches
	// pkg/task/spec.go:validate and protects direct callers who construct
	// a task.Spec without going through the loader.
	case task.RuntimeDeno, "", "js":
		return runDeno(ctx, spec, testFile)
	default:
		return Result{TaskID: spec.ID, Runtime: string(spec.Runtime), TestFile: testFile},
			&ErrUnsupportedRuntime{Runtime: string(spec.Runtime)}
	}
}

// findTestFile picks the first task.test.* that exists in the task dir.
// Mirrors the extension priority used by pkg/task.ScriptPath.
func findTestFile(spec *task.Spec) (string, error) {
	for _, ext := range []string{".ts", ".js", ".mjs"} {
		p := filepath.Join(spec.TaskDir, "task.test"+ext)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", ErrNoTestFile
}

// denoSummaryRe matches Deno 2.x's summary line:
//   "ok | 7 passed | 0 failed (80ms)"
//   "FAILED | 5 passed | 2 failed | 1 ignored (2s)"
var denoSummaryRe = regexp.MustCompile(`(\d+)\s+passed(?:\s*\([\d\w]+\))?\s*\|\s*(\d+)\s+failed(?:\s*\|\s*(\d+)\s+ignored)?`)

// denoConfigPath looks for tasks/deno.json walking up from the task dir
// up to a sensible ceiling. Matches the harness config that ships with the
// repo so `npm:...` imports resolve in-process.
func denoConfigPath(taskDir string) string {
	dir := filepath.Clean(taskDir)
	for range 10 {
		candidate := filepath.Join(dir, "tasks", "deno.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

func runDeno(ctx context.Context, spec *task.Spec, testFile string) (Result, error) {
	denoPath, err := deno.EnsureDeno(deno.DefaultVersion)
	if err != nil {
		return Result{TaskID: spec.ID, Runtime: "deno", TestFile: testFile, Error: err.Error()},
			fmt.Errorf("tasktest: ensure deno: %w", err)
	}

	args := []string{"test", "--allow-all"}
	if cfg := denoConfigPath(spec.TaskDir); cfg != "" {
		args = append(args, "--config="+cfg)
	}
	args = append(args, testFile)

	cmd := exec.CommandContext(ctx, denoPath, args...) //nolint:gosec
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	start := time.Now()
	runErr := cmd.Run()
	dur := time.Since(start)
	exitCode := 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}

	output := buf.String()
	passed, failed, skipped := parseDenoSummary(output)

	res := Result{
		TaskID:   spec.ID,
		Runtime:  "deno",
		TestFile: testFile,
		Passed:   passed,
		Failed:   failed,
		Skipped:  skipped,
		Duration: dur,
		ExitCode: exitCode,
		Output:   output,
	}
	// Non-zero exit that we couldn't parse is a legit failure; return nil
	// err but non-zero ExitCode — caller decides how to present it.
	// Unparseable output (e.g. deno itself crashed before running tests)
	// gets an Error string so the CLI can flag it.
	if passed == 0 && failed == 0 && exitCode != 0 {
		res.Error = fmt.Sprintf("deno test exited %d with no summary line", exitCode)
	}
	return res, nil
}

// parseDenoSummary extracts passed/failed/ignored counts from Deno test
// output. Returns zeros if the summary line isn't found.
func parseDenoSummary(output string) (passed, failed, skipped int) {
	// Match against the last N lines — the summary is always at the tail.
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-20; i-- {
		m := denoSummaryRe.FindStringSubmatch(stripANSI(lines[i]))
		if m == nil {
			continue
		}
		passed, _ = strconv.Atoi(m[1])
		failed, _ = strconv.Atoi(m[2])
		if m[3] != "" {
			skipped, _ = strconv.Atoi(m[3])
		}
		return
	}
	return
}

// stripANSI removes ANSI color/control escapes so the regex can match
// both `deno test` output (colored in terminals) and tee'd logs.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }
