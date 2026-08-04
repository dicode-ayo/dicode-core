package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"

	"github.com/dicode/dicode/pkg/onboarding"
)

// dataDirName is the runtime data dir dicode's generated config points at
// (SQLite DB, logs, the dashboard passphrase's home). It lives inside the
// scaffolded repo so the whole taskset directory is self-contained, which is
// exactly why it must be .gitignored — see writeGitignore.
const dataDirName = ".dicode"

const initUsage = `Usage: dicode init [path]

Scaffolds a new, git-versionable "root taskset" directory: a dicode.yaml
plus a tasks/ folder with one example task, ready to commit and push to
your own repo. No daemon or existing config is required.

  dicode init ~/dicode
  cd ~/dicode
  git remote add origin git@github.com:me/my-dicode-config
  git add -A && git commit -m "init"
  git push -u origin main

[path] defaults to "." (the current directory) if omitted.

Every path written into the generated dicode.yaml is ${CONFIGDIR}-relative,
so the directory keeps working after a fresh "git clone" onto a different
machine or a different home directory.

Refuses to run if <path>/dicode.yaml already exists, to avoid clobbering an
operator's existing config.`

// cmdInit implements `dicode init [path]`. It is daemon-free: like deno/
// python/relock/webhook above, it only touches the filesystem (plus an
// in-process go-git init), so it runs before ensureDaemon is ever reached.
func cmdInit(args []string) error {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			fmt.Fprintln(os.Stderr, initUsage)
			return nil
		}
	}
	if len(args) > 1 {
		return fmt.Errorf("usage: dicode init [path]")
	}

	path := "."
	if len(args) == 1 {
		path = args[0]
	}

	// 0o700: this directory is meant to be committed to git and pushed to a
	// remote (see the "next steps" printed below) — keep it non-world-
	// listable on shared hosts in the meantime, matching the intent already
	// documented on onboarding.WriteConfig's own MkdirAll.
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}

	configPath := filepath.Join(path, "dicode.yaml")
	if _, err := os.Stat(configPath); err == nil {
		return fmt.Errorf("dicode.yaml already exists at %s — remove it or pick a different path", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", configPath, err)
	}

	enabled := make(map[string]bool, len(onboarding.TaskSetPresets))
	for _, p := range onboarding.TaskSetPresets {
		enabled[p.Name] = true
	}
	result := onboarding.Result{
		TaskSetsEnabled: enabled,
		// ${CONFIGDIR}-relative (not home-relative like the first-run
		// wizard's default) so this directory still resolves correctly
		// after it's git-cloned onto another machine.
		LocalTasksDir: "${CONFIGDIR}/tasks",
		DataDir:       "${CONFIGDIR}/" + dataDirName,
		Port:          8080, // matches onboarding's own default (pkg/onboarding/cli.go defaultPort)
		// Deliberately empty: unlike the first-run wizard (which never
		// leaves the local machine), this directory is meant to be
		// git-committed and pushed to a remote — see the "next steps"
		// printed below. onboarding.RenderConfig writes Passphrase verbatim
		// as server.secret, and pkg/webui/passphrase.go's verifyPassphrase
		// treats a non-empty server.secret as an unhashed, precedence-
		// winning plaintext credential compared on every login. Baking a
		// real one in here would mean `git push` ships a working dashboard
		// password to the remote, recoverable from history forever even
		// after later removal. Leaving it empty means no YAML override, so
		// server.auth's own ensurePassphrase auto-generates a passphrase,
		// stores only its bcrypt hash in the (gitignored) local database,
		// and prints the plaintext once — on first `dicode daemon` start,
		// not here, and never into a file this command tells the operator
		// to commit.
		Passphrase: "",
	}

	if err := onboarding.WriteConfig(configPath, onboarding.RenderConfig(result)); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}

	tasksDir := filepath.Join(path, "tasks")
	if err := scaffoldRootTaskSet(tasksDir); err != nil {
		return err
	}

	if err := writeGitignore(path); err != nil {
		return err
	}

	if _, err := gogit.PlainInit(path, false); err != nil {
		if errors.Is(err, gogit.ErrRepositoryAlreadyExists) {
			fmt.Fprintln(os.Stdout, "note: a .git repository already exists at", path, "— leaving it as-is")
		} else {
			return fmt.Errorf("git init %s: %w", path, err)
		}
	}

	fmt.Println("━━━ dicode init complete ━━━")
	fmt.Println("Config written to", configPath)
	fmt.Println()
	fmt.Println("No dashboard passphrase was generated here — this directory is meant to be")
	fmt.Println("committed to git, and dicode.yaml has no server.secret to keep it that way.")
	fmt.Println("The first time you run `dicode daemon` (or `make run`) in this directory,")
	fmt.Println("a passphrase is generated automatically, its hash stored locally (never in")
	fmt.Println("dicode.yaml), and the plaintext printed once to that terminal — copy it then.")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Printf("  cd %s\n", path)
	fmt.Println("  git remote add origin <your-repo-url>")
	fmt.Println("  git add -A && git commit -m \"init\"")
	fmt.Println("  git push -u origin main")
	return nil
}

// rootTaskSetYAML is the TaskSet manifest written to <path>/tasks/taskset.yaml
// by `dicode init`. Unlike pkg/onboarding's starterTaskSetYAML (which
// deliberately writes an empty entries map for first-run onboarding and must
// not change — it has its own tests pinning that contract), this scaffold
// points at a real example entry so a freshly-init'd taskset is immediately
// runnable.
const rootTaskSetYAML = `apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: local
spec:
  # Add your tasks here. Each entry is keyed by the namespace segment
  # under which the referenced task or taskset is registered.
  entries:
    hello:
      ref:
        path: ./hello/task.yaml
`

// helloTaskYAML is a minimal manual-trigger example task, modeled on
// tasks/examples/hello-cron/task.yaml and tasks/examples/github-stars/task.yaml
// (runtime: deno, trigger.manual: true).
const helloTaskYAML = `apiVersion: dicode/v1
kind: Task
name: Hello
description: Minimal example task — logs a greeting. Edit or delete freely.
runtime: deno

trigger:
  manual: true

timeout: 10s
`

// helloTaskJS is the task.js paired with helloTaskYAML.
const helloTaskJS = `export default async function main() {
  const now = new Date().toISOString();
  console.log(` + "`Hello from dicode! (${now})`" + `);
  return { message: "Hello from dicode!" };
}
`

// scaffoldRootTaskSet creates tasksDir and writes the taskset manifest plus
// a single "hello" example task under it. The dicode.yaml guard in cmdInit
// only proves dicode.yaml itself is new — tasks/ can predate it (e.g. an
// operator who deleted just dicode.yaml, or hand-created tasks/ first), so
// each file is skipped rather than clobbered if it already exists.
func scaffoldRootTaskSet(tasksDir string) error {
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", tasksDir, err)
	}
	if err := writeFileIfAbsent(filepath.Join(tasksDir, "taskset.yaml"), rootTaskSetYAML); err != nil {
		return fmt.Errorf("write taskset.yaml: %w", err)
	}

	helloDir := filepath.Join(tasksDir, "hello")
	if err := os.MkdirAll(helloDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", helloDir, err)
	}
	if err := writeFileIfAbsent(filepath.Join(helloDir, "task.yaml"), helloTaskYAML); err != nil {
		return fmt.Errorf("write hello/task.yaml: %w", err)
	}
	if err := writeFileIfAbsent(filepath.Join(helloDir, "task.js"), helloTaskJS); err != nil {
		return fmt.Errorf("write hello/task.js: %w", err)
	}
	return nil
}

// writeFileIfAbsent writes content to path at 0o644 unless a file already
// exists there, in which case it's left untouched — see scaffoldRootTaskSet.
// A non-regular existing entry (most plausibly a directory — e.g. an
// operator's own tasks/hello/task.yaml/ subdirectory colliding with this
// scaffold's example) is an error rather than a silent skip: unlike the
// regular-file case, cmdInit would otherwise report success while leaving a
// non-runnable scaffold (a missing task.yaml) with no diagnostic at all.
func writeFileIfAbsent(path, content string) error {
	if fi, err := os.Stat(path); err == nil {
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s exists and is not a regular file (mode %s)", path, fi.Mode())
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// writeGitignore ensures <path>/.gitignore excludes the runtime data dir
// (SQLite DB + generated dashboard passphrase live there — see dataDirName).
// Without this, a careless "git add -A" in the scaffolded repo would commit
// secrets. Appends rather than overwrites if the file already exists (it
// shouldn't, given the dicode.yaml guard, but be defensive).
func writeGitignore(path string) error {
	gitignorePath := filepath.Join(path, ".gitignore")
	line := dataDirName + "/\n"

	existing, err := os.ReadFile(gitignorePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read %s: %w", gitignorePath, err)
		}
		if err := os.WriteFile(gitignorePath, []byte(line), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", gitignorePath, err)
		}
		return nil
	}

	content := string(existing)
	// Exact-suffix match, not a substring scan: gitignore semantics are
	// last-match-wins, so a prior rule that merely mentions ".dicode/" —
	// e.g. a negation like "!.dicode/", or a comment, or a nested
	// "foo/.dicode/" — does not actually ignore the data dir. Appending our
	// own rule as the final line always wins regardless of what came
	// before; only skip when that's already exactly the case (idempotent
	// re-run), not on a loose substring hit.
	if strings.HasSuffix(content, line) {
		return nil
	}
	if len(content) > 0 && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += line
	if err := os.WriteFile(gitignorePath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("append %s: %w", gitignorePath, err)
	}
	return nil
}
