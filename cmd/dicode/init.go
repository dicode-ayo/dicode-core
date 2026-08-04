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

	if err := os.MkdirAll(path, 0o755); err != nil {
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
		Passphrase:    onboarding.GeneratePassphrase(),
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

	onboarding.PrintSuccess(os.Stdout, result, configPath)
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
// a single "hello" example task under it. Reached only from a from-scratch
// run (the dicode.yaml guard in cmdInit already refuses when config exists),
// so no clobber-avoidance beyond that guard is needed.
func scaffoldRootTaskSet(tasksDir string) error {
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", tasksDir, err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, "taskset.yaml"), []byte(rootTaskSetYAML), 0o644); err != nil {
		return fmt.Errorf("write taskset.yaml: %w", err)
	}

	helloDir := filepath.Join(tasksDir, "hello")
	if err := os.MkdirAll(helloDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", helloDir, err)
	}
	if err := os.WriteFile(filepath.Join(helloDir, "task.yaml"), []byte(helloTaskYAML), 0o644); err != nil {
		return fmt.Errorf("write hello/task.yaml: %w", err)
	}
	if err := os.WriteFile(filepath.Join(helloDir, "task.js"), []byte(helloTaskJS), 0o644); err != nil {
		return fmt.Errorf("write hello/task.js: %w", err)
	}
	return nil
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
	if strings.Contains(content, dataDirName+"/") {
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
