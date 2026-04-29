# Dev-mode branch lifecycle + on_failure_chain params + branch validator — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement child issue #236 — extend `Source.SetDevMode` to clone a per-fix workspace on a named branch (using go-git, no `git` binary), add a `pkg/taskset.ValidateBranchName` validator, extend `on_failure_chain` to accept structured `{task, params}` form with reserved-key + autonomous-at-defaults config-load errors, and add the `dev-clones-cleanup` buildin task.

**Architecture:** Three independent feature areas wired together at the SourceManager + config layers:
1. Branch validation as a pure function in `pkg/taskset` — no I/O, fully unit-tested.
2. Dev-mode clone lifecycle in `pkg/taskset/source.go` using go-git's `PlainCloneContext` (already used by `pkg/source/git`).
3. `on_failure_chain` schema extension via a polymorphic YAML type in `pkg/config` + `pkg/task`, reusing the existing engine merge point in `pkg/trigger/engine.go:FireChain`.

The dev-clones-cleanup task mirrors the proven `temp-cleanup` pattern. No new core mechanism — reuses Deno SDK calls (`get_runs`) and filesystem permissions.

**Tech Stack:** Go (go-git/v5 v5.18, modernc/sqlite), Deno (cleanup task), YAML.

**Spec reference:** [docs/superpowers/specs/2026-04-28-on-failure-auto-fix-loop-design.md](../specs/2026-04-28-on-failure-auto-fix-loop-design.md), §§ 4.4, 4.5, 4.6.3, 4.2.

---

## File Structure

**Created:**
- `pkg/taskset/branch_validate.go` — `ValidateBranchName(branch, prefix string) error` + sentinel errors. Pure function, no I/O.
- `pkg/taskset/branch_validate_test.go` — unit tests covering each rejection rule.
- `pkg/taskset/source_devmode_test.go` — clone-mode lifecycle tests using a fixture git repo.
- `pkg/config/onfailurechain.go` — `OnFailureChainSpec` type with custom `UnmarshalYAML` accepting bare string OR struct.
- `pkg/config/onfailurechain_test.go` — round-trip tests for both forms + reserved-key + autonomous-at-defaults validation.
- `pkg/trigger/engine_chain_params_test.go` — engine merge integration test.
- `tasks/buildin/dev-clones-cleanup/task.yaml`
- `tasks/buildin/dev-clones-cleanup/task.ts`
- `tasks/buildin/dev-clones-cleanup/task.test.ts`

**Modified:**
- `pkg/taskset/source.go` — add `DevModeOpts` struct, change `SetDevMode` signature (caller side updated in `pkg/webui/sources.go`).
- `pkg/taskset/spec.go` — `Overrides` struct gains nothing in this issue (FS extension is in #238).
- `pkg/config/config.go:28-30` — replace `OnFailureChain string` with `OnFailureChain OnFailureChainSpec`.
- `pkg/task/spec.go:295-297` — replace `OnFailureChain *string` with `OnFailureChain *OnFailureChainSpec`.
- `pkg/task/spec.go` — add `BranchPrefix` and related auto-fix params (or note that they live in chain.Params; **decided: chain.Params**, no schema changes here).
- `pkg/trigger/engine.go:670-677` — extend `FireChain` to read structured chain config and merge `Params` into the input.
- `pkg/trigger/engine.go` (constructor / config wiring) — accept new `OnFailureChainSpec` from config.
- `pkg/webui/sources.go:108-122,158-180` — extend `SourceManager.SetDevMode` + REST handler to accept `branch`/`base`.
- `pkg/registry/reconciler.go` or similar — verify nothing else assumes `OnFailureChain` is a string.
- `tasks/buildin/mcp/task.ts:121-135` — extend `switch_dev_mode` schema with `branch`/`base`.
- `tasks/buildin/taskset.yaml` — add `dev-clones-cleanup` entry.

**Tests reference fixtures:** existing `pkg/source/git/git_test.go` already creates a temp git repo; reuse the helper.

---

## Task 1: Branch-name validator

**Files:**
- Create: `pkg/taskset/branch_validate.go`
- Create: `pkg/taskset/branch_validate_test.go`

This is a pure function with no dependencies. Implements the rules in spec § 4.6.3.

- [ ] **Step 1: Write failing tests**

`pkg/taskset/branch_validate_test.go`:

```go
package taskset

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateBranchName_Valid(t *testing.T) {
	cases := []struct{ branch, prefix string }{
		{"fix/abc-123", "fix/"},
		{"fix/process-payment_2026-04-29", "fix/"},
		{"main", "main"}, // exact match for autonomous-mode push
		{"feature/x.y.z", "feature/"},
	}
	for _, c := range cases {
		if err := ValidateBranchName(c.branch, c.prefix); err != nil {
			t.Errorf("ValidateBranchName(%q, %q) = %v, want nil", c.branch, c.prefix, err)
		}
	}
}

func TestValidateBranchName_RejectsBadRefFormat(t *testing.T) {
	cases := []struct {
		name, branch, prefix string
	}{
		{"double-dot", "fix/abc..def", "fix/"},
		{"leading-dash", "-fix/abc", "fix/"},
		{"control-char", "fix/abc\x01def", "fix/"},
		{"caret", "fix/abc^def", "fix/"},
		{"tilde", "fix/abc~def", "fix/"},
		{"colon", "fix/abc:def", "fix/"},
		{"question", "fix/abc?def", "fix/"},
		{"asterisk", "fix/abc*def", "fix/"},
		{"open-bracket", "fix/abc[def", "fix/"},
		{"backslash", "fix/abc\\def", "fix/"},
		{"space", "fix/abc def", "fix/"},
		{"leading-slash", "/fix/abc", "fix/"},
		{"trailing-slash", "fix/abc/", "fix/"},
		{"double-slash", "fix//abc", "fix/"},
		{"trailing-lock", "fix/abc.lock", "fix/"},
		{"at-sign-only", "@", ""},
		{"empty", "", ""},
		{"dot-leading-component", "fix/.hidden", "fix/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateBranchName(c.branch, c.prefix)
			if !errors.Is(err, ErrInvalidBranchName) {
				t.Errorf("got %v, want ErrInvalidBranchName", err)
			}
		})
	}
}

func TestValidateBranchName_RejectsPrefixMismatch(t *testing.T) {
	err := ValidateBranchName("hotfix/abc", "fix/")
	if !errors.Is(err, ErrBranchPrefixMismatch) {
		t.Errorf("got %v, want ErrBranchPrefixMismatch", err)
	}
}

func TestValidateBranchName_RejectsBadPrefix(t *testing.T) {
	cases := []string{"fix/*", "fix/[a-z]", "fix/?", "fix/{x,y}", "../fix/", "fix\\"}
	for _, prefix := range cases {
		err := ValidateBranchPrefix(prefix)
		if err == nil {
			t.Errorf("ValidateBranchPrefix(%q) = nil, want error", prefix)
		}
		if !strings.Contains(err.Error(), "prefix") {
			t.Errorf("error %q does not mention 'prefix'", err)
		}
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

```
go test ./pkg/taskset/ -run TestValidateBranchName -v
```

Expected: `FAIL` with `undefined: ValidateBranchName` (and friends).

- [ ] **Step 3: Implement minimal code**

`pkg/taskset/branch_validate.go`:

```go
// Package-level validator for branch names accepted by dev-mode clone-mode
// and dicode.git.commit_push. Pure function — no I/O.
//
// Rules (spec § 4.6.3): git check-ref-format equivalent + literal-prefix
// match against the per-task branch_prefix. Glob/regex characters in the
// prefix are rejected at config-load via ValidateBranchPrefix.

package taskset

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

var (
	ErrInvalidBranchName    = errors.New("invalid branch name (git check-ref-format)")
	ErrBranchPrefixMismatch = errors.New("branch does not start with required prefix")
)

// ValidateBranchName enforces git check-ref-format rules plus a literal-prefix
// match against `prefix`. An empty prefix means "no prefix required".
func ValidateBranchName(branch, prefix string) error {
	if branch == "" {
		return fmt.Errorf("%w: empty", ErrInvalidBranchName)
	}
	if branch == "@" {
		return fmt.Errorf("%w: name '@' is not allowed", ErrInvalidBranchName)
	}
	if strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") {
		return fmt.Errorf("%w: leading/trailing slash", ErrInvalidBranchName)
	}
	if strings.HasPrefix(branch, "-") {
		return fmt.Errorf("%w: leading dash", ErrInvalidBranchName)
	}
	if strings.Contains(branch, "..") || strings.Contains(branch, "//") {
		return fmt.Errorf("%w: '..' or '//'", ErrInvalidBranchName)
	}
	if strings.HasSuffix(branch, ".lock") {
		return fmt.Errorf("%w: trailing .lock", ErrInvalidBranchName)
	}
	for _, r := range branch {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: control char", ErrInvalidBranchName)
		}
		switch r {
		case ' ', '~', '^', ':', '?', '*', '[', '\\':
			return fmt.Errorf("%w: forbidden char %q", ErrInvalidBranchName, r)
		}
		_ = unicode.IsControl // (silence unused if loop changes)
	}
	for _, comp := range strings.Split(branch, "/") {
		if strings.HasPrefix(comp, ".") {
			return fmt.Errorf("%w: component starts with '.'", ErrInvalidBranchName)
		}
	}
	if prefix != "" && !strings.HasPrefix(branch, prefix) {
		return fmt.Errorf("%w: branch %q does not start with %q", ErrBranchPrefixMismatch, branch, prefix)
	}
	return nil
}

// ValidateBranchPrefix is called at config-load on each task's branch_prefix
// to reject glob/regex constructs that would make ValidateBranchName ambiguous.
func ValidateBranchPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	for _, r := range prefix {
		switch {
		case 'a' <= r && r <= 'z',
			'A' <= r && r <= 'Z',
			'0' <= r && r <= '9',
			r == '_' || r == '.' || r == '/' || r == '-':
			// allowed
		default:
			return fmt.Errorf("invalid character %q in branch prefix; allowed: [A-Za-z0-9_./-]", r)
		}
	}
	if strings.Contains(prefix, "..") {
		return fmt.Errorf("invalid '..' in branch prefix")
	}
	return nil
}
```

- [ ] **Step 4: Run test, verify pass**

```
go test ./pkg/taskset/ -run TestValidateBranchName -v
```

Expected: all subtests `PASS`.

- [ ] **Step 5: Commit**

```bash
git add pkg/taskset/branch_validate.go pkg/taskset/branch_validate_test.go
git commit -m "feat(taskset): branch-name validator for dev-mode and commit_push

Pure function ValidateBranchName(branch, prefix) implementing
git check-ref-format equivalence plus literal-prefix matching.
ValidateBranchPrefix rejects glob/regex characters at config-load.
No I/O; called from SetDevMode and (later) git.commit_push.

Refs #236"
```

---

## Task 2: DevModeOpts struct + SetDevMode signature change (no clone behavior yet)

Refactor the existing `Source.SetDevMode(ctx, enabled, localPath)` to accept an opts struct. **No clone behavior introduced in this task** — just the API change with `LocalPath` preserved. Behavior identical to today.

**Files:**
- Modify: `pkg/taskset/source.go:121-137`
- Modify: `pkg/webui/sources.go:108-122,162-180`

- [ ] **Step 1: Write failing test for the new signature**

Add to `pkg/taskset/source_devmode_test.go` (new file):

```go
package taskset_test

import (
	"context"
	"testing"

	"github.com/dicode/dicode/pkg/taskset"
)

func TestSetDevMode_LocalPath_StillWorks(t *testing.T) {
	src := newTestTaskSetSource(t)  // existing helper or new — see Task 3
	ctx := context.Background()

	if err := src.SetDevMode(ctx, true, taskset.DevModeOpts{LocalPath: "/tmp/fixture-taskset.yaml"}); err != nil {
		t.Fatalf("enable dev-mode with localPath: %v", err)
	}
	if !src.DevMode() {
		t.Fatal("DevMode() = false after enable, want true")
	}
	if got := src.DevRootPath(); got != "/tmp/fixture-taskset.yaml" {
		t.Errorf("DevRootPath = %q, want /tmp/fixture-taskset.yaml", got)
	}

	if err := src.SetDevMode(ctx, false, taskset.DevModeOpts{}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if src.DevMode() {
		t.Fatal("DevMode() = true after disable, want false")
	}
}
```

(`newTestTaskSetSource` will be needed for Task 3 too — define it as a stub for now returning a minimal `*Source`. Pull from any existing test helper in the package.)

- [ ] **Step 2: Run test, verify fail**

```
go test ./pkg/taskset/ -run TestSetDevMode_LocalPath_StillWorks -v
```

Expected: FAIL — `taskset.DevModeOpts undefined`.

- [ ] **Step 3: Add `DevModeOpts` struct and change `SetDevMode` signature**

In `pkg/taskset/source.go`, replace lines 121-137 with:

```go
// DevModeOpts configures dev-mode activation. Either LocalPath or Branch may
// be set, not both. RunID is required when Branch is set; the engine uses it
// to scope the per-fix clone directory under ${DATADIR}/dev-clones/<source>/<runID>/.
type DevModeOpts struct {
	LocalPath string // existing: point at a user's local taskset.yaml checkout
	Branch    string // new (Task 3): create a per-fix clone on this branch
	Base      string // new (Task 3): branch to fork from when Branch is unknown remotely; defaults to source's tracked branch
	RunID     string // new (Task 3): clone-dir name component
}

// SetDevMode enables or disables dev mode for this source.
//
// Modes:
//   - enabled=true, opts.LocalPath != "" : point dev-ref resolution at the
//     given local path (existing human-dev workflow).
//   - enabled=true, opts.Branch    != "" : clone-mode (Task 3). Engine clones
//     the source's git repo into a per-fix directory on `Branch`.
//   - enabled=false : revert. If a clone exists, it is removed.
//
// Returns ErrDevModeBusy if a different clone-mode session is already active
// on this source (Task 5).
func (s *Source) SetDevMode(ctx context.Context, enabled bool, opts DevModeOpts) error {
	if opts.LocalPath != "" && opts.Branch != "" {
		return fmt.Errorf("DevModeOpts: LocalPath and Branch are mutually exclusive")
	}
	s.resolver.SetDevMode(enabled)
	s.mu.Lock()
	s.devRootPath = opts.LocalPath
	if enabled && opts.LocalPath != "" {
		s.watchRoot = filepath.Dir(opts.LocalPath)
	}
	ch := s.ch
	s.mu.Unlock()
	if ch == nil {
		return nil
	}
	return s.syncAndEmit(ctx, ch)
}
```

- [ ] **Step 4: Update callers in `pkg/webui/sources.go`**

`SourceManager.SetDevMode` (line 110) becomes:

```go
func (m *SourceManager) SetDevMode(ctx context.Context, name string, enabled bool, opts taskset.DevModeOpts) error {
	m.mu.RLock()
	src, ok := m.tasksets[name]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("source %q not found or not a taskset source", name)
	}
	m.log.Info("dev mode toggled",
		zap.String("source", name),
		zap.Bool("enabled", enabled),
		zap.String("local_path", opts.LocalPath),
		zap.String("branch", opts.Branch),
	)
	return src.SetDevMode(ctx, enabled, opts)
}
```

REST handler `apiSetDevMode` (line 162) — change body decoding:

```go
var body struct {
	Enabled   bool   `json:"enabled"`
	LocalPath string `json:"local_path"`
	Branch    string `json:"branch"`
	Base      string `json:"base"`
	RunID     string `json:"run_id"`
}
// ... existing decode + error handling ...
if err := s.sourceMgr.SetDevMode(r.Context(), name, body.Enabled, taskset.DevModeOpts{
	LocalPath: body.LocalPath,
	Branch:    body.Branch,
	Base:      body.Base,
	RunID:     body.RunID,
}); err != nil {
	jsonErr(w, err.Error(), http.StatusBadRequest)
	return
}
```

- [ ] **Step 5: Run all-package tests, verify everything still builds and passes**

```
go test ./... -timeout 60s
```

Expected: all tests pass; the new `TestSetDevMode_LocalPath_StillWorks` PASSES.

- [ ] **Step 6: Commit**

```bash
git add pkg/taskset/source.go pkg/taskset/source_devmode_test.go pkg/webui/sources.go
git commit -m "refactor(taskset): SetDevMode accepts DevModeOpts struct

Replaces (enabled, localPath) with (enabled, opts) to make room for
Branch/Base/RunID added in the next task. LocalPath behavior is
preserved bit-for-bit.

Refs #236"
```

---

## Task 3: Clone lifecycle in `Source.SetDevMode` — go-git Clone on enable

Implement `enabled=true && opts.Branch != ""`: validate branch, clone via go-git into `${DATADIR}/dev-clones/<source>/<runID>/`, set `devRootPath` to the cloned `taskset.yaml`.

**Files:**
- Modify: `pkg/taskset/source.go`
- Modify: `pkg/taskset/source_devmode_test.go` (add clone-mode test)

- [ ] **Step 1: Write failing test for clone-mode enable**

Append to `pkg/taskset/source_devmode_test.go`:

```go
func TestSetDevMode_Branch_ClonesRepo(t *testing.T) {
	// Set up a fixture remote git repo with a single commit on `main`
	// containing a `taskset.yaml`. Use the helper from pkg/source/git/git_test.go
	// or copy the inline-create pattern.
	remoteDir := newFixtureRemote(t, "main", map[string]string{
		"taskset.yaml": `apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: fixture
spec:
  entries: {}
`,
	})

	src := newTestTaskSetSource(t, taskset.SourceOpts{
		URL:    remoteDir,
		Branch: "main",
		// dataDir set to t.TempDir() so dev-clones land in a clean fixture
	})

	ctx := context.Background()
	runID := "run-test-1"
	if err := src.SetDevMode(ctx, true, taskset.DevModeOpts{
		Branch: "fix/test-1",
		Base:   "main",
		RunID:  runID,
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !src.DevMode() {
		t.Fatal("DevMode() = false after enable")
	}

	wantPath := filepath.Join(src.DataDir(), "dev-clones", src.Name(), runID, "taskset.yaml")
	if got := src.DevRootPath(); got != wantPath {
		t.Errorf("DevRootPath = %q, want %q", got, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("clone taskset.yaml missing: %v", err)
	}

	// Branch should be checked out as fix/test-1 in the local clone
	cloneDir := filepath.Dir(wantPath)
	repo, err := gogit.PlainOpen(cloneDir)
	if err != nil {
		t.Fatalf("open clone repo: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("repo.Head: %v", err)
	}
	if got := head.Name().Short(); got != "fix/test-1" {
		t.Errorf("HEAD = %q, want fix/test-1", got)
	}
}
```

- [ ] **Step 2: Run test, verify fail**

```
go test ./pkg/taskset/ -run TestSetDevMode_Branch_ClonesRepo -v
```

Expected: FAIL — clone path missing or `Branch` ignored.

- [ ] **Step 3: Implement clone-mode in `SetDevMode`**

Extend `pkg/taskset/source.go`. Add helper `enableClone` and integrate into `SetDevMode`:

```go
import (
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
)

// enableClone clones the source's git repo into ${DataDir}/dev-clones/<sourceName>/<runID>/
// and switches devRootPath to point at the cloned taskset.yaml. Branch is
// created locally from Base if it doesn't exist remotely.
func (s *Source) enableClone(ctx context.Context, opts DevModeOpts) error {
	if opts.RunID == "" {
		return fmt.Errorf("DevModeOpts.RunID required when Branch is set")
	}
	if err := ValidateBranchName(opts.Branch, ""); err != nil {
		// prefix=="" here: enforced at the higher SourceManager layer where the per-task
		// branch_prefix config is known. Local validity is enough.
		return fmt.Errorf("validate branch: %w", err)
	}
	if s.gitURL == "" {
		return fmt.Errorf("clone-mode requires a git source")
	}

	clonePath := filepath.Join(s.dataDir, "dev-clones", s.name, opts.RunID)
	if err := os.MkdirAll(filepath.Dir(clonePath), 0o755); err != nil {
		return fmt.Errorf("mkdir parent: %w", err)
	}

	cloneOpts := &gogit.CloneOptions{
		URL: s.gitURL,
		// Auth: same as the source's pull auth — wire from sc.Auth.TokenEnv.
	}
	repo, err := gogit.PlainCloneContext(ctx, clonePath, false, cloneOpts)
	if err != nil {
		return fmt.Errorf("clone: %w", err)
	}

	// Try to check out opts.Branch. If it doesn't exist on the remote, create
	// it locally from opts.Base (or the default tracked branch).
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	branchRef := plumbing.NewBranchReferenceName(opts.Branch)
	co := &gogit.CheckoutOptions{Branch: branchRef}
	if err := wt.Checkout(co); err != nil {
		// branch doesn't exist locally — create it from Base
		base := opts.Base
		if base == "" {
			base = s.trackedBranch  // populated from SourceConfig.Branch in NewSource
		}
		baseRef := plumbing.NewBranchReferenceName(base)
		baseHash, err := repo.ResolveRevision(plumbing.Revision(baseRef))
		if err != nil {
			return fmt.Errorf("resolve base %q: %w", base, err)
		}
		if err := repo.Storer.SetReference(plumbing.NewHashReference(branchRef, *baseHash)); err != nil {
			return fmt.Errorf("create branch %q: %w", opts.Branch, err)
		}
		co.Create = false
		if err := wt.Checkout(co); err != nil {
			return fmt.Errorf("checkout %q: %w", opts.Branch, err)
		}
	}

	// devRootPath points at the cloned root taskset.yaml. The resolver will
	// re-read tasks from this directory.
	s.mu.Lock()
	s.devRootPath = filepath.Join(clonePath, "taskset.yaml")
	s.cloneRunID = opts.RunID
	s.mu.Unlock()
	return nil
}
```

Update `Source` struct (top of file) to add the new fields:

```go
type Source struct {
	// existing fields...
	gitURL         string  // populated when source is git
	dataDir        string  // daemon data directory; root for dev-clones
	trackedBranch  string  // e.g. "main"
	cloneRunID     string  // non-empty while in clone-mode
}
```

Update `NewSource` (or whatever constructs `*Source`) to populate `gitURL`, `dataDir`, `trackedBranch` from `SourceConfig`.

Then plumb `enableClone` into `SetDevMode`:

```go
func (s *Source) SetDevMode(ctx context.Context, enabled bool, opts DevModeOpts) error {
	if opts.LocalPath != "" && opts.Branch != "" {
		return fmt.Errorf("DevModeOpts: LocalPath and Branch are mutually exclusive")
	}
	if enabled && opts.Branch != "" {
		if err := s.enableClone(ctx, opts); err != nil {
			return err
		}
		s.resolver.SetDevMode(true)
		s.mu.Lock()
		ch := s.ch
		s.mu.Unlock()
		if ch != nil {
			return s.syncAndEmit(ctx, ch)
		}
		return nil
	}
	// existing LocalPath / disable path (preserved from Task 2)
	// ...
}
```

- [ ] **Step 4: Run test, verify pass**

```
go test ./pkg/taskset/ -run TestSetDevMode_Branch_ClonesRepo -v
```

Expected: PASS. The fixture remote is cloned, the new branch is checked out, `DevRootPath` resolves to the cloned `taskset.yaml`.

- [ ] **Step 5: Commit**

```bash
git add pkg/taskset/source.go pkg/taskset/source_devmode_test.go
git commit -m "feat(taskset): clone-mode dev-mode (go-git PlainClone)

Source.SetDevMode with opts.Branch set clones the source's git repo
into \${DataDir}/dev-clones/<source>/<runID>/, checks out Branch
(creating it from Base if not on remote), and points devRootPath at
the cloned taskset.yaml.

Pure go-git, no \`git\` binary. Per-source concurrency check arrives
in Task 5.

Refs #236"
```

---

## Task 4: Clone lifecycle — disable removes the clone

When `enabled=false` after a clone-mode session, `os.RemoveAll` the clone directory.

**Files:**
- Modify: `pkg/taskset/source.go`
- Modify: `pkg/taskset/source_devmode_test.go`

- [ ] **Step 1: Write failing test**

Append to `pkg/taskset/source_devmode_test.go`:

```go
func TestSetDevMode_Branch_DisableRemovesClone(t *testing.T) {
	remoteDir := newFixtureRemote(t, "main", map[string]string{
		"taskset.yaml": `apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: fixture
spec:
  entries: {}
`,
	})
	src := newTestTaskSetSource(t, taskset.SourceOpts{URL: remoteDir, Branch: "main"})
	ctx := context.Background()
	runID := "run-disable-1"

	if err := src.SetDevMode(ctx, true, taskset.DevModeOpts{
		Branch: "fix/disable", Base: "main", RunID: runID,
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	clonePath := filepath.Join(src.DataDir(), "dev-clones", src.Name(), runID)
	if _, err := os.Stat(clonePath); err != nil {
		t.Fatalf("clone dir missing after enable: %v", err)
	}

	if err := src.SetDevMode(ctx, false, taskset.DevModeOpts{}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := os.Stat(clonePath); !os.IsNotExist(err) {
		t.Errorf("clone dir still exists after disable; err = %v", err)
	}
	if src.DevMode() {
		t.Error("DevMode() = true after disable")
	}
}
```

- [ ] **Step 2: Run test, verify fail**

```
go test ./pkg/taskset/ -run TestSetDevMode_Branch_DisableRemovesClone -v
```

Expected: FAIL — clone dir still present after disable.

- [ ] **Step 3: Implement disable cleanup**

Extend `SetDevMode` in `pkg/taskset/source.go`:

```go
func (s *Source) SetDevMode(ctx context.Context, enabled bool, opts DevModeOpts) error {
	// ... validation + clone-enable branch unchanged ...

	if !enabled {
		// If we were in clone-mode, remove the clone directory.
		s.mu.Lock()
		runID := s.cloneRunID
		s.cloneRunID = ""
		s.mu.Unlock()
		if runID != "" {
			clonePath := filepath.Join(s.dataDir, "dev-clones", s.name, runID)
			if err := os.RemoveAll(clonePath); err != nil {
				// Log but don't fail — orphan sweep will retry.
				s.log.Warn("dev-clones disable: removeall",
					zap.String("source", s.name),
					zap.String("path", clonePath),
					zap.Error(err),
				)
			}
		}
	}

	// ... existing path: disable resolver dev mode, clear devRootPath, sync ...
}
```

- [ ] **Step 4: Run test, verify pass**

```
go test ./pkg/taskset/ -run TestSetDevMode_Branch_DisableRemovesClone -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/taskset/source.go pkg/taskset/source_devmode_test.go
git commit -m "feat(taskset): disable removes the dev-mode clone

When SetDevMode(enabled=false) follows a clone-mode session, the
clone dir is os.RemoveAll'd. Failures are logged at WARN; orphan
sweep (dev-clones-cleanup buildin, Task 13) retries.

Refs #236"
```

---

## Task 5: Per-source clone-mode concurrency check (`ErrDevModeBusy`)

A second `SetDevMode(enabled=true, Branch=...)` on a source already in clone-mode returns `ErrDevModeBusy`.

**Files:**
- Modify: `pkg/taskset/source.go`
- Modify: `pkg/taskset/source_devmode_test.go`

- [ ] **Step 1: Write failing test**

```go
func TestSetDevMode_Branch_RefusesConcurrent(t *testing.T) {
	remoteDir := newFixtureRemote(t, "main", map[string]string{
		"taskset.yaml": `apiVersion: dicode/v1
kind: TaskSet
metadata: {name: fixture}
spec: {entries: {}}
`,
	})
	src := newTestTaskSetSource(t, taskset.SourceOpts{URL: remoteDir, Branch: "main"})
	ctx := context.Background()

	if err := src.SetDevMode(ctx, true, taskset.DevModeOpts{Branch: "fix/a", Base: "main", RunID: "a"}); err != nil {
		t.Fatalf("first enable: %v", err)
	}
	err := src.SetDevMode(ctx, true, taskset.DevModeOpts{Branch: "fix/b", Base: "main", RunID: "b"})
	if !errors.Is(err, taskset.ErrDevModeBusy) {
		t.Errorf("got %v, want ErrDevModeBusy", err)
	}
}
```

- [ ] **Step 2: Run test, verify fail**

```
go test ./pkg/taskset/ -run TestSetDevMode_Branch_RefusesConcurrent -v
```

Expected: FAIL — second call succeeds (or panics on duplicate clone path).

- [ ] **Step 3: Implement concurrency guard**

In `pkg/taskset/source.go`, add the sentinel and the check:

```go
var ErrDevModeBusy = errors.New("dev-mode clone-mode already active on this source")

// In SetDevMode, before enableClone:
if enabled && opts.Branch != "" {
	s.mu.Lock()
	busy := s.cloneRunID != ""
	s.mu.Unlock()
	if busy {
		return ErrDevModeBusy
	}
	// ... existing enableClone ...
}
```

- [ ] **Step 4: Run test, verify pass**

```
go test ./pkg/taskset/ -run TestSetDevMode_Branch -v
```

Expected: all clone-mode tests pass, including the new concurrency test.

- [ ] **Step 5: Commit**

```bash
git add pkg/taskset/source.go pkg/taskset/source_devmode_test.go
git commit -m "feat(taskset): per-source clone-mode concurrency check

Second SetDevMode(enabled=true, Branch=...) on an already-clone-mode
source returns ErrDevModeBusy. The auto-fix engine serialises via
its per-task max_concurrent guard (#238); this is defense in depth.

Refs #236"
```

---

## Task 6: REST handler accepts the new opts

Already wired through `SourceManager.SetDevMode` in Task 2; this task adds the integration test and verifies the REST surface.

**Files:**
- Modify: `pkg/webui/sources_test.go` (or create if absent)

- [ ] **Step 1: Write failing test**

```go
func TestApiSetDevMode_AcceptsBranch(t *testing.T) {
	srv, _ := newTestServer(t)  // existing helper in pkg/webui
	defer srv.Close()

	body := `{"enabled":true,"branch":"fix/test","base":"main","run_id":"r1"}`
	resp, err := http.Post(srv.URL+"/api/sources/fixture/dev", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != 200 {
		got, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; body = %s", resp.StatusCode, got)
	}
}
```

- [ ] **Step 2: Run test, verify fail**

```
go test ./pkg/webui/ -run TestApiSetDevMode_AcceptsBranch -v
```

Expected: FAIL — handler ignores `branch`/`base` (or fixture not configured for clone-mode).

- [ ] **Step 3: Verify handler decoding**

The handler change was made in Task 2. Verify by reading the handler and the test fixture; if the fixture doesn't expose a git source, configure one in the test helper (pull from `pkg/source/git/git_test.go`'s `newFixtureRemote`).

- [ ] **Step 4: Run test, verify pass**

```
go test ./pkg/webui/ -run TestApiSetDevMode -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/webui/sources_test.go
git commit -m "test(webui): apiSetDevMode accepts branch/base/run_id

Verifies the REST surface for dev-mode clone activation.

Refs #236"
```

---

## Task 7: MCP `switch_dev_mode` argument schema extension

The MCP tool currently only documents `source`, `enabled`, `local_path`. Add `branch`, `base`, `run_id` to the schema and the dispatch text.

**Files:**
- Modify: `tasks/buildin/mcp/task.ts:121-135` and the dispatcher case at line 191-203
- Modify: `tasks/buildin/mcp/task.test.ts`

- [ ] **Step 1: Write failing test**

In `tasks/buildin/mcp/task.test.ts`, add:

```ts
Deno.test("switch_dev_mode tool advertises branch arg", async () => {
  const result = await callMcp({
    method: "tools/list",
    params: {},
  });
  const tool = (result.tools as Array<{name: string; inputSchema: any}>)
    .find(t => t.name === "switch_dev_mode");
  if (!tool) throw new Error("switch_dev_mode missing");
  const props = tool.inputSchema.properties;
  if (!props.branch) throw new Error("branch property missing");
  if (!props.base)   throw new Error("base property missing");
  if (!props.run_id) throw new Error("run_id property missing");
});
```

- [ ] **Step 2: Run test, verify fail**

```
deno test --allow-read tasks/buildin/mcp/task.test.ts
```

Expected: FAIL — properties absent.

- [ ] **Step 3: Extend the schema**

In `tasks/buildin/mcp/task.ts`, replace the `switch_dev_mode` tool def:

```ts
{
  name: "switch_dev_mode",
  description:
    "Return a hint for toggling dev mode on a taskset source. The MCP client should call PATCH /api/sources/{name}/dev directly.",
  inputSchema: schema(
    {
      source: { type: "string", description: "Source name" },
      enabled: { type: "boolean", description: "true to enable" },
      local_path: {
        type: "string",
        description: "Absolute path to a local taskset.yaml (when enabling local-path mode)",
      },
      branch: {
        type: "string",
        description: "Branch name to clone-and-checkout (when enabling clone-mode). Mutually exclusive with local_path.",
      },
      base: {
        type: "string",
        description: "Branch to fork from when `branch` does not exist remotely. Defaults to source's tracked branch.",
      },
      run_id: {
        type: "string",
        description: "Identifier for the per-fix clone directory; required with branch.",
      },
    },
    ["source", "enabled"],
  ),
},
```

Update the `case "switch_dev_mode"` dispatcher to include the new fields in the body it surfaces:

```ts
case "switch_dev_mode": {
  const src = String(args.source ?? "");
  if (!src) throw new Error("source is required");
  const body: Record<string, unknown> = { enabled: Boolean(args.enabled) };
  for (const k of ["local_path", "branch", "base", "run_id"]) {
    const v = args[k];
    if (typeof v === "string" && v) body[k] = v;
  }
  return textContent(
    `Dev-mode switching is not exposed via the dicode task SDK. ` +
      `Call \`PATCH /api/sources/${src}/dev\` with body ` +
      `${JSON.stringify(body)} directly.`,
  );
}
```

- [ ] **Step 4: Run test, verify pass**

```
deno test --allow-read tasks/buildin/mcp/task.test.ts
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tasks/buildin/mcp/task.ts tasks/buildin/mcp/task.test.ts
git commit -m "feat(mcp): switch_dev_mode advertises branch/base/run_id

Schema and dispatcher updated for the dev-mode clone flow. The tool
remains a hint pointing the client at PATCH /api/sources/{name}/dev;
the new fields just round-trip through the body.

Refs #236"
```

---

## Task 8: `OnFailureChainSpec` polymorphic type

Define the new YAML-polymorphic type accepting either bare string or struct form.

**Files:**
- Create: `pkg/config/onfailurechain.go`
- Create: `pkg/config/onfailurechain_test.go`

- [ ] **Step 1: Write failing tests**

`pkg/config/onfailurechain_test.go`:

```go
package config

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOnFailureChainSpec_BareString(t *testing.T) {
	src := []byte(`on_failure_chain: auto-fix`)
	var w struct {
		OnFailureChain OnFailureChainSpec `yaml:"on_failure_chain"`
	}
	if err := yaml.Unmarshal(src, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if w.OnFailureChain.Task != "auto-fix" {
		t.Errorf("Task = %q, want auto-fix", w.OnFailureChain.Task)
	}
	if len(w.OnFailureChain.Params) != 0 {
		t.Errorf("Params = %v, want empty", w.OnFailureChain.Params)
	}
}

func TestOnFailureChainSpec_Structured(t *testing.T) {
	src := []byte(`
on_failure_chain:
  task: auto-fix
  params:
    mode: review
    max_iterations: 3
`)
	var w struct {
		OnFailureChain OnFailureChainSpec `yaml:"on_failure_chain"`
	}
	if err := yaml.Unmarshal(src, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if w.OnFailureChain.Task != "auto-fix" {
		t.Errorf("Task = %q", w.OnFailureChain.Task)
	}
	wantParams := map[string]any{"mode": "review", "max_iterations": 3}
	if !reflect.DeepEqual(w.OnFailureChain.Params, wantParams) {
		t.Errorf("Params = %v, want %v", w.OnFailureChain.Params, wantParams)
	}
}

func TestOnFailureChainSpec_Empty(t *testing.T) {
	src := []byte(`{}`)
	var w struct {
		OnFailureChain OnFailureChainSpec `yaml:"on_failure_chain"`
	}
	if err := yaml.Unmarshal(src, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if w.OnFailureChain.Task != "" {
		t.Errorf("Task = %q, want empty", w.OnFailureChain.Task)
	}
}
```

- [ ] **Step 2: Run test, verify fail**

```
go test ./pkg/config/ -run TestOnFailureChainSpec -v
```

Expected: FAIL — `OnFailureChainSpec undefined`.

- [ ] **Step 3: Implement the type**

`pkg/config/onfailurechain.go`:

```go
package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// OnFailureChainSpec configures a chained-task fire-on-failure target.
// Accepts either a bare string (task ID) or a structured form with params.
//
//	on_failure_chain: auto-fix
//	# OR
//	on_failure_chain:
//	  task: auto-fix
//	  params: {mode: review}
type OnFailureChainSpec struct {
	Task   string         `yaml:"task"`
	Params map[string]any `yaml:"params,omitempty"`
}

func (s *OnFailureChainSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var bare string
		if err := value.Decode(&bare); err != nil {
			return err
		}
		s.Task = bare
		s.Params = nil
		return nil
	case yaml.MappingNode:
		type plain OnFailureChainSpec
		var p plain
		if err := value.Decode(&p); err != nil {
			return err
		}
		*s = OnFailureChainSpec(p)
		return nil
	default:
		return fmt.Errorf("on_failure_chain must be a string or mapping, got %v", value.Tag)
	}
}

// IsZero reports whether no chain is configured.
func (s OnFailureChainSpec) IsZero() bool { return s.Task == "" }
```

- [ ] **Step 4: Run test, verify pass**

```
go test ./pkg/config/ -run TestOnFailureChainSpec -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/config/onfailurechain.go pkg/config/onfailurechain_test.go
git commit -m "feat(config): OnFailureChainSpec accepts bare string or struct

Polymorphic YAML type with custom UnmarshalYAML. Backwards-compatible
with the existing 'on_failure_chain: <task-id>' form; adds the
structured form with params for chain-trigger parameter passing.

Refs #236"
```

---

## Task 9: Switch consumers from `string` to `OnFailureChainSpec`

Replace `OnFailureChain string` (defaults) and `*string` (per-task) with the new type.

**Files:**
- Modify: `pkg/config/config.go:28-30`
- Modify: `pkg/task/spec.go:295-297`
- Modify: `pkg/trigger/engine.go` (constructor + FireChain)

- [ ] **Step 1: Update config & task spec types**

`pkg/config/config.go:28-30`:

```go
// OnFailureChain is the chain target to fire whenever any task fails.
// Accepts a bare task ID or a structured `{task, params}` form.
// Per-task on_failure_chain field can override or disable this.
OnFailureChain OnFailureChainSpec `yaml:"on_failure_chain,omitempty"`
```

`pkg/task/spec.go:295-297`:

```go
// OnFailureChain overrides the global defaults.on_failure_chain for this task.
// Set to {task: ""} to disable the global default for this task only.
OnFailureChain *OnFailureChainSpec `yaml:"on_failure_chain,omitempty" json:"on_failure_chain,omitempty"`
```

This means `pkg/task/spec.go` needs to import `pkg/config`. Avoid circular imports by moving `OnFailureChainSpec` to a neutral package — recommended: keep it in `pkg/task` (since task spec already imports its own helpers and pkg/config imports pkg/task in some places). Decide based on the actual import graph.

**If pkg/config currently imports pkg/task** (likely), put `OnFailureChainSpec` in `pkg/task` and re-export from `pkg/config`. Move the file accordingly.

- [ ] **Step 2: Update engine to consume the new type**

`pkg/trigger/engine.go`:

- Wherever `defaultsOnFailureChain string` was, change to `defaultsOnFailureChain OnFailureChainSpec` (alias the type).
- Wherever the engine was passed `cfg.Defaults.OnFailureChain` as a string, pass the struct.
- Update `SetDefaultsOnFailureChain` accordingly.

- [ ] **Step 3: Build the whole tree, fix downstream type errors**

```
go build ./...
```

Expected: a few sites complain. Fix them by reading the diagnostic — most will be `string(...)` conversions or accessing `.Task` on the struct.

Likely sites:
- `pkg/registry/reconciler.go` — if it inspects per-task `OnFailureChain`.
- WebUI handlers that surface the field.

- [ ] **Step 4: Run all tests**

```
go test ./... -timeout 60s
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "refactor: replace OnFailureChain string with OnFailureChainSpec

Defaults.OnFailureChain and Task.OnFailureChain now carry the
structured spec. Bare-string YAML continues to parse correctly via
OnFailureChainSpec.UnmarshalYAML.

Refs #236"
```

---

## Task 10: Engine merge — `chain.Params` flow into the chained run's input

Extend `FireChain` to merge `chain.Params` into the input map after the reserved engine keys.

**Files:**
- Modify: `pkg/trigger/engine.go:670-677`
- Create: `pkg/trigger/engine_chain_params_test.go`

- [ ] **Step 1: Write failing test**

```go
package trigger_test

import (
	"context"
	"sync"
	"testing"

	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/trigger"
)

func TestFireChain_MergesParams(t *testing.T) {
	eng, fakeRuntime := newTestEngine(t)  // existing or new helper
	ctx := context.Background()

	// Register the chain target task with no special trigger.
	target := &task.Spec{ID: "auto-fix" /* ... */}
	eng.RegisterForTest(target)

	// Configure defaults.on_failure_chain = {task: auto-fix, params: {mode: "review"}}
	eng.SetDefaultsOnFailureChain(task.OnFailureChainSpec{
		Task:   "auto-fix",
		Params: map[string]any{"mode": "review"},
	})

	// Synchronisation: capture the input map of the next fired run.
	var captured map[string]any
	var wg sync.WaitGroup
	wg.Add(1)
	fakeRuntime.OnFire = func(spec *task.Spec, opts trigger.RunOptions) {
		if spec.ID == "auto-fix" {
			captured = opts.Input.(map[string]any)
			wg.Done()
		}
	}

	// Simulate a failed run.
	eng.FireChain(ctx, "user-task", "run-1", "failure", "boom")
	wg.Wait()

	if captured["taskID"] != "user-task" {
		t.Errorf("taskID = %v", captured["taskID"])
	}
	if captured["runID"] != "run-1" {
		t.Errorf("runID = %v", captured["runID"])
	}
	if captured["status"] != "failure" {
		t.Errorf("status = %v", captured["status"])
	}
	if captured["mode"] != "review" {
		t.Errorf("mode = %v, want review", captured["mode"])
	}
}
```

- [ ] **Step 2: Run, verify fail**

```
go test ./pkg/trigger/ -run TestFireChain_MergesParams -v
```

Expected: FAIL — params not in captured input.

- [ ] **Step 3: Implement merge**

`pkg/trigger/engine.go:670-677` becomes:

```go
if targetID != "" && targetID != completedTaskID {
	if targetSpec, ok := e.registry.Get(targetID); ok {
		input := map[string]any{
			"taskID": completedTaskID,
			"runID":  runID,
			"status": runStatus,
			"output": output,
		}
		// Merge user-provided chain params. Reserved keys are guarded by
		// config-load validation (Task 11), so direct assignment is safe.
		for k, v := range chainParams {
			input[k] = v
		}
		e.log.Info("on_failure_chain trigger",
			zap.String("from", completedTaskID),
			zap.String("to", targetID),
			zap.String("run", runID),
		)
		go e.fireAsync(ctx, targetSpec, pkgruntime.RunOptions{Input: input}, "chain")
	}
}
```

`chainParams` comes from the resolved `OnFailureChainSpec.Params`:

```go
// Resolve which spec applies (per-task overrides defaults).
chainSpec := e.defaultsOnFailureChain
if failedSpec, ok := e.registry.Get(completedTaskID); ok && failedSpec.OnFailureChain != nil {
	chainSpec = *failedSpec.OnFailureChain
}
targetID := chainSpec.Task
chainParams := chainSpec.Params
```

- [ ] **Step 4: Run, verify pass**

```
go test ./pkg/trigger/ -run TestFireChain -v
```

Expected: PASS, including the existing `engine_failure_chain_test.go` tests.

- [ ] **Step 5: Commit**

```bash
git add pkg/trigger/engine.go pkg/trigger/engine_chain_params_test.go
git commit -m "feat(trigger): chain params flow into chained-run input

FireChain now merges OnFailureChainSpec.Params into the chained run's
input map after the reserved keys (taskID/runID/status/output). Per-task
on_failure_chain fully replaces the defaults' value.

Refs #236"
```

---

## Task 11: Config-load validation — reserved keys + autonomous-at-defaults + branch_prefix

Three hard errors at config-load time.

**Files:**
- Modify: `pkg/config/config.go` (add `Validate()` method or extend existing)
- Modify: `pkg/config/onfailurechain.go` (add `ValidateAtDefaults` and `Validate` methods)
- Modify: `pkg/config/onfailurechain_test.go`

- [ ] **Step 1: Write failing tests**

Append to `pkg/config/onfailurechain_test.go`:

```go
func TestValidate_ReservedKeyCollision(t *testing.T) {
	cases := []string{"taskID", "runID", "status", "output", "_chain_depth"}
	for _, key := range cases {
		t.Run(key, func(t *testing.T) {
			s := OnFailureChainSpec{
				Task:   "auto-fix",
				Params: map[string]any{key: "x"},
			}
			err := s.Validate()
			if err == nil || !strings.Contains(err.Error(), "reserved") {
				t.Errorf("got %v, want reserved-key error", err)
			}
		})
	}
}

func TestValidate_AutonomousAtDefaultsRejected(t *testing.T) {
	s := OnFailureChainSpec{
		Task:   "auto-fix",
		Params: map[string]any{"mode": "autonomous"},
	}
	err := s.ValidateAtDefaults()
	if err == nil || !strings.Contains(err.Error(), "autonomous") {
		t.Errorf("got %v, want autonomous-at-defaults error", err)
	}
}

func TestValidate_AutonomousPerTaskAccepted(t *testing.T) {
	s := OnFailureChainSpec{
		Task:   "auto-fix",
		Params: map[string]any{"mode": "autonomous"},
	}
	if err := s.Validate(); err != nil {
		t.Errorf("per-task autonomous rejected: %v", err)
	}
}
```

- [ ] **Step 2: Run, verify fail**

```
go test ./pkg/config/ -run TestValidate -v
```

Expected: FAIL.

- [ ] **Step 3: Implement validators**

In `pkg/config/onfailurechain.go`:

```go
var reservedChainParamKeys = map[string]struct{}{
	"taskID": {}, "runID": {}, "status": {}, "output": {}, "_chain_depth": {},
}

// Validate runs at every site (defaults + per-task) and checks reserved-key
// collisions only.
func (s OnFailureChainSpec) Validate() error {
	for k := range s.Params {
		if _, reserved := reservedChainParamKeys[k]; reserved {
			return fmt.Errorf("on_failure_chain.params: %q is a reserved key (used by the engine)", k)
		}
	}
	return nil
}

// ValidateAtDefaults runs only at the defaults.on_failure_chain site and
// additionally rejects mode: autonomous (must be opted into per-task).
func (s OnFailureChainSpec) ValidateAtDefaults() error {
	if err := s.Validate(); err != nil {
		return err
	}
	if mode, ok := s.Params["mode"].(string); ok && mode == "autonomous" {
		return fmt.Errorf(
			"defaults.on_failure_chain.params.mode: %q is not allowed at the defaults level; "+
				"opt each task in via task.yaml on_failure_chain.params.mode (and ensure branch protection)", mode)
	}
	return nil
}
```

Wire into `pkg/config/config.go`'s top-level config validator (find where the existing config is validated; if no validator yet, add one called from `Load`):

```go
// In Config.Validate (or equivalent) — run at config load.
if err := c.Defaults.OnFailureChain.ValidateAtDefaults(); err != nil {
	return fmt.Errorf("defaults: %w", err)
}
```

For per-task validation, integrate into `pkg/task/spec.go`'s spec validator — search for where the task spec is validated post-parse and add:

```go
if s.OnFailureChain != nil {
	if err := s.OnFailureChain.Validate(); err != nil {
		return fmt.Errorf("task %q on_failure_chain: %w", s.ID, err)
	}
}
```

For `branch_prefix` validation: this lives on the auto-fix taskset override, so it isn't validated until the taskset is resolved. Add a check in the resolver — when an entry's resolved params include `branch_prefix`, call `taskset.ValidateBranchPrefix(prefix)`. Defer concrete wiring to issue #238 (auto-fix override block); for #236 just make the validator function available (Task 1 covered this).

- [ ] **Step 4: Run, verify pass**

```
go test ./pkg/config/ -run TestValidate -v
```

Expected: PASS.

- [ ] **Step 5: Add an integration test for config-load**

Create or extend a test that loads a `dicode.yaml` with `defaults.on_failure_chain.params.mode: autonomous` and verifies the daemon refuses to start. Locate in `pkg/config/config_test.go`:

```go
func TestConfigLoad_RejectsAutonomousAtDefaults(t *testing.T) {
	yamlSrc := []byte(`
defaults:
  on_failure_chain:
    task: auto-fix
    params: {mode: autonomous}
`)
	_, err := LoadFromBytes(yamlSrc)
	if err == nil {
		t.Fatal("LoadFromBytes accepted autonomous-at-defaults; want error")
	}
	if !strings.Contains(err.Error(), "autonomous") {
		t.Errorf("error = %v; want mention of autonomous", err)
	}
}
```

Adapt the loader function name to whatever exists in `pkg/config`.

- [ ] **Step 6: Run all `pkg/config` tests**

```
go test ./pkg/config/ -v
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add pkg/config/onfailurechain.go pkg/config/onfailurechain_test.go pkg/config/config.go pkg/config/config_test.go pkg/task/spec.go
git commit -m "feat(config): hard-error validation for on_failure_chain

Reserved-key collision (taskID/runID/status/output/_chain_depth) is
a config-load error. mode: autonomous at the defaults level is
rejected with a message directing users to per-task opt-in.
Per-task on_failure_chain still accepts mode: autonomous.

Refs #236"
```

---

## Task 12: Per-task `on_failure_chain` fully replaces defaults

Document and enforce that per-task `on_failure_chain` is a full replacement (no deep-merge).

**Files:**
- Modify: `pkg/trigger/engine_chain_params_test.go`
- Verify: existing FireChain logic already does full-replace at the spec level (it picks `failedSpec.OnFailureChain` outright when set). This task adds an integration test confirming the behavior.

- [ ] **Step 1: Write failing/confirming test**

```go
func TestFireChain_PerTaskFullyReplacesDefaults(t *testing.T) {
	eng, fakeRuntime := newTestEngine(t)
	ctx := context.Background()
	eng.RegisterForTest(&task.Spec{ID: "auto-fix"})
	eng.RegisterForTest(&task.Spec{ID: "different-handler"})

	// Defaults: auto-fix with params.mode = review.
	eng.SetDefaultsOnFailureChain(task.OnFailureChainSpec{
		Task:   "auto-fix",
		Params: map[string]any{"mode": "review", "max_iterations": 5},
	})

	// Per-task: redirect to different-handler with no params.
	failed := &task.Spec{
		ID:             "user-task",
		OnFailureChain: &task.OnFailureChainSpec{Task: "different-handler"},
	}
	eng.RegisterForTest(failed)

	var captured map[string]any
	var firedSpec *task.Spec
	var wg sync.WaitGroup
	wg.Add(1)
	fakeRuntime.OnFire = func(spec *task.Spec, opts trigger.RunOptions) {
		firedSpec = spec
		captured = opts.Input.(map[string]any)
		wg.Done()
	}

	eng.FireChain(ctx, "user-task", "run-x", "failure", "boom")
	wg.Wait()

	if firedSpec.ID != "different-handler" {
		t.Errorf("fired %q, want different-handler", firedSpec.ID)
	}
	// Defaults' params (mode, max_iterations) MUST NOT appear:
	if _, ok := captured["mode"]; ok {
		t.Errorf("defaults' mode leaked into per-task chain")
	}
	if _, ok := captured["max_iterations"]; ok {
		t.Errorf("defaults' max_iterations leaked into per-task chain")
	}
}
```

- [ ] **Step 2: Run, verify pass (the engine logic from Task 10 should already do this)**

```
go test ./pkg/trigger/ -run TestFireChain_PerTaskFullyReplacesDefaults -v
```

Expected: PASS. If it fails, the merge in Task 10 was implemented incorrectly — fix to use full-replace selection (`failedSpec.OnFailureChain` wholly replaces `defaultsOnFailureChain`).

- [ ] **Step 3: Commit**

```bash
git add pkg/trigger/engine_chain_params_test.go
git commit -m "test(trigger): per-task on_failure_chain fully replaces defaults

Verifies no deep-merge; defaults' params do not bleed into a per-task
chain that omits them.

Refs #236"
```

---

## Task 13: `dev-clones-cleanup` buildin task

Cron task mirroring `temp-cleanup`. Lists `${DATADIR}/dev-clones/<source>/<runID>/` dirs, cross-checks against running auto-fix runs via `dicode.get_runs`, removes orphans.

**Files:**
- Create: `tasks/buildin/dev-clones-cleanup/task.yaml`
- Create: `tasks/buildin/dev-clones-cleanup/task.ts`
- Create: `tasks/buildin/dev-clones-cleanup/task.test.ts`
- Modify: `tasks/buildin/taskset.yaml` (add entry)

- [ ] **Step 1: Write failing test**

`tasks/buildin/dev-clones-cleanup/task.test.ts`:

```ts
import { assertEquals } from "https://deno.land/std@0.224.0/assert/mod.ts";
import main from "./task.ts";
import { mockDicodeSdk } from "../../sdk-test.ts";

Deno.test("removes orphan clone dirs", async () => {
  const tmp = await Deno.makeTempDir();
  const sourceName = "fixture";
  const runIDOrphan = "run-orphan-1";
  const runIDActive = "run-active-1";
  await Deno.mkdir(`${tmp}/dev-clones/${sourceName}/${runIDOrphan}`, { recursive: true });
  await Deno.mkdir(`${tmp}/dev-clones/${sourceName}/${runIDActive}`, { recursive: true });

  // Active run is still running; orphan is not.
  const sdk = mockDicodeSdk({
    get_runs: async (taskID: string) => {
      if (taskID === "auto-fix") {
        return [{ ID: runIDActive, Status: "running" }];
      }
      return [];
    },
  });

  await main({ input: {}, params: { data_dir: tmp }, dicode: sdk } as any);

  // Orphan removed:
  let orphanRemoved = false;
  try { await Deno.stat(`${tmp}/dev-clones/${sourceName}/${runIDOrphan}`); }
  catch { orphanRemoved = true; }
  assertEquals(orphanRemoved, true);

  // Active retained:
  await Deno.stat(`${tmp}/dev-clones/${sourceName}/${runIDActive}`);
});
```

(`mockDicodeSdk` should already exist in `tasks/sdk-test.ts`; if not, define a minimal one inline.)

- [ ] **Step 2: Run, verify fail**

```
deno test --allow-read --allow-write tasks/buildin/dev-clones-cleanup/task.test.ts
```

Expected: FAIL — task.ts doesn't exist yet.

- [ ] **Step 3: Implement `task.yaml`**

`tasks/buildin/dev-clones-cleanup/task.yaml`:

```yaml
apiVersion: dicode/v1
kind: Task
name: "Dev-clone Orphan Sweep"
description: |
  Removes per-fix dev-mode clone directories whose run ID is no longer in
  the set of currently-running auto-fix runs. Mirrors temp-cleanup's
  pattern. No `git` binary involved — clones are plain dirs from the
  daemon's perspective; rm -rf is sufficient.

runtime: deno

trigger:
  cron: "*/15 * * * *"

params:
  data_dir:
    type: string
    default: "${DATADIR}"
    description: "Daemon data dir whose dev-clones/ subtree to sweep."

permissions:
  fs:
    - path: "${DATADIR}/dev-clones"
      permission: rw
  dicode:
    list_tasks: true
    get_runs: true

timeout: 60s

notify:
  on_success: false
  on_failure: true
```

- [ ] **Step 4: Implement `task.ts`**

`tasks/buildin/dev-clones-cleanup/task.ts`:

```ts
// Sweeps orphaned dev-mode clone directories.
//
// Layout: ${DATADIR}/dev-clones/<sourceName>/<runID>/
// A clone is orphan iff its <runID> is not in the set of currently-running
// auto-fix runs. Files/dirs that don't fit the layout are left alone.

interface Run {
  ID: string;
  Status: string;
}

const AUTO_FIX_TASK_IDS = ["auto-fix", "auto-fix-review", "auto-fix-autonomous"];

async function collectActiveRunIDs(dicode: Dicode): Promise<Set<string>> {
  const active = new Set<string>();
  for (const id of AUTO_FIX_TASK_IDS) {
    let runs: Run[] = [];
    try {
      runs = (await dicode.get_runs(id, { limit: 100 })) as Run[];
    } catch {
      // Task may not be registered (auto-fix not yet shipped); skip.
      continue;
    }
    for (const r of runs) {
      if (r.Status === "running") active.add(r.ID);
    }
  }
  return active;
}

export default async function main({ params, dicode }: DicodeSdk) {
  const dataDir = String(params?.data_dir ?? "");
  if (!dataDir) {
    dicode.log.error("data_dir param empty");
    return { ok: false, error: "data_dir empty" };
  }
  const root = `${dataDir}/dev-clones`;
  const active = await collectActiveRunIDs(dicode);

  let removed = 0;
  let kept = 0;

  let sourceEntries: Deno.DirEntry[] = [];
  try {
    for await (const entry of Deno.readDir(root)) {
      sourceEntries.push(entry);
    }
  } catch (e) {
    if (e instanceof Deno.errors.NotFound) {
      return { ok: true, removed: 0, kept: 0 };
    }
    throw e;
  }

  for (const sourceEntry of sourceEntries) {
    if (!sourceEntry.isDirectory) continue;
    const sourcePath = `${root}/${sourceEntry.name}`;
    for await (const runEntry of Deno.readDir(sourcePath)) {
      if (!runEntry.isDirectory) continue;
      const clonePath = `${sourcePath}/${runEntry.name}`;
      if (active.has(runEntry.name)) {
        kept++;
        continue;
      }
      try {
        await Deno.remove(clonePath, { recursive: true });
        removed++;
        dicode.log.info(`removed orphan clone ${clonePath}`);
      } catch (e) {
        dicode.log.warn(`failed to remove ${clonePath}: ${e}`);
      }
    }
  }

  return { ok: true, removed, kept };
}
```

- [ ] **Step 5: Run test, verify pass**

```
deno test --allow-read --allow-write tasks/buildin/dev-clones-cleanup/task.test.ts
```

Expected: PASS.

- [ ] **Step 6: Add taskset entry**

Edit `tasks/buildin/taskset.yaml`. Insert between `temp-cleanup` and `ai-agent`:

```yaml
    dev-clones-cleanup:
      ref:
        path: ./dev-clones-cleanup/task.yaml
```

- [ ] **Step 7: Commit**

```bash
git add tasks/buildin/dev-clones-cleanup/ tasks/buildin/taskset.yaml
git commit -m "feat(buildin): dev-clones-cleanup cron task

Mirrors temp-cleanup: lists \${DATADIR}/dev-clones/<src>/<runID>/
dirs, cross-checks against running auto-fix runs via
dicode.get_runs, removes orphans via Deno.remove. No git binary.

Refs #236"
```

---

## Self-review checklist

- [ ] **Spec coverage** (§ 4.4, § 4.5, § 4.6.3, § 4.2):
  - [x] §4.4 Dev mode `branch` lifecycle → Tasks 2-5
  - [x] §4.4 REST + MCP extensions → Tasks 6, 7
  - [x] §4.5 `on_failure_chain` structured form → Tasks 8, 9, 10
  - [x] §4.5 Reserved-key collision → Task 11
  - [x] §4.5 autonomous-at-defaults rejection → Task 11
  - [x] §4.5 per-task fully replaces defaults → Task 12
  - [x] §4.6.3 branch validator → Task 1
  - [x] §4.6.3 `branch_prefix` glob/regex rejection → Task 1 (`ValidateBranchPrefix`); wired to auto-fix override in #238
  - [x] §4.2 dev-clones-cleanup buildin → Task 13
  - [x] §4.4 stale-pin sweep at startup is *NOT* in this issue — it lives in #233 (the persistence block introduces `input_pinned`)
- [ ] **Placeholder scan:** none
- [ ] **Type consistency:**
  - `OnFailureChainSpec` defined in Task 8, consumed in Tasks 9-12 with consistent field names (`Task`, `Params`).
  - `DevModeOpts` introduced in Task 2 with `LocalPath`, `Branch`, `Base`, `RunID`; same shape in Tasks 3-7.
  - `ValidateBranchName(branch, prefix)` and `ValidateBranchPrefix(prefix)` from Task 1; called from Task 3 (`enableClone`) and from Task 11 (config-load, deferred to #238).
  - `ErrDevModeBusy`, `ErrInvalidBranchName`, `ErrBranchPrefixMismatch` defined once each.
- [ ] **No spec-out-of-scope creep:**
  - Engine chain-depth enforcement, push refspec scoping, cooldowns, storm circuit-breaker → all in #238.
  - Auto-fix override entry in `tasks/buildin/taskset.yaml` → #238.
  - `permissions.fs` extension on `Overrides` struct → #238.
  - `dicode.git.commit_push` SDK call → #234.
  - Run-input persistence + `input_pinned` schema column + stale-pin sweep → #233.

---

## Verification before marking complete

After all 13 tasks ship:

- [ ] `go test ./... -timeout 60s` — green
- [ ] `make lint` — clean
- [ ] `deno test --allow-read --allow-write tasks/buildin/dev-clones-cleanup/task.test.ts` — green
- [ ] Manual smoke: start `dicode daemon` against a test source; `curl -X PATCH .../api/sources/<name>/dev` with body `{"enabled":true,"branch":"fix/smoke","base":"main","run_id":"smoke-1"}` → clone dir appears under `${DATADIR}/dev-clones/<name>/smoke-1/`; `curl -X PATCH .../dev` with `{"enabled":false}` → clone dir gone.
- [ ] `dicode.yaml` with `defaults.on_failure_chain.params.mode: autonomous` → daemon refuses to start with a clear error.
- [ ] `dicode.yaml` with `defaults.on_failure_chain: auto-fix` (bare string) → daemon starts; the chained run sees no extra params (backward-compat).
