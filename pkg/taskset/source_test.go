package taskset

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/source"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

func newTestSource(t *testing.T, namespace, tsPath string) *Source {
	t.Helper()
	return NewSource(
		tsPath,
		namespace,
		&Ref{Path: tsPath},
		"",
		t.TempDir(),
		false,
		30*time.Second,
		zap.NewNop(),
	)
}

func collectEvents(t *testing.T, ch <-chan source.Event, timeout time.Duration) []source.Event {
	t.Helper()
	var events []source.Event
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, ev)
		case <-deadline:
			return events
		}
	}
}

func TestSource_InitialEvents(t *testing.T) {
	dir := t.TempDir()
	taskDir := writeTaskDir(t, dir, "deploy")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    deploy:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
`
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", tsContent)

	src := newTestSource(t, "infra", tsPath)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, err := src.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	events := collectEvents(t, ch, 2*time.Second)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d: %v", len(events), events)
	}
	ev := events[0]
	if ev.Kind != source.EventAdded {
		t.Errorf("kind: got %q, want Added", ev.Kind)
	}
	if ev.TaskID != "infra/deploy" {
		t.Errorf("TaskID: %q", ev.TaskID)
	}
	if ev.Kinded == nil {
		t.Error("Kinded should be non-nil")
	}
	if asSpec(ev.Kinded).Trigger.Cron != "0 8 * * *" {
		t.Errorf("cron: %q", asSpec(ev.Kinded).Trigger.Cron)
	}
}

// asSpec extracts the *task.Spec carried by a kind: Task value. Tests use it
// after source.Event/ResolvedTask migrated from a typed *task.Spec field to a
// task.Kinded field; every fixture in these tests is a kind: Task.
func asSpec(k task.Kinded) *task.Spec { return k.(*task.Spec) }

func TestSource_UpdateEmitted(t *testing.T) {
	dir := t.TempDir()
	taskDir := writeTaskDir(t, dir, "deploy", "0 8 * * *")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    deploy:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
`
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", tsContent)

	src := newTestSource(t, "infra", tsPath)
	// Prime the snapshot by running a full sync.
	ch0 := make(chan source.Event, 8)
	if err := src.syncAndEmit(context.Background(), ch0); err != nil {
		t.Fatal(err)
	}
	close(ch0)
	collectEvents(t, ch0, time.Second) // drain

	// Modify the task's cron so the hash changes.
	newYaml := "kind: Task\napiVersion: dicode/v1\nname: deploy\nruntime: deno\ntrigger:\n  cron: \"0 3 * * *\"\n"
	if err := os.WriteFile(filepath.Join(taskDir, "task.yaml"), []byte(newYaml), 0644); err != nil {
		t.Fatal(err)
	}

	ch := make(chan source.Event, 8)
	if err := src.syncAndEmit(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	close(ch)

	events := collectEvents(t, ch, time.Second)
	if len(events) != 1 {
		t.Fatalf("want 1 update event, got %d", len(events))
	}
	if events[0].Kind != source.EventUpdated {
		t.Errorf("kind: %q", events[0].Kind)
	}
}

func TestSource_RemovedEmitted(t *testing.T) {
	dir := t.TempDir()
	taskDir := writeTaskDir(t, dir, "deploy")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    deploy:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
`
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", tsContent)

	src := newTestSource(t, "infra", tsPath)

	// Prime snapshot with one task.
	ctx := context.Background()
	ch1 := make(chan source.Event, 8)
	if err := src.syncAndEmit(ctx, ch1); err != nil {
		t.Fatal(err)
	}
	close(ch1)

	// Now update taskset.yaml to have no entries.
	emptyTS := "apiVersion: dicode/v1\nkind: TaskSet\nmetadata:\n  name: infra\nspec:\n  entries: {}\n"
	if err := os.WriteFile(tsPath, []byte(emptyTS), 0644); err != nil {
		t.Fatal(err)
	}

	ch2 := make(chan source.Event, 8)
	if err := src.syncAndEmit(ctx, ch2); err != nil {
		t.Fatal(err)
	}
	close(ch2)

	events := collectEvents(t, ch2, time.Second)
	if len(events) != 1 {
		t.Fatalf("want 1 remove event, got %d: %v", len(events), events)
	}
	if events[0].Kind != source.EventRemoved {
		t.Errorf("kind: %q", events[0].Kind)
	}
	if events[0].TaskID != "infra/deploy" {
		t.Errorf("TaskID: %q", events[0].TaskID)
	}
}

func TestSource_SpecCarriedInEvent(t *testing.T) {
	// Verify overrides are applied and the resolved spec is in the event.
	dir := t.TempDir()
	taskDir := writeTaskDir(t, dir, "deploy")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    deploy:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
      overrides:
        trigger:
          cron: "0 2 * * *"
`
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", tsContent)

	src := newTestSource(t, "infra", tsPath)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, err := src.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	events := collectEvents(t, ch, 2*time.Second)
	if len(events) != 1 {
		t.Fatalf("want 1, got %d", len(events))
	}
	if asSpec(events[0].Kinded).Trigger.Cron != "0 2 * * *" {
		t.Errorf("override not applied: cron=%q", asSpec(events[0].Kinded).Trigger.Cron)
	}
}

func TestSource_ConfigDefaultsDeprecated(t *testing.T) {
	// kind:Config spec.defaults are deprecated and no longer applied to the override stack.
	// The source resolves successfully (no error) but the config values are not applied.
	dir := t.TempDir()
	taskDir := writeTaskDir(t, dir, "deploy")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    deploy:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
`
	tsPath := writeTaskSetFile(t, dir, "taskset.yaml", tsContent)

	cfgContent := `
apiVersion: dicode/v1
kind: Config
metadata:
  name: cfg
spec:
  defaults:
    timeout: 90s
    env:
      - RUNTIME=prod
`
	cfgPath := writeFile(t, dir, "dicode-config.yaml", cfgContent)

	src := NewSource(tsPath, "infra", &Ref{Path: tsPath}, cfgPath, t.TempDir(), false, 30*time.Second, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, err := src.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	events := collectEvents(t, ch, 2*time.Second)
	if len(events) != 1 {
		t.Fatalf("want 1, got %d", len(events))
	}
	spec := asSpec(events[0].Kinded)
	// Deprecated: config defaults should NOT be applied.
	if spec.Timeout == 90*time.Second {
		t.Errorf("deprecated kind:Config defaults should not be applied: timeout was set to 90s")
	}
	em := envMap(spec.Permissions.Env)
	if em["RUNTIME"] == "prod" {
		t.Errorf("deprecated kind:Config defaults should not be applied: found RUNTIME=prod")
	}
}

// TestSource_ParentOverridesApplied verifies that overrides passed via
// NewSource (originating from dicode.yaml's spec.entries.<src>.overrides)
// are honoured by the resolver — closes the latent gap where a daemon-
// initialised Source dropped entry.Overrides before plumbing them into
// resolve.
func TestSource_ParentOverridesApplied(t *testing.T) {
	repoDir := t.TempDir()
	taskDir := writeTaskDir(t, repoDir, "deploy")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: buildin
spec:
  entries:
    deploy:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)

	parent := &Overrides{
		Entries: map[string]*Overrides{
			"deploy": {Enabled: boolPtr(false)},
		},
	}

	src := NewSource(
		"src-id", "buildin",
		&Ref{Path: tsPath},
		"", t.TempDir(), false, 0,
		zaptest.NewLogger(t),
		WithParentOverrides(parent),
	)

	tasks, err := src.resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(tasks))
	}
	if tasks[0].Spec.Enabled {
		t.Errorf("Enabled = true, want false (parent override should disable)")
	}
}

// TestSource_RefreshAfterSetParentOverrides verifies that updating parent
// overrides via SetParentOverrides causes a fresh resolve+emit cycle on the
// already-running source — the user-facing path for the task enable/disable
// toggle in dc-task-list.
func TestSource_RefreshAfterSetParentOverrides(t *testing.T) {
	repoDir := t.TempDir()
	taskDir := writeTaskDir(t, repoDir, "deploy")

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: buildin
spec:
  entries:
    deploy:
      ref:
        path: ` + filepath.Join(taskDir, "task.yaml") + `
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)

	src := NewSource(
		"src-id", "buildin",
		&Ref{Path: tsPath},
		"", t.TempDir(), false, time.Hour, // long poll so only refresh signal fires
		zaptest.NewLogger(t),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ch, err := src.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Drain the initial Added event for "buildin/deploy".
	select {
	case ev := <-ch:
		if ev.Kind != source.EventAdded {
			t.Fatalf("first event kind = %v, want Added", ev.Kind)
		}
		if !asSpec(ev.Kinded).Enabled {
			t.Fatal("initial spec should be Enabled=true")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for initial Added event")
	}

	// Now operator disables the task via dicode.yaml override.
	src.SetParentOverrides(&Overrides{
		Entries: map[string]*Overrides{
			"deploy": {Enabled: boolPtr(false)},
		},
	})

	select {
	case ev := <-ch:
		if ev.Kind != source.EventUpdated {
			t.Fatalf("second event kind = %v, want Updated", ev.Kind)
		}
		if asSpec(ev.Kinded).Enabled {
			t.Error("post-refresh spec.Enabled = true, want false")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for refresh-driven Updated event")
	}
}
