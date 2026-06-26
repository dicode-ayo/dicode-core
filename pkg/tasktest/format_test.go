package tasktest

import (
	"strings"
	"testing"
	"time"
)

func TestFormatJUnit_PassOnly(t *testing.T) {
	r := Result{
		TaskID:   "buildin/webui",
		Runtime:  "deno",
		Passed:   7,
		Duration: 80 * time.Millisecond,
	}
	out := FormatJUnit(r)
	if !strings.Contains(out, `<?xml`) {
		t.Error("expected XML header")
	}
	if !strings.Contains(out, `name="buildin/webui"`) {
		t.Errorf("expected task ID in testsuite name, got:\n%s", out)
	}
	if !strings.Contains(out, `tests="7"`) {
		t.Errorf("expected tests=7, got:\n%s", out)
	}
	if !strings.Contains(out, `failures="0"`) {
		t.Errorf("expected failures=0, got:\n%s", out)
	}
	if strings.Contains(out, "<failure") {
		t.Error("expected no <failure> elements for pass-only result")
	}
	if n := strings.Count(out, "<testcase"); n != 7 {
		t.Errorf("expected 7 <testcase> elements, got %d", n)
	}
}

func TestFormatJUnit_WithFailures(t *testing.T) {
	r := Result{
		TaskID:  "buildin/alert",
		Runtime: "deno",
		Passed:  3,
		Failed:  2,
	}
	out := FormatJUnit(r)
	if !strings.Contains(out, `tests="5"`) {
		t.Errorf("expected tests=5, got:\n%s", out)
	}
	if !strings.Contains(out, `failures="2"`) {
		t.Errorf("expected failures=2, got:\n%s", out)
	}
	if n := strings.Count(out, "<failure"); n != 2 {
		t.Errorf("expected 2 <failure> elements, got %d\n%s", n, out)
	}
}

func TestFormatJUnit_Empty(t *testing.T) {
	r := Result{TaskID: "buildin/noop"}
	out := FormatJUnit(r)
	if !strings.Contains(out, `tests="0"`) {
		t.Errorf("expected tests=0 for empty result, got:\n%s", out)
	}
}

func TestFormatGHSummary_Pass(t *testing.T) {
	r := Result{
		TaskID:   "buildin/webui",
		Runtime:  "deno",
		Passed:   7,
		Duration: 80 * time.Millisecond,
	}
	out := FormatGHSummary(r)
	if !strings.Contains(out, "buildin/webui") {
		t.Error("expected task ID in summary")
	}
	if !strings.Contains(out, "7") {
		t.Error("expected pass count in summary")
	}
	if !strings.Contains(out, ":white_check_mark:") {
		t.Error("expected pass icon")
	}
}

func TestFormatGHSummary_Fail(t *testing.T) {
	r := Result{
		TaskID:   "buildin/alert",
		Failed:   1,
		ExitCode: 1,
	}
	out := FormatGHSummary(r)
	if !strings.Contains(out, ":x:") {
		t.Error("expected fail icon when Failed > 0")
	}
}

func TestFormatGHSummary_WithOutput(t *testing.T) {
	r := Result{
		TaskID: "buildin/foo",
		Passed: 1,
		Output: "test 1 ... ok\n",
	}
	out := FormatGHSummary(r)
	if !strings.Contains(out, "<details>") {
		t.Error("expected collapsible details block when output is non-empty")
	}
	if !strings.Contains(out, "test 1 ... ok") {
		t.Error("expected raw output inside details block")
	}
}
