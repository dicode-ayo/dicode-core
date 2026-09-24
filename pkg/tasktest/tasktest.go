// Package tasktest runs a task's test through the appropriate runtime and
// returns a structured result: a sibling task.test.ts/.js/.mjs/.py for Deno
// and Python, or the task's own Dockerfile "test" build stage for Docker
// and Podman.
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

	"github.com/dicode/dicode/internal/fsutil"
	"github.com/dicode/dicode/pkg/deno"
	"github.com/dicode/dicode/pkg/runtime/imagegc"
	"github.com/dicode/dicode/pkg/task"
	uvpkg "github.com/dicode/dicode/pkg/uv"
)

// runtimePython matches spec.Runtime's literal value for Python tasks.
// pkg/task's typed Runtime consts (RuntimeDeno, RuntimeDocker, RuntimePodman)
// don't include Python — it's the untyped default case in
// pkg/task/spec.go's validate() runtime switch (see also
// pkg/runtime/python/runtime.go's Name() and daemon.go's task.Runtime("python")
// wiring) — so this mirrors the same bare string literal rather than invent a
// cross-package shared constant for one string.
const runtimePython task.Runtime = "python"

// Result summarises a task test run. Output is captured combined stdout +
// stderr so callers (CLI, MCP) can display it verbatim; the integer fields
// are parsed from Deno's summary line so machine callers don't have to
// re-parse the text.
type Result struct {
	TaskID   string        `json:"taskID"`
	Runtime  string        `json:"runtime"`
	Passed   int           `json:"passed"`
	Failed   int           `json:"failed"`
	Skipped  int           `json:"skipped"`
	Duration time.Duration `json:"durationNs"`
	ExitCode int           `json:"exitCode"`
	Output   string        `json:"output"`
	TestFile string        `json:"testFile"`
	Error    string        `json:"error,omitempty"`
}

// ErrNoTestFile signals that the task has no sibling test file.
var ErrNoTestFile = fmt.Errorf("task has no test file")

// ErrUnsupportedRuntime signals a task whose runtime this package doesn't
// cover: Deno, Python, Docker and Podman are the only ones it knows.
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

	// Docker/Podman are handled up front: runContainerTest needs the
	// Dockerfile's bytes (for the image tag hash), and readDockerTestStage
	// already reads them while checking for the test stage — routing through
	// the generic findTestFile below would mean reading the file twice.
	if spec.Runtime == task.RuntimeDocker || spec.Runtime == task.RuntimePodman {
		dockerfilePath, content, err := readDockerTestStage(spec)
		if err != nil {
			return Result{TaskID: spec.ID}, err
		}
		binary := "docker"
		if spec.Runtime == task.RuntimePodman {
			binary = "podman"
		}
		return runContainerTest(ctx, binary, spec, dockerfilePath, content)
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
	case runtimePython:
		return runPython(ctx, spec, testFile)
	default:
		return Result{TaskID: spec.ID, Runtime: string(spec.Runtime), TestFile: testFile},
			&ErrUnsupportedRuntime{Runtime: string(spec.Runtime)}
	}
}

// findTestFile locates spec's test: a sibling task.test.* for Deno/Python,
// or the Dockerfile itself for Docker/Podman (see findDockerTestStage).
func findTestFile(spec *task.Spec) (string, error) {
	if spec.Runtime == task.RuntimeDocker || spec.Runtime == task.RuntimePodman {
		return findDockerTestStage(spec)
	}

	// For a Python-runtime spec, only .py is considered — otherwise a stale
	// task.test.ts left behind in a task dir that was converted to
	// runtime: python would silently shadow the real task.test.py and get run
	// through the wrong runtime. For every other runtime (including unset/""),
	// the Deno/TS extensions are checked first (matching the priority order used
	// by pkg/task.ScriptPath, ts-preferred-over-js), with .py checked last as a
	// defensive fallback for hand-constructed specs. Symlinks are rejected to
	// stay consistent with the production script-path policy.
	exts := []string{".ts", ".js", ".mjs", ".py"}
	if spec.Runtime == runtimePython {
		exts = []string{".py"}
	}
	for _, ext := range exts {
		p := filepath.Join(spec.TaskDir, "task.test"+ext)
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink == 0 {
			return p, nil
		}
	}
	return "", ErrNoTestFile
}

// dockerFromAsRe captures each Dockerfile stage name declared via
// `FROM [flags] <base> AS <name>` — `.+?` (not `\S+`) so a base image
// preceded by flag tokens like `--platform=...` still matches.
var dockerFromAsRe = regexp.MustCompile(`(?im)^\s*FROM\s+.+?\s+AS\s+([A-Za-z0-9_.-]+)`)

// dockerLineContinuationRe matches a Dockerfile backslash-newline
// continuation, joining a directive written across multiple physical lines
// back into one before stage parsing — the same joining the Dockerfile
// parser itself does.
var dockerLineContinuationRe = regexp.MustCompile(`\\\r?\n[ \t]*`)

// dockerStageNames returns every stage name content declares, in file order.
func dockerStageNames(content []byte) []string {
	joined := dockerLineContinuationRe.ReplaceAll(content, []byte(" "))
	matches := dockerFromAsRe.FindAllSubmatch(joined, -1)
	names := make([]string, len(matches))
	for i, m := range matches {
		names[i] = string(m[1])
	}
	return names
}

// dockerfileHasTestStage reports whether content declares a stage named
// exactly "test" (case-insensitive) — a stage merely prefixed with it, like
// "test-utils", doesn't count.
func dockerfileHasTestStage(content []byte) bool {
	for _, n := range dockerStageNames(content) {
		if strings.EqualFold(n, "test") {
			return true
		}
	}
	return false
}

// ErrTestStageLast signals a Dockerfile whose "test" stage is its last —
// a plain `docker`/`podman build` with no `--target` (what the production
// docker and podman runtimes always run) builds the last stage, so a
// trailing test stage would silently become the deployed image instead of
// the real one.
var ErrTestStageLast = fmt.Errorf("tasktest: Dockerfile's \"test\" stage must not be its last stage")

// findDockerTestStage returns spec's resolved Dockerfile path if it exists
// and declares a "test" build stage, ErrNoTestFile otherwise (no
// docker.build config, no Dockerfile, symlinked Dockerfile, or a Dockerfile
// with no test stage) — the Docker/Podman equivalent of a missing
// task.test.*. ErrTestStageLast instead if a test stage exists but is the
// last one declared.
func findDockerTestStage(spec *task.Spec) (string, error) {
	path, _, err := readDockerTestStage(spec)
	return path, err
}

// readDockerTestStage is findDockerTestStage plus the Dockerfile's bytes,
// for callers (runContainerTest) that need both and shouldn't re-read the
// file to get them.
func readDockerTestStage(spec *task.Spec) (path string, content []byte, err error) {
	if spec.Docker == nil || spec.Docker.Build == nil {
		return "", nil, ErrNoTestFile
	}
	dockerfilePath, _ := spec.Docker.Build.ResolvePaths(spec.TaskDir)
	fi, statErr := os.Lstat(dockerfilePath)
	if statErr != nil || fi.Mode()&os.ModeSymlink != 0 {
		return "", nil, ErrNoTestFile
	}
	b, readErr := os.ReadFile(dockerfilePath)
	if readErr != nil {
		return "", nil, ErrNoTestFile
	}
	if !dockerfileHasTestStage(b) {
		return "", nil, ErrNoTestFile
	}
	names := dockerStageNames(b)
	if strings.EqualFold(names[len(names)-1], "test") {
		return "", nil, ErrTestStageLast
	}
	return dockerfilePath, b, nil
}

// denoSummaryRe matches Deno 2.x's summary line:
//
//	"ok | 7 passed | 0 failed (80ms)"
//	"FAILED | 5 passed | 2 failed | 1 ignored (2s)"
var denoSummaryRe = regexp.MustCompile(`(\d+)\s+passed(?:\s*\([\d\w]+\))?\s*\|\s*(\d+)\s+failed(?:\s*\|\s*(\d+)\s+ignored)?`)

// denoConfigPath looks for tasks/deno.json walking up from the task dir
// up to a sensible ceiling. Matches the harness config that ships with the
// repo so `npm:...` imports resolve in-process.
func denoConfigPath(taskDir string) string {
	// maxParents=9: the task dir plus nine ancestors, matching the historical
	// ten-directory ceiling.
	path, _ := fsutil.FindUp(taskDir, filepath.Join("tasks", "deno.json"), 9)
	return path
}

// runCaptured runs name(args...) to completion, capturing combined
// stdout+stderr and reporting the elapsed duration and process exit code
// (0 on a clean exit, the real code on a normal non-zero exit, -1 if the
// process couldn't be waited on at all — e.g. it never started or was
// killed by a signal). Shared by runDeno and runPython, whose only
// differences are how they build argv and how they parse the resulting
// output.
func runCaptured(ctx context.Context, name string, args ...string) (output string, exitCode int, dur time.Duration) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // args are a runtime-provisioned binary plus a registry-sourced task path, not raw user input.
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	start := time.Now()
	runErr := cmd.Run()
	dur = time.Since(start)
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}
	return buf.String(), exitCode, dur
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

	output, exitCode, dur := runCaptured(ctx, denoPath, args...)
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

// runPython executes a Python task's test file via the managed uv binary.
//
// Contract with the test file (mirrors what `tasks/sdk_test.py` documents
// and what `tasks/examples/hello-python/task.test.py` demonstrates): a
// task.test.py is itself a PEP 723 script declaring `pytest` (plus whatever
// the adjacent task.py needs, e.g. `httpx`) in its inline `dependencies`,
// and ends with:
//
//	if __name__ == "__main__":
//	    from sdk_test import run_pytest_main
//	    run_pytest_main(__file__)
//
// So a single `uv run <testFile>` is enough — uv provisions the declared
// dependencies into an ephemeral venv (exactly like it does for the
// production wrapper in pkg/runtime/python/runtime.go's buildUvRunArgs) and
// executes the file as `__main__`, which in turn invokes pytest.main()
// against itself. This keeps runPython as simple as runDeno (a single
// subprocess, no Go-side dependency introspection) while letting each test
// file declare exactly the dependencies it needs.
//
// Test files are run unlocked (no `--locked`/lock-sidecar handling, unlike
// the production runtime) — task.test.py isn't a task the approval gate
// governs, and pinning its resolution is out of scope for this pass.
func runPython(ctx context.Context, spec *task.Spec, testFile string) (Result, error) {
	uvPath, err := uvpkg.EnsureUv(uvpkg.DefaultVersion)
	if err != nil {
		return Result{TaskID: spec.ID, Runtime: string(runtimePython), TestFile: testFile, Error: err.Error()},
			fmt.Errorf("tasktest: ensure uv: %w", err)
	}

	output, exitCode, dur := runCaptured(ctx, uvPath, "run", testFile)
	passed, failed, skipped := parsePytestSummary(output)

	res := Result{
		TaskID:   spec.ID,
		Runtime:  string(runtimePython),
		TestFile: testFile,
		Passed:   passed,
		Failed:   failed,
		Skipped:  skipped,
		Duration: dur,
		ExitCode: exitCode,
		Output:   output,
	}
	// Same convention as runDeno: a non-zero exit with nothing parsed gets a
	// diagnostic Error string (uv/pytest crashed before producing a summary,
	// e.g. a dependency failed to resolve, or the script raised at import
	// time); a clean parse with failures present is a legitimate test
	// failure, left for the caller to present via Failed/ExitCode.
	if passed == 0 && failed == 0 && skipped == 0 && exitCode != 0 {
		res.Error = fmt.Sprintf("pytest exited %d with no summary line", exitCode)
	}
	return res, nil
}

// pytestSummaryLineRe identifies pytest's own summary line among uv's
// resolution/install chatter and the test run's dot/verbose progress output,
// e.g.:
//
//	"4 passed in 0.02s"
//	"2 failed, 3 passed in 0.20s"
//	"1 passed, 1 skipped in 0.05s"
//	"3 failed in 0.10s"
//
// Anchoring on the trailing "in <duration>s" (pytest's own summary suffix)
// avoids false positives on unrelated lines like "collected 4 items".
var pytestSummaryLineRe = regexp.MustCompile(`\d+\s+\w+.*\sin\s[\d.]+s\b`)

// pytestCountRe extracts each "<N> <word>" pair from a matched summary line.
var pytestCountRe = regexp.MustCompile(`(\d+)\s+(passed|failed|skipped|error|errors|xfailed|xpassed)`)

// parsePytestSummary extracts passed/failed/skipped counts from pytest's
// text summary line. Returns zeros if no summary line is found (e.g. uv or
// Python crashed before pytest ran). xpassed counts as passed and
// error/xfailed count as failed/skipped respectively, matching how pytest's
// own exit code treats them (an error or unexpected failure is a failing
// outcome; xfail is an expected, non-blocking skip-like outcome).
func parsePytestSummary(output string) (passed, failed, skipped int) {
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-20; i-- {
		line := stripANSI(lines[i])
		if !pytestSummaryLineRe.MatchString(line) {
			continue
		}
		matches := pytestCountRe.FindAllStringSubmatch(line, -1)
		if len(matches) == 0 {
			continue
		}
		for _, m := range matches {
			n, _ := strconv.Atoi(m[1])
			switch m[2] {
			case "passed", "xpassed":
				passed += n
			case "failed", "error", "errors":
				failed += n
			case "skipped", "xfailed":
				skipped += n
			}
		}
		return
	}
	return
}

// runContainerTest builds dockerfilePath's "test" stage and runs the
// resulting image via the docker or podman CLI (binary picks which),
// capturing combined stdout+stderr and the exit code the same way
// runCaptured does for Deno/Python. Neither engine's CLI output carries a
// parseable per-test summary, so Passed/Failed/Skipped stay zero — ExitCode
// and Output (plus Error on a non-zero exit) are the only signal.
//
// The test container gets none of docker.*'s host config (ports, volumes,
// caps, network mode) — this isn't a fire path, and a test image built from
// the "test" stage isn't expected to need them.
func runContainerTest(ctx context.Context, binary string, spec *task.Spec, dockerfilePath string, content []byte) (Result, error) {
	binPath, err := exec.LookPath(binary)
	if err != nil {
		return Result{TaskID: spec.ID, Runtime: binary, TestFile: dockerfilePath, Error: err.Error()},
			fmt.Errorf("tasktest: %s not found in PATH: %w", binary, err)
	}

	_, contextDir := spec.Docker.Build.ResolvePaths(spec.TaskDir)
	tag := imagegc.TestTag(spec.ID, content)

	start := time.Now()
	buildOutput, buildExit, _ := runCaptured(ctx, binPath, "build", "--target", "test", "-t", tag, "-f", dockerfilePath, contextDir)
	if buildExit != 0 {
		return Result{
			TaskID:   spec.ID,
			Runtime:  binary,
			TestFile: dockerfilePath,
			Duration: time.Since(start),
			ExitCode: buildExit,
			Output:   buildOutput,
			Error:    fmt.Sprintf("%s build --target test exited %d", binary, buildExit),
		}, nil
	}

	runOutput, runExit, _ := runCaptured(ctx, binPath, "run", "--rm", tag)
	res := Result{
		TaskID:   spec.ID,
		Runtime:  binary,
		TestFile: dockerfilePath,
		Duration: time.Since(start),
		ExitCode: runExit,
		Output:   buildOutput + "\n" + runOutput,
	}
	if runExit != 0 {
		res.Error = fmt.Sprintf("%s run exited %d", binary, runExit)
	}
	return res, nil
}
