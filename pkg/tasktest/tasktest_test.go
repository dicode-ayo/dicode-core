package tasktest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

func TestParseDenoSummary_Passed(t *testing.T) {
	out := `Check file:///workspaces/dicode-core/tasks/buildin/webui/task.test.ts
running 7 tests from ./tasks/buildin/webui/task.test.ts
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
