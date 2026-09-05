package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"golang.org/x/term"
	_ "modernc.org/sqlite"

	"github.com/dicode/dicode/pkg/onboarding"
)

// dataDirName is the runtime data dir dicode's generated config points at
// (SQLite DB, logs, the dashboard passphrase's home). It lives inside the
// scaffolded repo so the whole taskset directory is self-contained, which is
// exactly why it must be .gitignored — see writeGitignore.
const dataDirName = ".dicode"

// dataDBName is the SQLite file onboarding.RenderConfig points database.path
// at, relative to the data dir. TestRenderConfigDBPathMatchesInit pins the two
// together.
const dataDBName = "data.db"

// storedPassphraseKey is pkg/webui's passphraseKVKey, unexported there. A
// drift between the two only costs this command the sharper of its two
// closing messages, never correctness.
const storedPassphraseKey = "auth.passphrase"

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

On a terminal this runs the same setup wizard as first-run onboarding —
which curated tasksets to enable, tasks and data directories, port, and
an optional dashboard passphrase — seeded with defaults suited to a
committable directory. When stdin is not a terminal (CI, a Dockerfile,
a pipe) those defaults are taken as-is and nothing is asked. Set
DICODE_ONBOARDING=cli or =silent to force either behaviour.

Every default path written into the generated dicode.yaml is
${CONFIGDIR}-relative, so the directory keeps working after a fresh "git
clone" onto a different machine or a different home directory. Answers
that give that up, or that put a credential where "git push" would
publish it, are called out at the end of the run.

Refuses to run if <path>/dicode.yaml already exists, to avoid clobbering an
operator's existing config.`

// cmdInit implements `dicode init [path]`. It is daemon-free: like deno/
// python/relock/webhook above, it only touches the filesystem (plus an
// in-process go-git init and, on a TTY, the CLI wizard), so it runs before
// ensureDaemon is ever reached.
func cmdInit(args []string) error {
	return runInit(args, os.Stdin, os.Stdout, os.Stderr,
		term.IsTerminal(int(os.Stdin.Fd())), os.Getenv)
}

// runInit is cmdInit with its environment injected, so tests can drive the
// wizard without a controlling terminal.
func runInit(args []string, in io.Reader, out, errOut io.Writer, isTTY bool, env func(string) string) error {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			fmt.Fprintln(errOut, initUsage)
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

	result := initDefaults()
	if shouldRunWizard(isTTY, env, errOut) {
		r, err := onboarding.RunCLIWith(in, out, onboarding.CLIOptions{
			Seed: result,
			// Unlike the daemon's first-run wizard, the seed passphrase is
			// empty and must be able to stay that way — see initDefaults.
			PromptPassphrase: true,
		})
		if err != nil {
			return fmt.Errorf("wizard: %w", err)
		}
		result = r
		fmt.Fprintln(out)
	}

	if err := onboarding.WriteConfig(configPath, onboarding.RenderConfig(result)); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}

	tasksDir := resolveConfigPath(result.LocalTasksDir, path)
	if tasksDir != "" {
		if err := scaffoldRootTaskSet(tasksDir); err != nil {
			return err
		}
	}

	ignore, err := dataDirIgnoreEntry(path, result.DataDir)
	if err != nil {
		return err
	}
	if ignore != "" {
		if err := writeGitignore(path, ignore); err != nil {
			return err
		}
	}

	if _, err := gogit.PlainInit(path, false); err != nil {
		if errors.Is(err, gogit.ErrRepositoryAlreadyExists) {
			fmt.Fprintln(out, "note: a .git repository already exists at", path, "— leaving it as-is")
		} else {
			return fmt.Errorf("git init %s: %w", path, err)
		}
	}

	printInitSummary(out, path, configPath, result, ignore != "",
		storedPassphraseDB(resolveConfigPath(result.DataDir, path)))
	return nil
}

// initDefaults is the Result `dicode init` starts from, and the one it uses
// verbatim when nothing is asked. It differs from the daemon's first-run
// defaults in two ways, both because this directory is meant to be committed
// and pushed to a git remote:
//
// Paths are ${CONFIGDIR}-relative rather than home-relative, so they still
// resolve after a clone onto another machine or a different home directory.
//
// The passphrase is empty. onboarding.RenderConfig writes it verbatim as
// server.secret, and pkg/webui/passphrase.go's verifyPassphrase treats a
// non-empty server.secret as an unhashed, precedence-winning plaintext
// credential compared on every login. Baking one in would mean `git push`
// ships a working dashboard password to the remote, recoverable from history
// forever even after later removal. Empty means no YAML override, so
// server.auth's own ensurePassphrase auto-generates a passphrase, stores only
// its bcrypt hash in the (gitignored) local database, and prints the
// plaintext once — on first `dicode daemon` start, not here.
func initDefaults() onboarding.Result {
	enabled := make(map[string]bool, len(onboarding.TaskSetPresets))
	for _, p := range onboarding.TaskSetPresets {
		enabled[p.Name] = true
	}
	return onboarding.Result{
		TaskSetsEnabled: enabled,
		LocalTasksDir:   "${CONFIGDIR}/tasks",
		DataDir:         "${CONFIGDIR}/" + dataDirName,
		Port:            8080, // matches onboarding's own default (pkg/onboarding/cli.go defaultPort)
		Passphrase:      "",
	}
}

// shouldRunWizard reports whether cmdInit should prompt. Only a terminal
// gets prompts: anything that might be a script — a pipe, a Dockerfile RUN, a
// CI step — must scaffold from the defaults rather than block forever on a
// question nobody can answer. DICODE_ONBOARDING forces the decision either
// way, matching onboarding.PickSurface's own env override; its browser value
// resolves to the CLI prompts, the only surface this command has.
func shouldRunWizard(isTTY bool, env func(string) string, warn io.Writer) bool {
	switch strings.ToLower(strings.TrimSpace(env("DICODE_ONBOARDING"))) {
	case "silent":
		return false
	case "cli":
		return true
	case "browser":
		fmt.Fprintln(warn, "note: dicode init has no browser wizard — using the CLI prompts")
		return true
	}
	return isTTY
}

// resolveConfigPath maps a path as written into dicode.yaml onto a real
// filesystem path, mirroring the ~, ${HOME} and ${CONFIGDIR} expansion
// pkg/config performs at load time. Returns "" for a value this command
// cannot resolve: ${DATADIR} is only known to the daemon once it has loaded
// the config, and an unknown variable must not be pasted into a mkdir.
func resolveConfigPath(value, configDir string) string {
	if value == "" {
		return ""
	}
	home, homeErr := os.UserHomeDir()
	if strings.HasPrefix(value, "~/") && homeErr == nil {
		value = home + value[1:]
	}
	value = strings.ReplaceAll(value, "${CONFIGDIR}", configDir)
	if homeErr == nil {
		value = strings.ReplaceAll(value, "${HOME}", home)
	}
	if strings.Contains(value, "${") {
		return ""
	}
	return value
}

// dataDirIgnoreEntry returns the .gitignore pattern excluding the runtime
// data dir from the scaffolded repo, or "" when that dir lives outside the
// repo and git would never see it anyway. The data dir holds the SQLite
// database — dashboard passphrase hash, task KV, the encrypted secrets store
// — so wherever the operator put it, a "git add -A" in this directory must
// not sweep it up.
//
// The pattern is unanchored (".dicode/", not "/.dicode/"), the form
// writeGitignore's idempotence check compares against.
func dataDirIgnoreEntry(root, dataDir string) (string, error) {
	resolved := resolveConfigPath(dataDir, root)
	if resolved == "" {
		return "", nil
	}
	rel, ok := relWithin(root, resolved)
	// "." means the operator pointed the data dir at the repo root itself:
	// there is no pattern that excludes it without excluding everything, so
	// leave .gitignore alone and let printInitSummary say so.
	if !ok || rel == "." {
		return "", nil
	}
	return filepath.ToSlash(rel) + "/\n", nil
}

// rootTaskSetYAML is the TaskSet manifest written to <tasks dir>/taskset.yaml
// by `dicode init`. It names a real entry so the scaffold is runnable as
// written; pkg/onboarding's starterTaskSetYAML is a separate contract with an
// empty entries map, pinned by its own tests.
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
// only proves dicode.yaml itself is new — the tasks dir can predate it (e.g.
// an operator who deleted just dicode.yaml, hand-created tasks/ first, or
// pointed the wizard at a directory they already keep tasks in), so each
// file is skipped rather than clobbered if it already exists.
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

// writeGitignore ensures <path>/.gitignore contains line (see
// dataDirIgnoreEntry). Without it, a careless "git add -A" in the scaffolded
// repo would commit the SQLite database. Appends rather than overwrites if
// the file already exists (it shouldn't, given the dicode.yaml guard, but be
// defensive).
func writeGitignore(path, line string) error {
	gitignorePath := filepath.Join(path, ".gitignore")

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

// storedPassphraseDB returns the path of the database in dataDir that already
// holds a dashboard passphrase, or "" when there is none to find — no data
// dir, no database, no kv table, no row. The daemon reuses such a passphrase
// silently, so the closing banner must not promise one will be generated and
// printed on the next start.
//
// The database is opened read-only: this command scaffolds a directory and
// must never create or migrate a database the daemon owns. Every failure
// (unreadable file, foreign schema, a daemon mid-write) resolves to "", which
// only costs the sharper message.
func storedPassphraseDB(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	dbPath := filepath.Join(dataDir, dataDBName)
	if _, err := os.Stat(dbPath); err != nil {
		return ""
	}
	handle, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return ""
	}
	defer func() { _ = handle.Close() }()

	var value string
	if err := handle.QueryRow(
		`SELECT value FROM kv WHERE key = ?`, storedPassphraseKey,
	).Scan(&value); err != nil || value == "" {
		return ""
	}
	return dbPath
}

// printInitSummary writes the closing banner: where the config went, how the
// dashboard credential will be obtained, any way the chosen answers undercut
// the two properties this command exists to provide (a directory that is safe
// to push, and that still resolves after a clone elsewhere), and the git
// commands to publish it.
func printInitSummary(out io.Writer, path, configPath string, res onboarding.Result, dataDirIgnored bool, storedPassphraseDB string) {
	fmt.Fprintln(out, "━━━ dicode init complete ━━━")
	fmt.Fprintln(out, "Config written to", configPath)
	fmt.Fprintln(out)
	if res.Passphrase == "" {
		fmt.Fprintln(out, "No dashboard passphrase was generated here — this directory is meant to be")
		fmt.Fprintln(out, "committed to git, and dicode.yaml has no server.secret to keep it that way.")
		if storedPassphraseDB != "" {
			fmt.Fprintln(out, "The data directory carries one over from an earlier install:")
			fmt.Fprintln(out, "  "+storedPassphraseDB)
			fmt.Fprintln(out, "`dicode daemon` reuses that passphrase and prints nothing. If you no longer")
			fmt.Fprintln(out, "have it, start the daemon and run `dicode auth reset-passphrase`.")
		} else {
			fmt.Fprintln(out, "The first time you run `dicode daemon` in this directory, a passphrase is")
			fmt.Fprintln(out, "generated automatically, its hash stored locally (never in dicode.yaml),")
			fmt.Fprintln(out, "and the plaintext printed once to that terminal — copy it then.")
		}
	} else {
		fmt.Fprintf(out, "Dashboard: http://localhost:%d\n", res.Port)
		fmt.Fprintln(out, "Login passphrase: the one you entered, stored in dicode.yaml as server.secret.")
	}
	fmt.Fprintln(out, "━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	if w := initWarnings(path, res, dataDirIgnored); len(w) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Before you push:")
		for _, line := range w {
			fmt.Fprintln(out, "  ! "+line)
		}
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Next steps:")
	fmt.Fprintf(out, "  cd %s\n", path)
	fmt.Fprintln(out, "  git remote add origin <your-repo-url>")
	fmt.Fprintln(out, "  git add -A && git commit -m \"init\"")
	fmt.Fprintln(out, "  git push -u origin main")
}

// initWarnings lists the ways the wizard's answers break what `dicode init`
// guarantees by default. Empty when the operator kept the defaults, which is
// why the whole "Before you push" block disappears on an unattended run.
func initWarnings(path string, res onboarding.Result, dataDirIgnored bool) []string {
	var out []string

	if res.Passphrase != "" {
		out = append(out,
			"dicode.yaml now holds your passphrase in plaintext as server.secret, and it "+
				"wins over the hashed one. Pushing this repo publishes a working dashboard "+
				"login to anyone who can read it — and to its history, after any later removal.")
	}

	switch {
	case res.LocalTasksDir == "":
	case resolveConfigPath(res.LocalTasksDir, path) == "":
		out = append(out, fmt.Sprintf(
			"Tasks directory %s uses a variable only the daemon can expand, so no starter "+
				"taskset was written there. Create it, with a taskset.yaml inside, before "+
				"the first run.", res.LocalTasksDir))
	case !insideDir(path, res.LocalTasksDir):
		out = append(out, fmt.Sprintf(
			"Tasks directory %s is outside this directory, so a clone elsewhere will not "+
				"find your tasks. ${CONFIGDIR}/... keeps them travelling with the repo.",
			res.LocalTasksDir))
	}

	for _, f := range []struct{ label, value string }{
		{"Tasks directory", res.LocalTasksDir},
		{"Data directory", res.DataDir},
	} {
		if machineSpecific(f.value) {
			out = append(out, fmt.Sprintf(
				"%s %s is written into dicode.yaml verbatim, naming a path that exists on "+
					"this machine. A clone on another one resolves it to the same place and "+
					"finds nothing there; ${CONFIGDIR}/... resolves to wherever the clone "+
					"landed.", f.label, f.value))
		}
	}

	if !dataDirIgnored && insideDir(path, res.DataDir) {
		out = append(out, fmt.Sprintf(
			"Data directory %s is inside this directory but could not be excluded in "+
				".gitignore. It holds the SQLite database — passphrase hash, task KV, "+
				"encrypted secrets — so move it or exclude it before committing.",
			res.DataDir))
	}

	return out
}

// machineSpecific reports whether a dicode.yaml path value names an absolute
// location on this machine rather than one relative to the config. Such a
// value survives the commit unchanged and resolves, on a clone elsewhere, to
// a path that has nothing to do with where the clone landed — so it defeats
// the portability the ${CONFIGDIR} defaults exist to provide, whether or not
// it happens to sit inside the scaffolded directory today.
func machineSpecific(value string) bool {
	if value == "" || strings.Contains(value, "${CONFIGDIR}") {
		return false
	}
	return filepath.IsAbs(value) || strings.HasPrefix(value, "~/") || strings.Contains(value, "${HOME}")
}

// insideDir reports whether a dicode.yaml path value resolves to root or
// somewhere beneath it. An unresolvable value (see resolveConfigPath) counts
// as outside: this only ever gates a warning, and guessing "inside" would
// claim git coverage that may not exist.
func insideDir(root, value string) bool {
	resolved := resolveConfigPath(value, root)
	if resolved == "" {
		return false
	}
	_, ok := relWithin(root, resolved)
	return ok
}

// relWithin returns path's location relative to root, and whether it is root
// or below it. A path that escapes root, or that cannot be made absolute,
// reports false.
func relWithin(root, path string) (string, bool) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(absRoot, abs)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}
