package taskset

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
)

const entryTestTaskSet = `# operator notes survive a rewrite
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: scratch
spec:
  entries:
    # keep me
    existing:
      ref:
        path: ./existing/task.yaml
`

// A scaffolded directory is only reachable through spec.entries — resolution
// never scans the source tree — so this walks the whole path: add the entry,
// then resolve the taskset and require the task to come back.
func TestAddTaskEntry_MakesTaskResolvable(t *testing.T) {
	dir := t.TempDir()
	writeTaskDir(t, dir, "regcheck")
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", "apiVersion: dicode/v1\nkind: TaskSet\nspec:\n  entries: {}\n")

	if err := AddTaskEntry(tsPath, "regcheck"); err != nil {
		t.Fatalf("AddTaskEntry: %v", err)
	}

	tasks, failures, err := newResolver(t).Resolve(context.Background(), "scratch", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("resolve failures: %v", failures)
	}
	if len(tasks) != 1 || tasks[0].ID != "scratch/regcheck" {
		t.Fatalf("resolved %d tasks (%v), want scratch/regcheck", len(tasks), tasks)
	}
}

func TestAddTaskEntry_PreservesExistingDocument(t *testing.T) {
	dir := t.TempDir()
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", entryTestTaskSet)

	if err := AddTaskEntry(tsPath, "regcheck"); err != nil {
		t.Fatalf("AddTaskEntry: %v", err)
	}

	out, err := os.ReadFile(tsPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{
		"# operator notes survive a rewrite",
		"# keep me",
		"name: scratch",
		"path: ./existing/task.yaml",
		"path: ./regcheck/task.yaml",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rewritten taskset lost %q:\n%s", want, got)
		}
	}

	ts, err := LoadTaskSet(tsPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(ts.Spec.Entries) != 2 {
		t.Fatalf("entries = %v, want existing + regcheck", ts.Spec.Entries)
	}
}

// `entries: {}` is a flow mapping; appending to it without switching to block
// style would render every entry on one line.
func TestAddTaskEntry_EmptyFlowMappingBecomesBlock(t *testing.T) {
	dir := t.TempDir()
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", "apiVersion: dicode/v1\nkind: TaskSet\nspec:\n  entries: {}\n")

	if err := AddTaskEntry(tsPath, "regcheck"); err != nil {
		t.Fatalf("AddTaskEntry: %v", err)
	}

	out, err := os.ReadFile(tsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "{") {
		t.Errorf("entry rendered in flow style:\n%s", out)
	}
}

func TestAddTaskEntry_CreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	writeTaskDir(t, dir, "regcheck")
	tsPath := filepath.Join(dir, "taskset.yaml")

	if err := AddTaskEntry(tsPath, "regcheck"); err != nil {
		t.Fatalf("AddTaskEntry: %v", err)
	}

	ts, err := LoadTaskSet(tsPath)
	if err != nil {
		t.Fatalf("load created taskset: %v", err)
	}
	if ts.Spec.Entries["regcheck"] == nil {
		t.Fatalf("entries = %v, want regcheck", ts.Spec.Entries)
	}
}

func TestAddTaskEntry_SameTargetIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", entryTestTaskSet)

	if err := AddTaskEntry(tsPath, "existing"); err != nil {
		t.Fatalf("AddTaskEntry: %v", err)
	}

	ts, err := LoadTaskSet(tsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts.Spec.Entries) != 1 {
		t.Fatalf("entries = %v, want the single existing entry", ts.Spec.Entries)
	}
}

func TestAddTaskEntry_DifferentTargetConflicts(t *testing.T) {
	dir := t.TempDir()
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml",
		"apiVersion: dicode/v1\nkind: TaskSet\nspec:\n  entries:\n    regcheck:\n      ref:\n        path: ./elsewhere/task.yaml\n")

	err := AddTaskEntry(tsPath, "regcheck")
	if !errors.Is(err, ErrEntryConflict) {
		t.Fatalf("err = %v, want ErrEntryConflict", err)
	}
}

func TestAddTaskEntry_RejectsPathSeparator(t *testing.T) {
	dir := t.TempDir()
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", entryTestTaskSet)

	if err := AddTaskEntry(tsPath, "../escape"); err == nil {
		t.Fatal("AddTaskEntry accepted a name with path separators")
	}
}

func TestAddTaskEntry_RejectsNonTaskSetFile(t *testing.T) {
	dir := t.TempDir()
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", "apiVersion: dicode/v1\nkind: Task\nname: x\n")

	if err := AddTaskEntry(tsPath, "regcheck"); err == nil {
		t.Fatal("AddTaskEntry rewrote a kind: Task file")
	}
}

// Two scaffolds racing on one taskset must not drop either entry.
func TestAddTaskEntry_ConcurrentAddsKeepEveryEntry(t *testing.T) {
	dir := t.TempDir()
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", "apiVersion: dicode/v1\nkind: TaskSet\nspec:\n  entries: {}\n")

	names := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}
	var wg sync.WaitGroup
	errs := make([]error, len(names))
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			errs[i] = AddTaskEntry(tsPath, name)
		}(i, name)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("AddTaskEntry(%s): %v", names[i], err)
		}
	}
	ts, err := LoadTaskSet(tsPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, name := range names {
		if ts.Spec.Entries[name] == nil {
			t.Errorf("entry %q lost; got %v", name, ts.Spec.Entries)
		}
	}
}

func TestRemoveTaskEntry_DropsEntryPointingAtTaskDir(t *testing.T) {
	dir := t.TempDir()
	taskDir := writeTaskDir(t, dir, "existing")
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", entryTestTaskSet)

	removed, err := RemoveTaskEntry(tsPath, "existing", taskDir)
	if err != nil {
		t.Fatalf("RemoveTaskEntry: %v", err)
	}
	if !removed {
		t.Fatal("removed = false, want true")
	}
	ts, err := LoadTaskSet(tsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts.Spec.Entries) != 0 {
		t.Fatalf("entries = %v, want empty", ts.Spec.Entries)
	}
}

func TestRemoveTaskEntry_LeavesEntryPointingElsewhere(t *testing.T) {
	dir := t.TempDir()
	other := writeTaskDir(t, dir, "other")
	writeTaskDir(t, dir, "existing")
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", entryTestTaskSet)

	removed, err := RemoveTaskEntry(tsPath, "existing", other)
	if err != nil {
		t.Fatalf("RemoveTaskEntry: %v", err)
	}
	if removed {
		t.Fatal("removed an entry whose ref points at another directory")
	}
}

func TestRemoveTaskEntry_MissingKeyIsNoOp(t *testing.T) {
	dir := t.TempDir()
	taskDir := writeTaskDir(t, dir, "ghost")
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", entryTestTaskSet)

	removed, err := RemoveTaskEntry(tsPath, "ghost", taskDir)
	if err != nil {
		t.Fatalf("RemoveTaskEntry: %v", err)
	}
	if removed {
		t.Fatal("removed = true for a key that was never there")
	}
}

func TestRootTaskSetPath_LocalRefForms(t *testing.T) {
	dir := t.TempDir()
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", entryTestTaskSet)

	// A ref naming the file, and a ref naming the directory that holds it,
	// must both land on the file the resolver reads.
	for _, refPath := range []string{tsPath, dir} {
		src := NewSource("id", "scratch", &Ref{Path: refPath}, "", t.TempDir(), false, 0, zap.NewNop())
		if got := src.RootTaskSetPath(); got != tsPath {
			t.Errorf("RootTaskSetPath() for ref %q = %q, want %q", refPath, got, tsPath)
		}
	}
}

// A directory with no taskset file yet still names where one belongs, so
// scaffolding into it has somewhere to write.
func TestRootTaskSetPath_BareDirectoryWithoutTaskSet(t *testing.T) {
	dir := t.TempDir()
	src := NewSource("id", "scratch", &Ref{Path: dir}, "", t.TempDir(), false, 0, zap.NewNop())

	want := filepath.Join(dir, "taskset.yaml")
	if got := src.RootTaskSetPath(); got != want {
		t.Errorf("RootTaskSetPath() = %q, want %q", got, want)
	}
}
