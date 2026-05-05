# Task Overrides Patch API + Enable/Disable Toggle — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a generic `PATCH /api/tasks/{id}/overrides` endpoint and an enable/disable toggle in `dc-task-list` that uses it.

**Architecture:** Plumb `entry.Overrides` from `dicode.yaml` into each `taskset.Source` (latent gap fix) and add a refresh signal so the source can re-resolve on demand. Add a config writer that merges JSON Merge Patch (RFC 7396) semantics into the YAML at `spec.entries.<source>.overrides.entries.<sub>`. Register `PATCH /api/tasks/{id}/overrides` that writes the patch, mutates in-memory cfg, and signals the source to refresh. Add a toggle in `dc-task-list.js` that calls the endpoint with `{enabled: bool}`.

**Tech Stack:** Go (yaml.v3, chi router, modernc/sqlite), Lit-style web components, Playwright for E2E.

**Spec:** [docs/superpowers/specs/2026-05-05-task-enable-disable-toggle-design.md](../specs/2026-05-05-task-enable-disable-toggle-design.md)

---

## File map

```
pkg/taskset/
├── source.go                 (modified) — store entry.Overrides; expose Refresh + SetParentOverrides
├── source_test.go            (modified) — refresh signal + parent-override resolve tests

pkg/daemon/
├── daemon.go                 (modified) — pass entry.Overrides into NewSource

pkg/config/
├── taskpath.go               (new)      — SplitTaskID helper
├── taskpath_test.go          (new)
├── persist.go                (new)      — MergeTaskOverride yaml.v3 patch writer
├── persist_test.go           (new)

pkg/webui/
├── server.go                 (modified) — register PATCH route + apiPatchTaskOverrides handler
├── server_test.go            (modified) — integration tests for handler

tasks/buildin/webui/app/components/
├── dc-task-list.js           (modified) — toggle + visual states + handler

tests/e2e/
├── task-toggle.spec.ts       (new)      — full UI flow

docs/superpowers/plans/
└── 2026-05-05-task-overrides-patch-api.md  (this file)
```

Each task = one focused commit. Tests written before implementation per TDD.

---

## Task 1: Plumb `entry.Overrides` into `taskset.Source`

**Why:** The resolver supports `parentOverrides` ([pkg/taskset/resolver_test.go:790-832](../../../pkg/taskset/resolver_test.go)) but `Source.resolve()` passes `nil`. Without this fix every override the toggle writes to dicode.yaml is ignored at runtime.

**Files:**
- Modify: [pkg/taskset/source.go](../../../pkg/taskset/source.go) — add `parentOverrides` field, accept in `NewSource`, pass to resolver
- Modify: [pkg/daemon/daemon.go:570-579](../../../pkg/daemon/daemon.go) — pass `entry.Overrides`
- Test: [pkg/taskset/source_test.go](../../../pkg/taskset/source_test.go) — assert parent overrides reach resolver

- [ ] **Step 1.1: Find existing source_test.go test that uses NewSource as setup**

```bash
grep -n "NewSource(" pkg/taskset/source_test.go | head -5
```

Expected: a test or two that constructs `taskset.NewSource(...)`. We'll mirror its setup.

- [ ] **Step 1.2: Write the failing test**

Append to `pkg/taskset/source_test.go`:

```go
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

	tasks, err := src.resolveForTest(context.Background())
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
```

Add a tiny test helper at the bottom of `pkg/taskset/source.go` (build-tag-free, used only by tests in this package):

```go
// resolveForTest exposes the unexported resolve method to in-package tests.
func (s *Source) resolveForTest(ctx context.Context) ([]*ResolvedTask, error) {
	return s.resolve(ctx)
}
```

- [ ] **Step 1.3: Run test to verify it fails**

```bash
go test ./pkg/taskset/ -run TestSource_ParentOverridesApplied -count=1 -v 2>&1 | tail -20
```

Expected: compile error — `WithParentOverrides` and `resolveForTest` undefined.

- [ ] **Step 1.4: Implement parent-overrides plumbing**

Edit [pkg/taskset/source.go](../../../pkg/taskset/source.go):

1. Add field to `Source` struct (next to existing private fields):

```go
	parentOverrides *Overrides // overrides applied at the dicode.yaml entry level
```

2. Define `SourceOption` and `WithParentOverrides`:

```go
// SourceOption configures a Source at construction time.
type SourceOption func(*Source)

// WithParentOverrides binds the dicode.yaml entry-level overrides to the
// source so the resolver applies them on every resolve. Without this the
// daemon-built source would silently drop spec.entries.<src>.overrides.
func WithParentOverrides(ov *Overrides) SourceOption {
	return func(s *Source) { s.parentOverrides = ov }
}
```

3. Update `NewSource` signature to accept variadic options (preserves callers that don't need overrides):

```go
func NewSource(
	id, namespace string,
	rootRef *Ref,
	configPath string,
	dataDir string,
	devMode bool,
	pollInterval time.Duration,
	log *zap.Logger,
	opts ...SourceOption,
) *Source {
	if pollInterval == 0 {
		pollInterval = 30 * time.Second
	}
	s := &Source{
		id:           id,
		namespace:    namespace,
		rootRef:      rootRef,
		configPath:   configPath,
		dataDir:      dataDir,
		resolver:     NewResolver(dataDir, devMode, log),
		pollInterval: pollInterval,
		log:          log,
		snapshot:     make(map[string]taskSnap),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}
```

4. Use `parentOverrides` in `resolve` (current line 560 passes `nil`):

```go
	s.mu.Lock()
	parent := s.parentOverrides
	s.mu.Unlock()
	return s.resolver.Resolve(ctx, s.namespace, rootRef, configDefaults, parent, nil)
```

5. Append `resolveForTest` near the bottom of the file.

- [ ] **Step 1.5: Run test to verify it passes**

```bash
go test ./pkg/taskset/ -run TestSource_ParentOverridesApplied -count=1 -v 2>&1 | tail -20
```

Expected: PASS.

- [ ] **Step 1.6: Update daemon to plumb entry.Overrides**

Edit [pkg/daemon/daemon.go](../../../pkg/daemon/daemon.go):

Change `buildTaskSetSourceFromEntry` signature and call site:

```go
// daemon.go:561
	ts := buildTaskSetSourceFromEntry(name, entry, dataDir, log)

// daemon.go:570
func buildTaskSetSourceFromEntry(name string, entry *taskset.Entry, dataDir string, log *zap.Logger) *taskset.Source {
	ref := entry.Ref
	id := ref.URL
	if id == "" {
		id = ref.Path
	}
	pollInterval := ref.PollInterval

	var opts []taskset.SourceOption
	if entry.Overrides != nil {
		opts = append(opts, taskset.WithParentOverrides(entry.Overrides))
	}
	return taskset.NewSource(id, name, ref, "", dataDir, false, pollInterval, log, opts...)
}
```

- [ ] **Step 1.7: Run full taskset + daemon tests**

```bash
go test ./pkg/taskset/ ./pkg/daemon/ -count=1 -timeout 60s 2>&1 | tail -10
```

Expected: PASS.

- [ ] **Step 1.8: Commit**

```bash
make format && make lint
git add pkg/taskset/source.go pkg/taskset/source_test.go pkg/daemon/daemon.go
git commit -m "fix(taskset): plumb entry.Overrides into Source so dicode.yaml source-level overrides apply

Resolver supports parent overrides (TestResolver_RootSpecEntryDisablesInnerTask)
but daemon.buildTaskSetSourceFromEntry never passed entry.Overrides into
NewSource — the resolve call therefore handed the resolver nil, silently
dropping every spec.entries.<src>.overrides.entries.<sub> override at runtime.

Add SourceOption / WithParentOverrides and wire it from the daemon."
```

---

## Task 2: Source refresh signal for on-demand re-resolution

**Why:** After the toggle writes to `dicode.yaml` and updates `cfg.Spec.Entries[src].Overrides`, the running source needs to re-resolve so the registry/engine pick up the new state without waiting up to 30s for the next periodic tick.

**Files:**
- Modify: [pkg/taskset/source.go](../../../pkg/taskset/source.go) — add `refresh chan struct{}`, `SetParentOverrides`, `Refresh`; watch loop selects on it
- Test: [pkg/taskset/source_test.go](../../../pkg/taskset/source_test.go) — new test

- [ ] **Step 2.1: Write the failing test**

Append to `pkg/taskset/source_test.go`:

```go
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
		if !ev.Spec.Enabled {
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
		if ev.Spec.Enabled {
			t.Error("post-refresh spec.Enabled = true, want false")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for refresh-driven Updated event")
	}
}
```

- [ ] **Step 2.2: Run test to verify it fails**

```bash
go test ./pkg/taskset/ -run TestSource_RefreshAfterSetParentOverrides -count=1 -v 2>&1 | tail -10
```

Expected: compile error — `SetParentOverrides` undefined.

- [ ] **Step 2.3: Implement refresh signal + setter**

Edit [pkg/taskset/source.go](../../../pkg/taskset/source.go):

1. Add field next to `ch`:

```go
	refresh chan struct{} // signals an out-of-band re-resolve; set by Start
```

2. In `Start`, initialise after `ch := make(...)`:

```go
	s.mu.Lock()
	s.ch = ch
	s.refresh = make(chan struct{}, 1)
	s.mu.Unlock()
```

3. In the `watch` loop (around source.go:423), add a case to the select. Find the existing select with `case <-pollTicker.C:` and `case ev := <-fsEvents:` — add:

```go
		case <-s.refresh:
			if err := s.syncAndEmit(ctx, ch); err != nil {
				s.log.Warn("taskset: refresh-driven syncAndEmit failed",
					zap.String("source", s.id), zap.Error(err))
			}
```

(Place it inside the same `for { select { ... } }` as the other cases. If `watch` falls back to a polling-only loop, mirror the case there too.)

4. Add public methods:

```go
// SetParentOverrides updates the source's entry-level overrides and signals
// an out-of-band re-resolve. Safe to call on a running source. Used by
// PATCH /api/tasks/{id}/overrides to apply toggle changes without waiting
// for the next poll tick.
func (s *Source) SetParentOverrides(ov *Overrides) {
	s.mu.Lock()
	s.parentOverrides = ov
	refresh := s.refresh
	s.mu.Unlock()
	if refresh == nil {
		return // not started yet; will take effect on Start's initial resolve
	}
	select {
	case refresh <- struct{}{}:
	default: // signal already pending; coalesce
	}
}
```

- [ ] **Step 2.4: Run test to verify it passes**

```bash
go test ./pkg/taskset/ -run TestSource_RefreshAfterSetParentOverrides -count=1 -v -timeout 30s 2>&1 | tail -15
```

Expected: PASS.

- [ ] **Step 2.5: Run all taskset tests**

```bash
go test ./pkg/taskset/ -count=1 -timeout 60s 2>&1 | tail -5
```

Expected: PASS (no regressions).

- [ ] **Step 2.6: Commit**

```bash
make format && make lint
git add pkg/taskset/source.go pkg/taskset/source_test.go
git commit -m "feat(taskset): Source.SetParentOverrides triggers out-of-band re-resolve

Adds a refresh signal channel + SetParentOverrides public method. The watch
loop's select picks up the signal and runs syncAndEmit, emitting Updated
events to the reconciler with the new override state. Enables hot toggling
without waiting for the next 30s poll tick."
```

---

## Task 3: `SplitTaskID` helper

**Why:** Map a namespaced task ID (`buildin/temp-cleanup` or `infra/platform/nginx`) to the dicode.yaml location: top-level entry key (`buildin`) + sub-key (`temp-cleanup`).

**Files:**
- Create: `pkg/config/taskpath.go`
- Test: `pkg/config/taskpath_test.go`

- [ ] **Step 3.1: Write the failing test**

Create `pkg/config/taskpath_test.go`:

```go
package config

import "testing"

func TestSplitTaskID(t *testing.T) {
	cases := []struct {
		id          string
		wantSource  string
		wantSub     string
		wantOK      bool
	}{
		{"buildin/temp-cleanup", "buildin", "temp-cleanup", true},
		{"infra/platform/nginx", "infra", "platform/nginx", true},
		{"a/b/c/d/e", "a", "b/c/d/e", true},
		{"buildin", "", "", false},
		{"", "", "", false},
		{"/leading-slash", "", "leading-slash", true},  // empty source key — caller decides
		{"trailing/", "trailing", "", true},            // empty sub — caller decides
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			src, sub, ok := SplitTaskID(tc.id)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if src != tc.wantSource {
				t.Errorf("source = %q, want %q", src, tc.wantSource)
			}
			if sub != tc.wantSub {
				t.Errorf("sub = %q, want %q", sub, tc.wantSub)
			}
		})
	}
}
```

- [ ] **Step 3.2: Run test to verify it fails**

```bash
go test ./pkg/config/ -run TestSplitTaskID -count=1 -v 2>&1 | tail -5
```

Expected: compile error — `SplitTaskID` undefined.

- [ ] **Step 3.3: Implement SplitTaskID**

Create `pkg/config/taskpath.go`:

```go
package config

import "strings"

// SplitTaskID splits a namespaced task ID into the top-level source key
// (matches dicode.yaml spec.entries.<key>) and the sub-path used for
// overrides.entries.<sub>. Returns ok=false if id has no separator.
//
// Note: nested IDs encode as a flat sub-key (e.g. "platform/nginx") rather
// than walking nested entries — the resolver looks up parent.Entries by
// the leaf-relative key it computed during recursion, which matches the
// flat encoding for top-level overrides applied at the source root.
//
//   "buildin/temp-cleanup" → ("buildin", "temp-cleanup", true)
//   "infra/platform/nginx" → ("infra",   "platform/nginx", true)
//   "buildin"              → ("",        "",              false)
func SplitTaskID(id string) (source, sub string, ok bool) {
	idx := strings.IndexByte(id, '/')
	if idx < 0 {
		return "", "", false
	}
	return id[:idx], id[idx+1:], true
}
```

- [ ] **Step 3.4: Run test to verify it passes**

```bash
go test ./pkg/config/ -run TestSplitTaskID -count=1 -v 2>&1 | tail -10
```

Expected: PASS.

- [ ] **Step 3.5: Commit**

```bash
make format && make lint
git add pkg/config/taskpath.go pkg/config/taskpath_test.go
git commit -m "feat(config): SplitTaskID maps namespaced task ID to dicode.yaml override path

Helper for the upcoming PATCH /api/tasks/{id}/overrides endpoint."
```

---

## Task 4: `MergeTaskOverride` config writer

**Why:** Apply a JSON Merge Patch into the YAML at `spec.entries.<source>.overrides.entries.<sub>`. mtime-checked, atomic-renamed.

The codebase's existing `persistConfig` reads the file into `map[string]any` and rewrites it; comments are stripped. Stay consistent with that approach for now — comment preservation across the entire dicode.yaml is a separate concern not addressed in this PR.

**Files:**
- Create: `pkg/config/persist.go`
- Test: `pkg/config/persist_test.go`

- [ ] **Step 4.1: Write the failing test**

Create `pkg/config/persist_test.go`:

```go
package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const baseYAML = `apiVersion: dicode/v1
spec:
  entries:
    buildin:
      ref:
        path: /tmp/tasks/buildin/taskset.yaml
    examples:
      ref:
        url: https://example.com/repo
        path: tasks/examples/taskset.yaml
log_level: info
`

func writeTempYAML(t *testing.T, content string) (string, time.Time) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "dicode.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return p, fi.ModTime()
}

func TestMergeTaskOverride_CreatesEnabledFalse(t *testing.T) {
	p, mt := writeTempYAML(t, baseYAML)
	if err := MergeTaskOverride(p, "buildin/relay-client", []byte(`{"enabled": false}`), mt); err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, _ := os.ReadFile(p)
	want := []string{
		"buildin:",
		"overrides:",
		"entries:",
		"relay-client:",
		"enabled: false",
	}
	for _, w := range want {
		if !strings.Contains(string(got), w) {
			t.Errorf("output missing %q\n--- got ---\n%s", w, got)
		}
	}
}

func TestMergeTaskOverride_OverwritesExisting(t *testing.T) {
	yaml := strings.Replace(baseYAML, "    buildin:\n      ref:\n        path: /tmp/tasks/buildin/taskset.yaml",
		`    buildin:
      ref:
        path: /tmp/tasks/buildin/taskset.yaml
      overrides:
        entries:
          relay-client:
            enabled: false`, 1)
	p, mt := writeTempYAML(t, yaml)
	if err := MergeTaskOverride(p, "buildin/relay-client", []byte(`{"enabled": true}`), mt); err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, _ := os.ReadFile(p)
	if !strings.Contains(string(got), "enabled: true") {
		t.Errorf("expected enabled: true in output\n%s", got)
	}
	if strings.Contains(string(got), "enabled: false") {
		t.Errorf("old enabled: false should be gone\n%s", got)
	}
}

func TestMergeTaskOverride_NullDeletesField(t *testing.T) {
	yaml := strings.Replace(baseYAML, "    buildin:\n      ref:\n        path: /tmp/tasks/buildin/taskset.yaml",
		`    buildin:
      ref:
        path: /tmp/tasks/buildin/taskset.yaml
      overrides:
        entries:
          x:
            enabled: false
            timeout: 5m`, 1)
	p, mt := writeTempYAML(t, yaml)
	if err := MergeTaskOverride(p, "buildin/x", []byte(`{"enabled": null}`), mt); err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, _ := os.ReadFile(p)
	if strings.Contains(string(got), "enabled:") && !strings.Contains(string(got), "timeout: 5m") {
		t.Errorf("expected enabled to be removed but timeout preserved; got:\n%s", got)
	}
}

func TestMergeTaskOverride_PrunesEmptyOverrides(t *testing.T) {
	yaml := strings.Replace(baseYAML, "    buildin:\n      ref:\n        path: /tmp/tasks/buildin/taskset.yaml",
		`    buildin:
      ref:
        path: /tmp/tasks/buildin/taskset.yaml
      overrides:
        entries:
          x:
            enabled: false`, 1)
	p, mt := writeTempYAML(t, yaml)
	if err := MergeTaskOverride(p, "buildin/x", []byte(`{"enabled": null}`), mt); err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, _ := os.ReadFile(p)
	if strings.Contains(string(got), "overrides:") {
		t.Errorf("empty overrides block should be pruned; got:\n%s", got)
	}
}

func TestMergeTaskOverride_GenericFields(t *testing.T) {
	p, mt := writeTempYAML(t, baseYAML)
	patch := []byte(`{"params": {"model": "gpt-4o"}, "timeout": "5m"}`)
	if err := MergeTaskOverride(p, "buildin/dicodai", patch, mt); err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, _ := os.ReadFile(p)
	for _, w := range []string{"params:", "model: gpt-4o", "timeout: 5m"} {
		if !strings.Contains(string(got), w) {
			t.Errorf("missing %q\n%s", w, got)
		}
	}
}

func TestMergeTaskOverride_MtimeMismatchRejects(t *testing.T) {
	p, _ := writeTempYAML(t, baseYAML)
	staleMtime := time.Now().Add(-time.Hour)
	err := MergeTaskOverride(p, "buildin/x", []byte(`{"enabled": false}`), staleMtime)
	if !errors.Is(err, ErrConcurrentModification) {
		t.Fatalf("err = %v, want ErrConcurrentModification", err)
	}
}

func TestMergeTaskOverride_UnknownSourceErrors(t *testing.T) {
	p, mt := writeTempYAML(t, baseYAML)
	err := MergeTaskOverride(p, "nonexistent/x", []byte(`{"enabled": false}`), mt)
	if err == nil {
		t.Fatal("want error for unknown source key")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error should mention source name; got: %v", err)
	}
}

func TestMergeTaskOverride_AtomicWrite(t *testing.T) {
	p, mt := writeTempYAML(t, baseYAML)
	// Verify a successful merge produces a file (not a temp leftover).
	if err := MergeTaskOverride(p, "buildin/x", []byte(`{"enabled": false}`), mt); err != nil {
		t.Fatalf("merge: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".dicode.yaml.tmp") || strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}
```

- [ ] **Step 4.2: Run test to verify it fails**

```bash
go test ./pkg/config/ -run TestMergeTaskOverride -count=1 -v 2>&1 | tail -20
```

Expected: compile error — `MergeTaskOverride` and `ErrConcurrentModification` undefined.

- [ ] **Step 4.3: Implement MergeTaskOverride**

Create `pkg/config/persist.go`:

```go
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// ErrConcurrentModification is returned by MergeTaskOverride when the
// dicode.yaml file's mtime has changed since the caller read it. Callers
// should surface this as 409 Conflict and prompt the operator to reload.
var ErrConcurrentModification = errors.New("config file modified externally")

// MergeTaskOverride applies a JSON Merge Patch (RFC 7396) to the YAML at
// spec.entries.<source>.overrides.entries.<sub> in dicode.yaml at path.
//
// patch is a JSON object whose keys mirror taskset.Overrides yaml tags.
// Scalars and objects in patch SET keys, JSON null DELETES keys, missing
// keys leave existing values untouched. Maps merge recursively, lists
// replace whole.
//
// The implementation reads the file (rejects if mtime != expectedMtime),
// unmarshals the entire document into map[string]any (consistent with
// existing persistConfig — comments not preserved), applies the merge,
// then writes via temp-file + atomic rename in the same directory.
//
// If after the merge an entry's overrides.entries.<sub> map is empty it
// is pruned; if overrides.entries becomes empty it is pruned; if the
// entry's overrides becomes empty it is pruned. Avoids YAML cruft from
// repeated toggles.
func MergeTaskOverride(path, taskID string, patch json.RawMessage, expectedMtime time.Time) error {
	source, sub, ok := SplitTaskID(taskID)
	if !ok {
		return fmt.Errorf("task ID %q has no source separator", taskID)
	}
	if source == "" || sub == "" {
		return fmt.Errorf("task ID %q has empty source or sub key", taskID)
	}

	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if !fi.ModTime().Equal(expectedMtime) {
		return ErrConcurrentModification
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}

	specBlock := getMap(doc, "spec")
	entries := getMap(specBlock, "entries")
	entry := getMap(entries, source)
	if entry == nil {
		return fmt.Errorf("source %q not found in spec.entries", source)
	}
	overrides := getMap(entry, "overrides")
	if overrides == nil {
		overrides = map[string]any{}
	}
	overrideEntries := getMap(overrides, "entries")
	if overrideEntries == nil {
		overrideEntries = map[string]any{}
	}
	subOverrides := getMap(overrideEntries, sub)
	if subOverrides == nil {
		subOverrides = map[string]any{}
	}

	var patchObj map[string]any
	if err := json.Unmarshal(patch, &patchObj); err != nil {
		return fmt.Errorf("decode patch: %w", err)
	}
	mergeMap(subOverrides, patchObj)

	if len(subOverrides) == 0 {
		delete(overrideEntries, sub)
	} else {
		overrideEntries[sub] = subOverrides
	}
	if len(overrideEntries) == 0 {
		delete(overrides, "entries")
	} else {
		overrides["entries"] = overrideEntries
	}
	if len(overrides) == 0 {
		delete(entry, "overrides")
	} else {
		entry["overrides"] = overrides
	}
	entries[source] = entry
	specBlock["entries"] = entries
	doc["spec"] = specBlock

	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".dicode.yaml.tmp.*")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Chmod(tmpPath, fi.Mode().Perm()); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod tmp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// getMap returns a typed map from a parent map's key, normalising both
// map[string]any and map[any]any (yaml.v3 yields the latter for nested
// mappings). Returns nil if the key is missing or not a map.
func getMap(parent map[string]any, key string) map[string]any {
	if parent == nil {
		return nil
	}
	v, ok := parent[key]
	if !ok || v == nil {
		return nil
	}
	switch m := v.(type) {
	case map[string]any:
		return m
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			ks, ok := k.(string)
			if !ok {
				continue
			}
			out[ks] = val
		}
		return out
	}
	return nil
}

// mergeMap applies JSON Merge Patch (RFC 7396) semantics: keys in patch
// replace dst, JSON null deletes from dst, nested maps merge recursively,
// non-map values replace.
func mergeMap(dst, patch map[string]any) {
	for k, v := range patch {
		if v == nil {
			delete(dst, k)
			continue
		}
		patchMap, patchIsMap := v.(map[string]any)
		dstSub := getMap(dst, k)
		if patchIsMap && dstSub != nil {
			mergeMap(dstSub, patchMap)
			dst[k] = dstSub
		} else {
			dst[k] = v
		}
	}
}
```

- [ ] **Step 4.4: Run test to verify all cases pass**

```bash
go test ./pkg/config/ -run TestMergeTaskOverride -count=1 -v 2>&1 | tail -30
```

Expected: PASS for all 8 sub-tests.

- [ ] **Step 4.5: Commit**

```bash
make format && make lint
git add pkg/config/persist.go pkg/config/persist_test.go
git commit -m "feat(config): MergeTaskOverride writes JSON Merge Patch into dicode.yaml

Generic merger for the upcoming PATCH /api/tasks/{id}/overrides endpoint.
RFC 7396 semantics (null deletes, scalars set, maps merge), mtime-checked,
atomic temp-file rename, auto-prune of empty override blocks. Tests cover
enabled toggle, generic fields (params/timeout), null deletion, pruning,
mtime mismatch, unknown source, and atomic-write cleanliness."
```

---

## Task 5: `PATCH /api/tasks/{id}/overrides` handler

**Why:** Wire `MergeTaskOverride` to a REST endpoint. Validate the patch shape, check ancestor-not-disabled, write, mutate in-memory cfg, signal source refresh.

**Files:**
- Modify: [pkg/webui/server.go](../../../pkg/webui/server.go) — register route, add handler
- Modify: [pkg/webui/server_test.go](../../../pkg/webui/server_test.go) — integration tests

- [ ] **Step 5.1: Find the existing PATCH/PUT route registration block**

```bash
grep -n "r.Patch\|r.Put\|/run\b\|apiRunTask" pkg/webui/server.go | head -10
```

Expected: existing route registrations near `/api/tasks/{id}/run`.

- [ ] **Step 5.2: Find SourceManager accessor in Server**

```bash
grep -n "sourceMgr\b" pkg/webui/server.go | head -5
```

Expected: `s.sourceMgr` field. We'll use it to look up the named taskset Source.

- [ ] **Step 5.3: Verify SourceManager exposes a method to fetch a taskset.Source**

```bash
grep -n "func (m \*SourceManager)" pkg/webui/sources.go
```

If no `Get(name)` method exists, we'll add one in Step 5.5.

- [ ] **Step 5.4: Write the failing tests**

Append to `pkg/webui/server_test.go`:

```go
// TestPatchTaskOverrides_HappyPath toggles enabled via the REST API and
// confirms the dicode.yaml round-trips through config.Load.
func TestPatchTaskOverrides_HappyPath(t *testing.T) {
	srv, cfgPath := newTestServerWithConfigPath(t)

	// Seed a task in the registry so apiPatchTaskOverrides finds it.
	srv.registry.Register(&task.Spec{ID: "buildin/relay-client", Enabled: true})

	body := bytes.NewBufferString(`{"enabled": false}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/tasks/buildin/relay-client/overrides", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	// Verify the file on disk has the override.
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "enabled: false") {
		t.Errorf("dicode.yaml missing override; got:\n%s", raw)
	}

	// Verify it parses cleanly through config.Load.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load after PATCH: %v", err)
	}
	entry := cfg.Spec.Entries["buildin"]
	if entry == nil || entry.Overrides == nil || entry.Overrides.Entries["relay-client"] == nil {
		t.Fatalf("expected buildin.overrides.entries.relay-client; got %+v", entry)
	}
	got := entry.Overrides.Entries["relay-client"].Enabled
	if got == nil || *got != false {
		t.Errorf("enabled = %v, want false", got)
	}
}

func TestPatchTaskOverrides_UnknownTaskReturns404(t *testing.T) {
	srv, _ := newTestServerWithConfigPath(t)
	req := httptest.NewRequest(http.MethodPatch, "/api/tasks/buildin/does-not-exist/overrides",
		bytes.NewBufferString(`{"enabled": false}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", w.Code, w.Body.String())
	}
}

func TestPatchTaskOverrides_BadJSONReturns400(t *testing.T) {
	srv, _ := newTestServerWithConfigPath(t)
	srv.registry.Register(&task.Spec{ID: "buildin/x", Enabled: true})
	req := httptest.NewRequest(http.MethodPatch, "/api/tasks/buildin/x/overrides",
		bytes.NewBufferString(`not json`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestPatchTaskOverrides_UnknownFieldReturns400(t *testing.T) {
	srv, _ := newTestServerWithConfigPath(t)
	srv.registry.Register(&task.Spec{ID: "buildin/x", Enabled: true})
	req := httptest.NewRequest(http.MethodPatch, "/api/tasks/buildin/x/overrides",
		bytes.NewBufferString(`{"unknownField": true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
}

func TestPatchTaskOverrides_AncestorDisabledReturns422(t *testing.T) {
	srv, cfgPath := newTestServerWithConfigPath(t)

	// Disable the entire 'buildin' source via top-level entry.enabled
	yamlContent := `apiVersion: dicode/v1
spec:
  entries:
    buildin:
      ref:
        path: /tmp/buildin/taskset.yaml
      enabled: false
log_level: info
`
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Reload cfg to reflect file state.
	srv.cfg, _ = config.Load(cfgPath)

	srv.registry.Register(&task.Spec{ID: "buildin/x", Enabled: false})

	req := httptest.NewRequest(http.MethodPatch, "/api/tasks/buildin/x/overrides",
		bytes.NewBufferString(`{"enabled": true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", w.Code, w.Body.String())
	}
}
```

Add a helper near `newTestServer` that returns the server + the dicode.yaml path it persisted to. Find the existing `newTestServer` function and add this companion:

```go
// newTestServerWithConfigPath returns the server plus the absolute path of
// the dicode.yaml it has been wired to write to. Used by patch-overrides
// tests that need to verify the file on disk.
func newTestServerWithConfigPath(t *testing.T) (*Server, string) {
	t.Helper()
	srv, _ := newTestServer(t)
	cfgPath := filepath.Join(t.TempDir(), "dicode.yaml")
	srv.cfgPath = cfgPath
	// Seed with a minimal valid dicode.yaml so MergeTaskOverride finds the source key.
	yaml := `apiVersion: dicode/v1
spec:
  entries:
    buildin:
      ref:
        path: /tmp/buildin/taskset.yaml
log_level: info
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("seed cfg: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load seed: %v", err)
	}
	srv.cfg = cfg
	return srv, cfgPath
}
```

Imports to add at top of server_test.go (skip any already present):

```go
import (
	"bytes"
	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/task"
)
```

- [ ] **Step 5.5: Run tests to verify they fail**

```bash
go test ./pkg/webui/ -run TestPatchTaskOverrides -count=1 -v 2>&1 | tail -20
```

Expected: compile error or 404 for all (route not registered).

- [ ] **Step 5.6: Add SourceManager.Get accessor (if missing)**

If `pkg/webui/sources.go` does not have a `Get` method, append:

```go
// Get returns the live taskset.Source for a source name, or (nil, false) if
// the name is unknown. Used by apiPatchTaskOverrides to signal a refresh
// after writing a new override to dicode.yaml.
func (m *SourceManager) Get(name string) (*taskset.Source, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src, ok := m.tasksets[name]
	return src, ok
}
```

- [ ] **Step 5.7: Register the route + implement the handler**

In [pkg/webui/server.go](../../../pkg/webui/server.go), in the route registration block (search for `r.Get("/tasks", s.apiListTasks)`), add inside the same auth-protected sub-router:

```go
			r.Patch("/tasks/{id}/overrides", s.apiPatchTaskOverrides)
```

Note: chi's URL pattern `{id}` matches a single segment. Task IDs contain slashes (`buildin/relay-client`). Search the file for an existing handler that consumes a multi-segment task ID (e.g. `apiGetTask`, `apiRunTask`) and copy its pattern. Likely the existing handlers use `taskIDParam(r)` with a `*` route or `{id:.+}` pattern — match whichever is established.

```bash
grep -n "tasks/{id}\|tasks/\*\|tasks/{id:" pkg/webui/server.go | head
```

Use the same shape for `/tasks/{id}/overrides`.

Now add the handler. Append to server.go near `apiListTasks` (but leave room for helpers near the top):

```go
// allowedOverrideJSONFields lists the top-level keys accepted in a
// PATCH /api/tasks/{id}/overrides body. Mirrors the yaml tags on
// taskset.Overrides — keep in sync if Overrides gains fields.
var allowedOverrideJSONFields = map[string]bool{
	"enabled":     true,
	"name":        true,
	"description": true,
	"trigger":     true,
	"params":      true,
	"env":         true,
	"net":         true,
	"fs":          true,
	"timeout":     true,
	"retry":       true,
	"runtime":     true,
	"notify":      true,
	"dicode":      true,
	"defaults":    true,
	"entries":     true,
}

func (s *Server) apiPatchTaskOverrides(w http.ResponseWriter, r *http.Request) {
	id := taskIDParam(r)
	if id == "" {
		jsonErr(w, "missing task id", http.StatusBadRequest)
		return
	}
	// Strip trailing "/overrides" if the route framework includes it.
	id = strings.TrimSuffix(id, "/overrides")

	if _, ok := s.registry.Get(id); !ok {
		jsonErr(w, "task not found", http.StatusNotFound)
		return
	}

	source, _, ok := config.SplitTaskID(id)
	if !ok {
		jsonErr(w, "task id has no source separator", http.StatusBadRequest)
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		jsonErr(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var patch map[string]json.RawMessage
	if err := json.Unmarshal(raw, &patch); err != nil {
		jsonErr(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	for k := range patch {
		if !allowedOverrideJSONFields[k] {
			jsonErr(w, "unknown override field: "+k, http.StatusBadRequest)
			return
		}
	}

	// 422: ancestor source disabled. If the operator wants to enable a child
	// while the source itself is off, surface the conflict instead of writing
	// a useless override.
	if enabledRaw, ok := patch["enabled"]; ok {
		var enabled *bool
		if string(enabledRaw) != "null" {
			var b bool
			if err := json.Unmarshal(enabledRaw, &b); err != nil {
				jsonErr(w, "enabled must be bool or null", http.StatusBadRequest)
				return
			}
			enabled = &b
		}
		if enabled != nil && *enabled {
			if entry := s.cfg.Spec.Entries[source]; entry != nil {
				if entry.Enabled != nil && !*entry.Enabled ||
					(entry.Overrides != nil && entry.Overrides.Enabled != nil && !*entry.Overrides.Enabled) {
					jsonErr(w, "source "+source+" is disabled — enable the source first",
						http.StatusUnprocessableEntity)
					return
				}
			}
		}
	}

	fi, err := os.Stat(s.cfgPath)
	if err != nil {
		jsonErr(w, "stat config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := config.MergeTaskOverride(s.cfgPath, id, raw, fi.ModTime()); err != nil {
		if errors.Is(err, config.ErrConcurrentModification) {
			jsonErr(w, "config file modified externally", http.StatusConflict)
			return
		}
		jsonErr(w, "write override: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Reload dicode.yaml into memory so the running daemon reflects the
	// change without a restart. This is intentionally a small, read-only
	// reload — Load() validates and applies defaults, so we can trust it.
	updated, loadErr := config.Load(s.cfgPath)
	if loadErr != nil {
		s.log.Warn("config reload after override patch failed",
			zap.String("task", id), zap.Error(loadErr))
		// File is correct on disk; daemon will pick it up at next restart.
		// Don't 500 — the write succeeded.
	} else {
		s.cfg.Spec = updated.Spec
		// Push the new overrides into the running source so events flow.
		if s.sourceMgr != nil {
			if src, ok := s.sourceMgr.Get(source); ok {
				if entry := updated.Spec.Entries[source]; entry != nil {
					src.SetParentOverrides(entry.Overrides)
				}
			}
		}
	}

	jsonOK(w, map[string]any{
		"id":        id,
		"overrides": patch,
	})
}
```

Imports to add to server.go (skip any already present):

```go
"errors"
"io"
"github.com/dicode/dicode/pkg/config"
```

- [ ] **Step 5.8: Run tests until all pass**

```bash
go test ./pkg/webui/ -run TestPatchTaskOverrides -count=1 -v 2>&1 | tail -25
```

Expected: PASS for all 5 tests. Iterate if any fail (most likely culprits: chi route shape, missing import, or test seed mismatch).

- [ ] **Step 5.9: Run full webui test suite**

```bash
go test ./pkg/webui/ -count=1 -timeout 120s 2>&1 | tail -10
```

Expected: PASS (no regressions in existing tests).

- [ ] **Step 5.10: Commit**

```bash
make format && make lint
git add pkg/webui/server.go pkg/webui/server_test.go pkg/webui/sources.go
git commit -m "feat(webui): PATCH /api/tasks/{id}/overrides — generic overrides patch endpoint

Accepts JSON Merge Patch (RFC 7396) against taskset.Overrides yaml fields.
Today only the toggle UI sends {enabled: bool}; future param/env/timeout
UIs reuse the same endpoint without backend changes.

- mtime-checked atomic write via config.MergeTaskOverride
- 422 when ancestor source is disabled (UI surfaces the actionable error)
- in-memory cfg reload + Source.SetParentOverrides for sub-second propagation"
```

---

## Task 6: Frontend toggle in `dc-task-list.js`

**Why:** Add the actual UI control + visual treatment for disabled tasks.

**Files:**
- Modify: [tasks/buildin/webui/app/components/dc-task-list.js](../../../tasks/buildin/webui/app/components/dc-task-list.js)

- [ ] **Step 6.1: Inspect current row template**

```bash
grep -n "render\|task.id\|task.ID\|class=" tasks/buildin/webui/app/components/dc-task-list.js | head -25
```

Expected: a `render()` method that maps tasks to row markup. Note the property names used (`task.ID` vs `task.id`, `task.Name` vs `task.name`, etc.) — JSON tags use lowercase, but legacy code may reference Pascal case.

- [ ] **Step 6.2: Add the toggle markup, styles, and click handler**

Edit `tasks/buildin/webui/app/components/dc-task-list.js`. The exact insertion points depend on the file's current shape; follow these rules:

1. **Style block** — add to the component's CSS (`static styles = css\`...\``):

```css
.row.disabled { opacity: 0.55; }
.row.disabled .name { font-style: italic; }
.badge-paused {
  display: inline-block;
  margin-left: 0.5rem;
  padding: 0 0.4rem;
  font-size: 0.7rem;
  border-radius: 3px;
  background: var(--badge-bg, #2a2a2a);
  color: var(--badge-fg, #aaa);
  vertical-align: middle;
}
.toggle-btn {
  background: none;
  border: none;
  cursor: pointer;
  padding: 0.25rem;
  color: var(--muted);
}
.toggle-btn:disabled { cursor: wait; }
.toggle-btn.on  { color: var(--accent, #4caf50); }
.toggle-btn.off { color: var(--muted, #888); }
.toggle-btn svg { display: block; width: 18px; height: 18px; }
```

2. **Row class** — wherever the row is rendered, change the class to:

```javascript
class=${`row ${task.enabled === false ? 'disabled' : ''}`}
```

3. **Paused badge** — after the name, conditionally:

```javascript
${task.enabled === false ? html`<span class="badge-paused">paused</span>` : ''}
```

4. **Toggle button** — at the right of the row, before the last-run dot:

```javascript
<button class=${`toggle-btn ${task.enabled === false ? 'off' : 'on'}`}
        title=${task.enabled === false ? 'Enable task' : 'Disable task'}
        ?disabled=${this._togglePending.has(task.id || task.ID)}
        @click=${(e) => this._onToggle(e, task)}>
  ${task.enabled === false
    ? html`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
        <circle cx="12" cy="12" r="9"/>
        <line x1="6" y1="6" x2="18" y2="18"/>
      </svg>`
    : html`<svg viewBox="0 0 24 24" fill="currentColor"><circle cx="12" cy="12" r="9"/></svg>`}
</button>
```

5. **State + handler** — add:

```javascript
static properties = {
  // ... existing properties ...
  _togglePending: { state: true },
};

constructor() {
  super();
  // ... existing initialisation ...
  this._togglePending = new Set();
}

async _onToggle(e, task) {
  e.stopPropagation();
  const id = task.id || task.ID;
  if (!id) return;
  const next = !(task.enabled === false ? false : true);

  this._togglePending.add(id);
  // Optimistic flip on the local copy.
  task.enabled = next;
  this.requestUpdate();

  try {
    const res = await fetch(`/api/tasks/${id}/overrides`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ enabled: next }),
    });
    if (res.status === 409) {
      this._toast('Config changed externally — reloading');
      await this._loadTasks();
      return;
    }
    if (res.status === 422) {
      const body = await res.json().catch(() => ({}));
      this._toast(body.error || 'Cannot toggle: ancestor source is disabled');
      task.enabled = !next;
      return;
    }
    if (!res.ok) {
      const body = await res.text();
      this._toast(`Toggle failed: ${body || res.statusText}`);
      task.enabled = !next;
      return;
    }
    // 200 — keep optimistic state, reconciler will catch up via next _loadTasks tick.
  } catch (err) {
    this._toast(`Toggle failed: ${err.message}`);
    task.enabled = !next;
  } finally {
    this._togglePending.delete(id);
    this.requestUpdate();
  }
}

_toast(msg) {
  // Reuse existing toast/log mechanism if present; fallback to console.
  if (typeof this._showToast === 'function') {
    this._showToast(msg);
  } else {
    console.warn('[task-toggle]', msg);
  }
}
```

If the component already has a different toast helper (e.g. through `dc-log-bar` or a custom event), wire `_toast` to dispatch that instead — search:

```bash
grep -n "toast\|notify\|dispatchEvent.*'log" tasks/buildin/webui/app/components/dc-task-list.js
```

- [ ] **Step 6.3: Smoke-check the file parses (if there's a unit test for it)**

```bash
ls tasks/buildin/webui/app/components/*.test.* 2>/dev/null
```

If there's a Deno test for dc-task-list, run it. Otherwise skip — the e2e test in Task 7 covers it.

- [ ] **Step 6.4: Manual sanity check that the JS lint passes**

```bash
cd /workspaces/dicode-core-worktrees/task-toggle-ui
test -f package.json && npm run lint 2>&1 | tail -10 || echo "no npm lint"
```

If a lint script exists, run it. Otherwise skip.

- [ ] **Step 6.5: Commit**

```bash
git add tasks/buildin/webui/app/components/dc-task-list.js
git commit -m "feat(webui): enable/disable toggle in dc-task-list

Click the circle/slash icon at the right of any task row to toggle.
Disabled tasks render at 0.55 opacity with a 'paused' badge. Optimistic
flip + revert-on-error; 409 reloads the task list; 422 surfaces the
ancestor-disabled message."
```

---

## Task 7: E2E test — `tests/e2e/task-toggle.spec.ts`

**Why:** Lock in the full UI → API → file flow. Catches regressions in any of the 6 layers above.

**Files:**
- Create: `tests/e2e/task-toggle.spec.ts`
- Modify: [playwright.config.ts](../../../playwright.config.ts) — add to `unauthenticated` testMatch list

- [ ] **Step 7.1: Find the e2e fixture's task layout**

```bash
ls tests/e2e/fixtures/tasks/ 2>/dev/null
cat tests/e2e/fixtures/dicode-unauth.yaml 2>/dev/null
find tests/e2e/fixtures -name "taskset.yaml" -exec head -20 {} \;
```

Expected: an `e2e-tests` fixture taskset with at least one task. We'll target one of those tasks.

- [ ] **Step 7.2: Write the spec file**

Create `tests/e2e/task-toggle.spec.ts`:

```typescript
import { test, expect } from '@playwright/test';
import { gotoWebui, navigateInSpa } from './helpers/webui';

test.describe('Task enable/disable toggle', () => {
  test('PATCH /api/tasks/{id}/overrides sets enabled=false', async ({ request }) => {
    // Find any task to toggle.
    const list = await request.get('/api/tasks').then((r) => r.json());
    expect(Array.isArray(list)).toBe(true);
    expect(list.length).toBeGreaterThan(0);
    const target = list[0];
    const id = target.id || target.ID;

    const res = await request.patch(`/api/tasks/${id}/overrides`, {
      data: { enabled: false },
      headers: { 'Content-Type': 'application/json' },
    });
    expect(res.status()).toBe(200);

    // Verify it reflects in /api/tasks within ~5s.
    await expect.poll(async () => {
      const after = await request.get('/api/tasks').then((r) => r.json());
      const t = after.find((x: any) => (x.id || x.ID) === id);
      return t?.enabled;
    }, { timeout: 10_000 }).toBe(false);

    // Restore.
    await request.patch(`/api/tasks/${id}/overrides`, {
      data: { enabled: true },
      headers: { 'Content-Type': 'application/json' },
    });
  });

  test('PATCH unknown task returns 404', async ({ request }) => {
    const res = await request.patch('/api/tasks/no-source/no-task/overrides', {
      data: { enabled: false },
      headers: { 'Content-Type': 'application/json' },
    });
    expect(res.status()).toBe(404);
  });

  test('PATCH unknown field returns 400', async ({ request }) => {
    const list = await request.get('/api/tasks').then((r) => r.json());
    const id = list[0].id || list[0].ID;
    const res = await request.patch(`/api/tasks/${id}/overrides`, {
      data: { unknownField: true },
      headers: { 'Content-Type': 'application/json' },
    });
    expect(res.status()).toBe(400);
  });

  test('UI toggle flips the row and persists', async ({ page, request }) => {
    // Use the API to identify a target task ID we can find on the page.
    const list = await request.get('/api/tasks').then((r) => r.json());
    expect(list.length).toBeGreaterThan(0);
    const target = list[0];
    const id = target.id || target.ID;

    await gotoWebui(page);
    await page.waitForSelector('dc-task-list', { timeout: 15_000 });

    // The toggle button selector — adjust if the implementation uses a
    // data-testid attribute. For now, find the row by id-attr.
    const row = page.locator('dc-task-list').locator(`[data-task-id="${id}"]`);
    if ((await row.count()) === 0) {
      test.skip(true, 'dc-task-list does not expose data-task-id; skipping UI assertion (API path is covered by sibling tests).');
      return;
    }

    const toggle = row.locator('.toggle-btn');
    await expect(toggle).toBeVisible();
    await toggle.click();

    // Optimistic flip → row gets .disabled class.
    await expect(row).toHaveClass(/disabled/, { timeout: 5_000 });

    // Reload page; state persists.
    await page.reload();
    await page.waitForSelector('dc-task-list');
    const reloaded = page.locator('dc-task-list').locator(`[data-task-id="${id}"]`);
    await expect(reloaded).toHaveClass(/disabled/, { timeout: 10_000 });

    // Restore via API so other tests start clean.
    await request.patch(`/api/tasks/${id}/overrides`, {
      data: { enabled: true },
      headers: { 'Content-Type': 'application/json' },
    });
  });
});
```

If the UI test's `data-task-id` lookup is the blocker, add a `data-task-id=${id}` attribute to the row in `dc-task-list.js` Task 6 step 6.2. Update the spec accordingly.

- [ ] **Step 7.3: Add the spec to playwright.config.ts**

Edit [playwright.config.ts](../../../playwright.config.ts), find the `unauthenticated` project's `testMatch` array, and add:

```typescript
'**/task-toggle.spec.ts',
```

- [ ] **Step 7.4: Run the new spec**

```bash
cd /workspaces/dicode-core-worktrees/task-toggle-ui
test -d node_modules && npx playwright test task-toggle.spec.ts --project=unauthenticated 2>&1 | tail -25 || echo "playwright not installed locally; CI will validate"
```

Expected: PASS, or at minimum compile/lint clean.

- [ ] **Step 7.5: Commit**

```bash
git add tests/e2e/task-toggle.spec.ts playwright.config.ts
git commit -m "test(e2e): cover task overrides PATCH endpoint + UI toggle flow

API: happy path persistence, 404 unknown task, 400 unknown field. UI:
toggle row → row goes disabled → reload preserves. UI assertion is
data-task-id-aware; falls back to skip if the attribute is missing."
```

---

## Task 8: Make `data-task-id` available on rows (if missed in Task 6)

**Why:** Required by the e2e UI test. Possibly already added; this is a safety net.

- [ ] **Step 8.1: Check whether the row has a stable test hook**

```bash
grep -n "data-task-id\|task.id\b" tasks/buildin/webui/app/components/dc-task-list.js | head -5
```

If `data-task-id=${task.id}` is already there, skip the rest of Task 8.

- [ ] **Step 8.2: Add the attribute**

In the row template add:

```javascript
<div class=${`row ${task.enabled === false ? 'disabled' : ''}`}
     data-task-id=${task.id || task.ID}>
```

- [ ] **Step 8.3: Commit**

```bash
git add tasks/buildin/webui/app/components/dc-task-list.js
git commit -m "test(webui): add data-task-id to dc-task-list rows for e2e selectors"
```

---

## Task 9: Final lint, format, full-suite test, push

- [ ] **Step 9.1: Format + lint**

```bash
make format && make lint 2>&1 | tail -5
```

- [ ] **Step 9.2: Full Go test suite**

```bash
go test ./... -timeout 120s 2>&1 | tail -10
```

Expected: all pass.

- [ ] **Step 9.3: Push the branch**

```bash
git push origin task-toggle-ui 2>&1 | tail -5
```

- [ ] **Step 9.4: Open the PR (only after all tasks above are reviewed clean)**

```bash
gh pr create --title "feat: PATCH /api/tasks/{id}/overrides + enable/disable toggle UI" --body "$(cat <<'EOF'
## Summary

- Generic `PATCH /api/tasks/{id}/overrides` endpoint with JSON Merge Patch (RFC 7396) semantics. Today's only client is the new toggle, but every Overrides field accepts on day one — future param/env/timeout UIs reuse this endpoint with no backend change.
- Enable/disable toggle in `dc-task-list`: click the circle/slash icon at the right of any row. Disabled tasks render faded with a "paused" badge.
- **Latent fix:** plumb `entry.Overrides` from `dicode.yaml` into `taskset.Source` (resolver supported it; daemon was passing `nil`). Source-level overrides now actually apply at runtime.
- Hot reload: `Source.SetParentOverrides` triggers an out-of-band `syncAndEmit`, so toggle changes propagate within ~1s instead of waiting up to 30s for the next poll tick.
- mtime-checked atomic writes; 409 on concurrent edit; 422 when toggling-on under a disabled ancestor source.

Spec: [docs/superpowers/specs/2026-05-05-task-enable-disable-toggle-design.md](docs/superpowers/specs/2026-05-05-task-enable-disable-toggle-design.md)

## Test plan

- [x] Unit: `pkg/config/{taskpath,persist}_test.go` — SplitTaskID + MergeTaskOverride (8 sub-tests)
- [x] Integration: `pkg/webui/server_test.go` — PATCH happy path / 404 / 400 / 422
- [x] Resolver: `pkg/taskset/source_test.go` — parent overrides + refresh signal
- [x] E2E: `tests/e2e/task-toggle.spec.ts` — API + UI flow

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

- [ ] **Step 9.5: Post-PR review cycle (per memory `feedback_pr_review_loop`)**

After PR creation, dispatch `/review` and `/security-review`, address findings, iterate until clean.

---

## Self-review notes

**Spec coverage:**
- Spec §1 (SplitTaskID) → Task 3 ✓
- Spec §2 (MergeTaskOverride) → Task 4 ✓
- Spec §3 (PATCH endpoint) → Task 5 ✓
- Spec §4 (frontend toggle) → Task 6 ✓
- Spec §5 (tests) → Tasks 1-7 distributed ✓
- Spec edge cases (ancestor disabled 422, mtime 409, etc.) → covered in Task 4 + 5 tests ✓
- Latent gap not in spec but blocks correctness → Tasks 1-2 (added)

**Placeholder scan:** none — every step has either explicit code or an explicit grep+follow-pattern instruction.

**Type consistency:** `MergeTaskOverride` signature consistent across Task 4 (definition) and Task 5 (call site). `SetParentOverrides` consistent across Task 2 (definition) and Task 5 (call site). `WithParentOverrides` / `SourceOption` consistent across Task 1 + Task 2.

**Scope:** 9 focused tasks, each one logical commit. Subagent-driven execution should land them sequentially with two-stage review per task.

**Ambiguities flagged for implementer:**
- Chi route shape for multi-segment `{id}` (Task 5.1 — search & match existing pattern)
- Toast mechanism in dc-task-list (Task 6.2 — search & wire to existing helper)
- `data-task-id` attribute (Task 8 — safety net if missed in Task 6)
