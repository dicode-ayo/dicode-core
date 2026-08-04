package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"

	"github.com/dicode/dicode/pkg/config"
)

// TestCmdInit_HappyPath scaffolds a fresh directory and checks every file
// the issue's contract requires, including that the generated dicode.yaml
// round-trips through the real config.Load path — a template that doesn't
// actually load is exactly the class of bug `dicode init` exists to avoid
// (mirrors pkg/onboarding/onboarding_test.go's TestRenderConfig_LoadsCleanly).
func TestCmdInit_HappyPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "myconfig")

	if err := cmdInit([]string{target}); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}

	cfgPath := filepath.Join(target, "dicode.yaml")
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("dicode.yaml not created: %v", err)
	}

	tsPath := filepath.Join(target, "tasks", "taskset.yaml")
	if _, err := os.Stat(tsPath); err != nil {
		t.Fatalf("tasks/taskset.yaml not created: %v", err)
	}

	helloYAML := filepath.Join(target, "tasks", "hello", "task.yaml")
	if _, err := os.Stat(helloYAML); err != nil {
		t.Fatalf("tasks/hello/task.yaml not created: %v", err)
	}
	helloJS := filepath.Join(target, "tasks", "hello", "task.js")
	if _, err := os.Stat(helloJS); err != nil {
		t.Fatalf("tasks/hello/task.js not created: %v", err)
	}

	gitignorePath := filepath.Join(target, ".gitignore")
	gitignoreBytes, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatalf(".gitignore not created: %v", err)
	}
	if !strings.Contains(string(gitignoreBytes), ".dicode/") {
		t.Errorf(".gitignore = %q, want it to contain %q", gitignoreBytes, ".dicode/")
	}

	gitDir := filepath.Join(target, ".git")
	if fi, err := os.Stat(gitDir); err != nil || !fi.IsDir() {
		t.Fatalf(".git directory not created: err=%v", err)
	}

	// The generated config must actually load through the real parser, and
	// every path in it must be ${CONFIGDIR}-relative rather than baked to
	// this temp dir's absolute path — the whole point of `dicode init` is
	// that the directory still resolves after being git-cloned elsewhere.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("generated dicode.yaml failed to load: %v", err)
	}
	if len(cfg.Spec.Entries) == 0 {
		t.Error("cfg.Spec.Entries should not be empty")
	}
	if cfg.Database.Type != "sqlite" {
		t.Errorf("cfg.Database.Type = %q, want sqlite", cfg.Database.Type)
	}

	rawYAML, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read dicode.yaml: %v", err)
	}
	if !strings.Contains(string(rawYAML), "${CONFIGDIR}") {
		t.Error("generated dicode.yaml should contain the literal ${CONFIGDIR} placeholder")
	}
	if strings.Contains(string(rawYAML), target) {
		t.Errorf("generated dicode.yaml should not contain the absolute temp path %q", target)
	}
	// Regression guard: this file is scaffolded for `git add -A && git push`
	// (see the "next steps" cmdInit prints), so it must never carry a real,
	// precedence-winning dashboard credential (pkg/webui/passphrase.go's
	// verifyPassphrase treats a non-empty server.secret as a standing
	// plaintext login compared on every request). server.secret must be the
	// empty string here — the passphrase is generated later, locally, by
	// ensurePassphrase on first `dicode daemon` start.
	if cfg.Server.Secret != "" {
		t.Errorf("cfg.Server.Secret = %q, want empty — dicode init must not bake a live credential into a git-committed file", cfg.Server.Secret)
	}
	if !strings.Contains(string(rawYAML), `secret: ""`) {
		t.Errorf("generated dicode.yaml should contain an empty server.secret, got:\n%s", rawYAML)
	}

	// The scaffolded directory is meant to be pushed to a remote — keep it
	// non-world-listable on shared hosts in the meantime (matches the
	// directory-permission intent onboarding.WriteConfig already documents
	// for the first-run wizard's own config dir).
	if fi, err := os.Stat(target); err != nil {
		t.Fatalf("stat %s: %v", target, err)
	} else if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("scaffolded dir mode = %o, want 0700", perm)
	}
}

// TestCmdInit_RefusesExistingConfig guards against clobbering an operator's
// existing dicode.yaml, and verifies the file is left byte-for-byte intact.
func TestCmdInit_RefusesExistingConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")
	const sentinel = "# my hand-written config\n"
	if err := os.WriteFile(cfgPath, []byte(sentinel), 0o644); err != nil {
		t.Fatalf("seed dicode.yaml: %v", err)
	}

	err := cmdInit([]string{dir})
	if err == nil {
		t.Fatal("expected cmdInit to refuse an existing dicode.yaml, got nil error")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v, want it to mention 'already exists'", err)
	}

	got, readErr := os.ReadFile(cfgPath)
	if readErr != nil {
		t.Fatalf("read dicode.yaml after refusal: %v", readErr)
	}
	if string(got) != sentinel {
		t.Errorf("existing dicode.yaml was modified; got %q, want %q", got, sentinel)
	}
}

// TestCmdInit_PreservesExistingTasksTree guards against the clobber this
// PR's review caught: dicode.yaml not existing only proves *that file* is
// new — tasks/ can predate it (e.g. an operator who deleted just
// dicode.yaml, or hand-created tasks/ first). scaffoldRootTaskSet must skip
// files that already exist rather than overwrite them.
func TestCmdInit_PreservesExistingTasksTree(t *testing.T) {
	dir := t.TempDir()
	helloDir := filepath.Join(dir, "tasks", "hello")
	if err := os.MkdirAll(helloDir, 0o755); err != nil {
		t.Fatalf("seed tasks/hello: %v", err)
	}
	const sentinel = "# hand-written task, do not clobber\nname: MyRealTask\n"
	taskYAMLPath := filepath.Join(helloDir, "task.yaml")
	if err := os.WriteFile(taskYAMLPath, []byte(sentinel), 0o644); err != nil {
		t.Fatalf("seed hello/task.yaml: %v", err)
	}

	if err := cmdInit([]string{dir}); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}

	got, err := os.ReadFile(taskYAMLPath)
	if err != nil {
		t.Fatalf("read hello/task.yaml after init: %v", err)
	}
	if string(got) != sentinel {
		t.Errorf("pre-existing hello/task.yaml was overwritten; got %q, want %q", got, sentinel)
	}

	// Files that genuinely didn't exist yet (task.js, taskset.yaml) must
	// still get scaffolded — this isn't a blanket "skip the whole tree".
	if _, err := os.Stat(filepath.Join(helloDir, "task.js")); err != nil {
		t.Errorf("hello/task.js should still be scaffolded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tasks", "taskset.yaml")); err != nil {
		t.Errorf("tasks/taskset.yaml should still be scaffolded: %v", err)
	}
}

// TestCmdInit_ExistingGitRepoIsFine covers running `dicode init` inside a
// directory that's already a git repo (git.ErrRepositoryAlreadyExists from
// go-git's PlainInit must be treated as a no-op, not a fatal error).
func TestCmdInit_ExistingGitRepoIsFine(t *testing.T) {
	dir := t.TempDir()
	if _, err := gogit.PlainInit(dir, false); err != nil {
		t.Fatalf("seed .git: %v", err)
	}

	if err := cmdInit([]string{dir}); err != nil {
		t.Fatalf("cmdInit should tolerate a pre-existing .git dir, got: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "dicode.yaml")); err != nil {
		t.Fatalf("dicode.yaml not created despite pre-existing .git: %v", err)
	}
}

// TestCmdInit_DefaultPathIsCurrentDir covers the no-argument form
// ("dicode init" defaults to "."), without actually chdir'ing the test
// process (which would race other parallel tests) — it drives cmdInit with
// an explicit "." after chdir'ing into a scratch dir instead, then restores
// the working directory.
func TestCmdInit_DefaultPathIsCurrentDir(t *testing.T) {
	dir := t.TempDir()
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(origWD); err != nil {
			t.Fatalf("restore wd: %v", err)
		}
	}()

	if err := cmdInit(nil); err != nil {
		t.Fatalf("cmdInit(nil): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dicode.yaml")); err != nil {
		t.Fatalf("dicode.yaml not created in cwd: %v", err)
	}
}

func TestCmdInit_TooManyArgs(t *testing.T) {
	if err := cmdInit([]string{"a", "b"}); err == nil {
		t.Fatal("expected error for more than one positional arg")
	}
}
