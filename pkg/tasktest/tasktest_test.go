package tasktest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dicode/dicode/pkg/task"
	uvpkg "github.com/dicode/dicode/pkg/uv"
)

func TestParseDenoSummary_Passed(t *testing.T) {
	out := `Check file:///workspaces/dicode-core/tasks/examples/repo-prune/task.test.ts
running 7 tests from ./tasks/examples/repo-prune/task.test.ts
ping returns pong ... ok (1ms)

ok | 7 passed | 0 failed (80ms)
`
	p, f, s := parseDenoSummary(out)
	if p != 7 || f != 0 || s != 0 {
		t.Errorf("passed=%d failed=%d skipped=%d; want 7/0/0", p, f, s)
	}
}

func TestParseDenoSummary_Failed(t *testing.T) {
	out := "FAILED | 5 passed | 2 failed | 1 ignored (1s)\n"
	p, f, s := parseDenoSummary(out)
	if p != 5 || f != 2 || s != 1 {
		t.Errorf("passed=%d failed=%d skipped=%d; want 5/2/1", p, f, s)
	}
}

func TestParseDenoSummary_WithANSI(t *testing.T) {
	out := "\x1b[32mok\x1b[0m | \x1b[1m3 passed\x1b[0m | 0 failed (10ms)\n"
	p, f, _ := parseDenoSummary(out)
	if p != 3 || f != 0 {
		t.Errorf("ANSI stripped incorrectly: passed=%d failed=%d; want 3/0", p, f)
	}
}

func TestParseDenoSummary_Absent(t *testing.T) {
	out := "deno: command failed\n"
	p, f, s := parseDenoSummary(out)
	if p != 0 || f != 0 || s != 0 {
		t.Errorf("no-summary output should parse as zeros; got %d/%d/%d", p, f, s)
	}
}

func TestFindTestFile_TsPreferred(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "task.test.ts"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.test.js"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	spec := &task.Spec{TaskDir: dir}
	got, err := findTestFile(spec)
	if err != nil {
		t.Fatalf("findTestFile: %v", err)
	}
	if filepath.Base(got) != "task.test.ts" {
		t.Errorf("got %q, want task.test.ts", got)
	}
}

func TestFindTestFile_NoTest(t *testing.T) {
	dir := t.TempDir()
	spec := &task.Spec{TaskDir: dir}
	_, err := findTestFile(spec)
	if err != ErrNoTestFile {
		t.Errorf("err = %v, want ErrNoTestFile", err)
	}
}

func TestRun_UnsupportedRuntime(t *testing.T) {
	spec := &task.Spec{ID: "foo", TaskDir: t.TempDir(), Runtime: task.RuntimeDocker}
	_ = os.WriteFile(filepath.Join(spec.TaskDir, "task.test.ts"), []byte(""), 0644)

	_, err := Run(context.Background(), spec)
	if err == nil {
		t.Fatal("expected ErrUnsupportedRuntime")
	}
	if _, ok := err.(*ErrUnsupportedRuntime); !ok {
		t.Errorf("err = %T, want *ErrUnsupportedRuntime", err)
	}
}

func TestRun_NoTestFile(t *testing.T) {
	spec := &task.Spec{ID: "foo", TaskDir: t.TempDir(), Runtime: task.RuntimeDeno}
	_, err := Run(context.Background(), spec)
	if err != ErrNoTestFile {
		t.Errorf("err = %v, want ErrNoTestFile", err)
	}
}

func TestFindTestFile_PyDiscovered(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "task.test.py"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	spec := &task.Spec{TaskDir: dir}
	got, err := findTestFile(spec)
	if err != nil {
		t.Fatalf("findTestFile: %v", err)
	}
	if filepath.Base(got) != "task.test.py" {
		t.Errorf("got %q, want task.test.py", got)
	}
}

func TestFindTestFile_TsPreferredOverPy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "task.test.py"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.test.ts"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	spec := &task.Spec{TaskDir: dir}
	got, err := findTestFile(spec)
	if err != nil {
		t.Fatalf("findTestFile: %v", err)
	}
	if filepath.Base(got) != "task.test.ts" {
		t.Errorf("got %q, want task.test.ts (Deno/TS extensions take priority)", got)
	}
}

func TestParsePytestSummary_PassedOnly(t *testing.T) {
	out := "....                                                            [100%]\n4 passed in 0.02s\n"
	p, f, s := parsePytestSummary(out)
	if p != 4 || f != 0 || s != 0 {
		t.Errorf("passed=%d failed=%d skipped=%d; want 4/0/0", p, f, s)
	}
}

func TestParsePytestSummary_FailedAndPassed(t *testing.T) {
	out := "FF...                                                           [100%]\n2 failed, 3 passed in 0.20s\n"
	p, f, s := parsePytestSummary(out)
	if p != 3 || f != 2 || s != 0 {
		t.Errorf("passed=%d failed=%d skipped=%d; want 3/2/0", p, f, s)
	}
}

func TestParsePytestSummary_WithSkipped(t *testing.T) {
	out := ".s                                                              [100%]\n1 passed, 1 skipped in 0.05s\n"
	p, f, s := parsePytestSummary(out)
	if p != 1 || f != 0 || s != 1 {
		t.Errorf("passed=%d failed=%d skipped=%d; want 1/0/1", p, f, s)
	}
}

func TestParsePytestSummary_FailedOnly(t *testing.T) {
	out := "3 failed in 0.10s\n"
	p, f, s := parsePytestSummary(out)
	if p != 0 || f != 3 || s != 0 {
		t.Errorf("passed=%d failed=%d skipped=%d; want 0/3/0", p, f, s)
	}
}

func TestParsePytestSummary_WithANSI(t *testing.T) {
	out := "\x1b[32m\x1b[1m3 passed\x1b[0m\x1b[32m in 0.10s\x1b[0m\n"
	p, f, _ := parsePytestSummary(out)
	if p != 3 || f != 0 {
		t.Errorf("ANSI stripped incorrectly: passed=%d failed=%d; want 3/0", p, f)
	}
}

func TestParsePytestSummary_Absent(t *testing.T) {
	out := "Traceback (most recent call last):\n  ...\nModuleNotFoundError: No module named 'pytest'\n"
	p, f, s := parsePytestSummary(out)
	if p != 0 || f != 0 || s != 0 {
		t.Errorf("no-summary output should parse as zeros; got %d/%d/%d", p, f, s)
	}
}

func TestFindTestFile_PyNotConfusedByPassedCount(t *testing.T) {
	// Regression guard: "collected 4 items" must not be mistaken for a
	// summary line just because it has a leading digit.
	out := "collected 4 items\n\n4 passed in 0.01s\n"
	p, f, s := parsePytestSummary(out)
	if p != 4 || f != 0 || s != 0 {
		t.Errorf("passed=%d failed=%d skipped=%d; want 4/0/0", p, f, s)
	}
}

// runPythonFixture skips (via t.Skip) if uv can't be provisioned, writes src
// as task.test.py in a fresh temp dir, and drives it through the real Run()
// end-to-end. Shared by TestRun_Python and TestRun_PythonFailure, which
// differ only in the script body and expected pass/fail counts.
func runPythonFixture(t *testing.T, id, src string) Result {
	t.Helper()
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	if _, err := uvpkg.EnsureUv(uvpkg.DefaultVersion); err != nil {
		t.Skipf("uv provisioning failed (offline?): %v", err)
	}

	dir := t.TempDir()
	testFile := filepath.Join(dir, "task.test.py")
	if err := os.WriteFile(testFile, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	spec := &task.Spec{ID: id, TaskDir: dir, Runtime: task.Runtime("python")}
	res, err := Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v\noutput:\n%s", err, res.Output)
	}
	return res
}

// TestRun_Python is the regression-coverage test for #159 Phase 2: before
// this change, a python-runtime spec always returned ErrUnsupportedRuntime
// regardless of whether a task.test.py existed. It builds a minimal
// self-testing PEP 723 task.test.py (the same shape documented in
// tasks/sdk_test.py / demonstrated by tasks/examples/hello-python) and drives
// it through the real uv binary end-to-end.
func TestRun_Python(t *testing.T) {
	res := runPythonFixture(t, "examples/py-tasktest-fixture", `# /// script
# requires-python = ">=3.11"
# dependencies = ["pytest"]
# ///
def test_ok():
    assert 1 + 1 == 2


def test_ok_too():
    assert "a" in "abc"


if __name__ == "__main__":
    import pytest

    raise SystemExit(pytest.main([__file__, "-q", "--no-header", "--import-mode=importlib"]))
`)
	if res.Runtime != "python" {
		t.Errorf("Runtime = %q, want %q", res.Runtime, "python")
	}
	if res.Passed != 2 || res.Failed != 0 {
		t.Errorf("Passed=%d Failed=%d; want 2/0\noutput:\n%s", res.Passed, res.Failed, res.Output)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0\noutput:\n%s", res.ExitCode, res.Output)
	}
}

// TestRun_PythonFailure asserts a genuinely failing pytest run is reported
// as a failure (non-zero Failed, non-zero ExitCode, no Error string — a
// clean parse of a failing summary is not the same as a crash).
func TestRun_PythonFailure(t *testing.T) {
	res := runPythonFixture(t, "examples/py-tasktest-fixture-fail", `# /// script
# requires-python = ">=3.11"
# dependencies = ["pytest"]
# ///
def test_fails():
    assert 1 == 2


if __name__ == "__main__":
    import pytest

    raise SystemExit(pytest.main([__file__, "-q", "--no-header", "--import-mode=importlib"]))
`)
	if res.Failed != 1 || res.Passed != 0 {
		t.Errorf("Passed=%d Failed=%d; want 0/1\noutput:\n%s", res.Passed, res.Failed, res.Output)
	}
	if res.ExitCode == 0 {
		t.Error("ExitCode = 0, want non-zero for a failing test run")
	}
	if res.Error != "" {
		t.Errorf("Error = %q, want empty — a parsed failing summary is not a crash", res.Error)
	}
}
