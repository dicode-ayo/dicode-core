package main

import (
	"io"
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

// TestCmdInit_RejectsDirectoryCollision guards against writeFileIfAbsent
// silently no-op'ing (and cmdInit reporting success) when a scaffold target
// like tasks/hello/task.yaml already exists as a directory rather than a
// file — that would leave a non-runnable scaffold with no diagnostic at all.
func TestCmdInit_RejectsDirectoryCollision(t *testing.T) {
	dir := t.TempDir()
	// tasks/hello/task.yaml is a directory instead of a file.
	collision := filepath.Join(dir, "tasks", "hello", "task.yaml")
	if err := os.MkdirAll(collision, 0o755); err != nil {
		t.Fatalf("seed collision dir: %v", err)
	}

	if err := cmdInit([]string{dir}); err == nil {
		t.Fatal("expected cmdInit to error on a directory collision, got nil")
	}
}

// TestCmdInit_GitignoreExactMatchOnly guards against writeGitignore treating
// a loose substring hit (e.g. a prior negation like "!.dicode/") as proof
// the runtime data dir is already ignored. gitignore semantics are
// last-match-wins, so ".dicode/" must end up as the final line regardless
// of what a pre-existing file already contains — a substring.Contains check
// would wrongly skip appending it here and leave the data dir (which holds
// the SQLite DB and, post-daemon-start, the passphrase hash) un-ignored.
func TestCmdInit_GitignoreExactMatchOnly(t *testing.T) {
	dir := t.TempDir()
	const preexisting = "node_modules/\n!.dicode/\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(preexisting), 0o644); err != nil {
		t.Fatalf("seed .gitignore: %v", err)
	}

	if err := cmdInit([]string{dir}); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.HasSuffix(string(got), ".dicode/\n") {
		t.Errorf(".gitignore = %q, want it to end with the effective (last-match-wins) rule %q", got, ".dicode/\n")
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

// wizardStdin builds stdin for the init wizard: one line per prompt, in
// order — one per curated taskset, the local tasks dir, the advanced y/N
// (plus data dir and port when that is "y"), and finally the passphrase.
func wizardStdin(lines ...string) *strings.Reader {
	return strings.NewReader(strings.Join(lines, "\n") + "\n")
}

// runInitInteractive drives runInit through the wizard with scripted answers
// and returns everything it printed.
func runInitInteractive(t *testing.T, dir string, answers ...string) string {
	t.Helper()
	var out strings.Builder
	err := runInit([]string{dir}, wizardStdin(answers...), &out, &out, true,
		func(string) string { return "" })
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}
	return out.String()
}

// TestRunInit_NonTTYKeepsSilentDefaults pins the contract that keeps `dicode
// init` usable from a script: a pipe, a CI step or a Dockerfile RUN gets the
// full scaffold with no prompt and no blocking read.
func TestRunInit_NonTTYKeepsSilentDefaults(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	if err := runInit([]string{dir}, strings.NewReader(""), &out, &out, false,
		func(string) string { return "" }); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	if strings.Contains(out.String(), "first-run setup") {
		t.Errorf("wizard ran without a TTY:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Before you push") {
		t.Errorf("default answers must not warn:\n%s", out.String())
	}

	cfg, err := config.Load(filepath.Join(dir, "dicode.yaml"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Server.Secret != "" {
		t.Errorf("Server.Secret = %q; want empty so nothing is committed", cfg.Server.Secret)
	}
	for _, f := range []string{"tasks/taskset.yaml", "tasks/hello/task.yaml", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
}

// TestRunInit_WizardDisablesPreset: an answer given at the prompt has to
// reach the generated dicode.yaml.
func TestRunInit_WizardDisablesPreset(t *testing.T) {
	dir := t.TempDir()
	out := runInitInteractive(t, dir,
		"y", // buildin
		"n", // examples off
		"y", // auth
		"",  // local tasks dir default
		"",  // advanced? no
		"",  // passphrase: auto-generate
	)
	if !strings.Contains(out, "first-run setup") {
		t.Fatalf("wizard did not run:\n%s", out)
	}

	body := readFile(t, filepath.Join(dir, "dicode.yaml"))
	if strings.Contains(body, "tasks/examples/taskset.yaml") {
		t.Errorf("examples preset was declined but still rendered:\n%s", body)
	}
	if !strings.Contains(body, "tasks/buildin/taskset.yaml") {
		t.Errorf("buildin preset was accepted but is missing:\n%s", body)
	}
}

// TestRunInit_EmptyPassphraseStaysEmpty guards the default that keeps this
// directory safe to push: pressing enter at the passphrase prompt must leave
// server.secret absent, not generate one the way the daemon's wizard does.
func TestRunInit_EmptyPassphraseStaysEmpty(t *testing.T) {
	dir := t.TempDir()
	out := runInitInteractive(t, dir, "", "", "", "", "", "")

	cfg, err := config.Load(filepath.Join(dir, "dicode.yaml"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Server.Secret != "" {
		t.Errorf("Server.Secret = %q; want empty", cfg.Server.Secret)
	}
	if strings.Contains(out, "Before you push") {
		t.Errorf("default answers must not warn:\n%s", out)
	}
}

// TestRunInit_PassphraseIsStoredAndWarned covers the "ask everything, warn on
// unsafe answers" contract: the operator may knowingly bake a passphrase in,
// but must be told that pushing the repo then publishes a working login.
func TestRunInit_PassphraseIsStoredAndWarned(t *testing.T) {
	dir := t.TempDir()
	out := runInitInteractive(t, dir, "", "", "", "", "", "hunter2-hunter2")

	cfg, err := config.Load(filepath.Join(dir, "dicode.yaml"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Server.Secret != "hunter2-hunter2" {
		t.Errorf("Server.Secret = %q; want the entered passphrase", cfg.Server.Secret)
	}
	if !strings.Contains(out, "Before you push") || !strings.Contains(out, "server.secret") {
		t.Errorf("entering a passphrase must warn:\n%s", out)
	}
}

// TestRunInit_RelocatedDataDirIsGitignored: the data dir holds the SQLite
// database (passphrase hash, task KV, encrypted secrets), so wherever the
// operator moves it inside the repo, "git add -A" must not sweep it up.
func TestRunInit_RelocatedDataDirIsGitignored(t *testing.T) {
	dir := t.TempDir()
	out := runInitInteractive(t, dir,
		"", "", "", // presets
		"",                   // local tasks dir default
		"y",                  // advanced
		"${CONFIGDIR}/state", // data dir
		"",                   // port default
		"",                   // passphrase
	)

	got := readFile(t, filepath.Join(dir, ".gitignore"))
	if !strings.HasSuffix(got, "state/\n") {
		t.Errorf(".gitignore = %q; want it to end with %q", got, "state/\n")
	}
	if strings.Contains(out, "Before you push") {
		t.Errorf("a relocated but ignorable data dir must not warn:\n%s", out)
	}
}

// TestRunInit_DataDirOutsideRepoIsNotIgnored: nothing to exclude when the
// data dir is not under the repo at all, so .gitignore stays unwritten.
func TestRunInit_DataDirOutsideRepoIsNotIgnored(t *testing.T) {
	dir := t.TempDir()
	runInitInteractive(t, dir,
		"", "", "",
		"",                              // local tasks dir default
		"y",                             // advanced
		filepath.Join(t.TempDir(), "d"), // data dir elsewhere
		"",                              // port
		"",                              // passphrase
	)

	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); !os.IsNotExist(err) {
		t.Errorf("stat .gitignore: got %v; want it not to exist", err)
	}
}

// TestRunInit_TasksDirOutsideRepoWarns: a tasks dir outside the scaffold
// still gets created, but a clone elsewhere will not find it.
func TestRunInit_TasksDirOutsideRepoWarns(t *testing.T) {
	dir := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "my-tasks")
	out := runInitInteractive(t, dir,
		"", "", "",
		elsewhere, // local tasks dir
		"",        // advanced no
		"",        // passphrase
	)

	if _, err := os.Stat(filepath.Join(elsewhere, "taskset.yaml")); err != nil {
		t.Errorf("scaffold did not follow the chosen tasks dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tasks")); !os.IsNotExist(err) {
		t.Errorf("stat tasks/: got %v; want the default location unused", err)
	}
	if !strings.Contains(out, "outside this directory") {
		t.Errorf("a tasks dir outside the repo must warn:\n%s", out)
	}
}

// TestRunInit_SkipLocalDirScaffoldsNothing: "skip" omits the local source
// from dicode.yaml, so there is no directory to scaffold either.
func TestRunInit_SkipLocalDirScaffoldsNothing(t *testing.T) {
	dir := t.TempDir()
	runInitInteractive(t, dir, "", "", "", "skip", "", "")

	if _, err := os.Stat(filepath.Join(dir, "tasks")); !os.IsNotExist(err) {
		t.Errorf("stat tasks/: got %v; want it not to exist", err)
	}
	if body := readFile(t, filepath.Join(dir, "dicode.yaml")); strings.Contains(body, "    local:") {
		t.Errorf("skipped local dir still rendered a local entry:\n%s", body)
	}
}

func TestShouldRunWizard(t *testing.T) {
	tests := []struct {
		name  string
		isTTY bool
		env   string
		want  bool
	}{
		{"tty prompts", true, "", true},
		{"no tty stays silent", false, "", false},
		{"env silent beats tty", true, "silent", false},
		{"env cli beats no tty", false, "cli", true},
		{"browser downgrades to cli", false, "BROWSER", true},
		{"unknown value falls through to tty", false, "wat", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldRunWizard(tc.isTTY, func(string) string { return tc.env }, io.Discard)
			if got != tc.want {
				t.Errorf("shouldRunWizard = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestDataDirIgnoreEntry(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name    string
		dataDir string
		want    string
	}{
		{"default", "${CONFIGDIR}/.dicode", ".dicode/\n"},
		{"nested", "${CONFIGDIR}/var/state", "var/state/\n"},
		{"absolute inside", filepath.Join(root, "inside"), "inside/\n"},
		{"outside", filepath.Join(t.TempDir(), "d"), ""},
		{"repo root itself", "${CONFIGDIR}", ""},
		{"unresolvable variable", "${DATADIR}/x", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := dataDirIgnoreEntry(root, tc.dataDir)
			if err != nil {
				t.Fatalf("dataDirIgnoreEntry: %v", err)
			}
			if got != tc.want {
				t.Errorf("dataDirIgnoreEntry(%q) = %q; want %q", tc.dataDir, got, tc.want)
			}
		})
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestRunInit_UnresolvableTasksDirWarns: ${DATADIR} is only known to the
// daemon once it has loaded the config, so init cannot create the directory.
// Saying nothing would leave a config whose local source has no taskset.yaml
// and no explanation.
func TestRunInit_UnresolvableTasksDirWarns(t *testing.T) {
	dir := t.TempDir()
	out := runInitInteractive(t, dir,
		"", "", "",
		"${DATADIR}/tasks", // local tasks dir
		"",                 // advanced no
		"",                 // passphrase
	)
	if !strings.Contains(out, "only the daemon can expand") {
		t.Errorf("want an unresolvable-tasks-dir warning:\n%s", out)
	}
	if strings.Contains(out, "outside this directory") {
		t.Errorf("unresolvable is not the same as outside:\n%s", out)
	}
}

// TestRunInit_AbsolutePathInsideRepoWarns: sitting inside the scaffold is not
// the same as travelling with it. An absolute path is committed verbatim and
// resolves, on another machine, to a directory that has nothing to do with
// where the clone landed.
func TestRunInit_AbsolutePathInsideRepoWarns(t *testing.T) {
	dir := t.TempDir()
	inside := filepath.Join(dir, "tasks")
	out := runInitInteractive(t, dir,
		"", "", "",
		inside, // local tasks dir, absolute but inside the scaffold
		"",     // advanced no
		"",     // passphrase
	)
	if !strings.Contains(out, "written into dicode.yaml verbatim") {
		t.Errorf("an absolute in-repo tasks dir must warn about portability:\n%s", out)
	}
	if strings.Contains(out, "outside this directory") {
		t.Errorf("the path is inside the scaffold; that warning is wrong:\n%s", out)
	}
}

func TestMachineSpecific(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"", false},
		{"${CONFIGDIR}/tasks", false},
		{"tasks", false},
		{"/home/someone/tasks", true},
		{"~/tasks", true},
		{"${HOME}/tasks", true},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			if got := machineSpecific(tc.value); got != tc.want {
				t.Errorf("machineSpecific(%q) = %v; want %v", tc.value, got, tc.want)
			}
		})
	}
}
