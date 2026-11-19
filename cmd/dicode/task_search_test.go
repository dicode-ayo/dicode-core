package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/dicode/dicode/internal/gitops"
)

type fixtureTask struct{ id, name, description string }

var searchFixtureTasks = []fixtureTask{
	{"slack-notify", "Slack Notifier", "Posts a message to a channel."},
	{"db-backup", "Nightly Backup", "Dumps the Postgres database to S3."},
	{"hello", "Hello World", "Greets the operator."},
}

// writeSearchTaskSet writes a TaskSet with tasks into dir and returns the
// taskset.yaml path.
func writeSearchTaskSet(t *testing.T, dir string, tasks []fixtureTask) string {
	t.Helper()
	var ts strings.Builder
	ts.WriteString("apiVersion: dicode/v1\nkind: TaskSet\nmetadata:\n  name: fixture\nspec:\n  entries:\n")
	for _, tk := range tasks {
		taskDir := filepath.Join(dir, tk.id)
		if err := os.MkdirAll(taskDir, 0o755); err != nil {
			t.Fatal(err)
		}
		yml := "apiVersion: dicode/v1\nkind: Task\nname: " + tk.name + "\ndescription: " + tk.description +
			"\nruntime: deno\ntrigger:\n  manual: true\n"
		if err := os.WriteFile(filepath.Join(taskDir, "task.yaml"), []byte(yml), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(taskDir, "task.ts"), []byte("export default async () => {};\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		ts.WriteString("    " + tk.id + ":\n      ref:\n        path: ./" + tk.id + "/task.yaml\n")
	}
	p := filepath.Join(dir, "taskset.yaml")
	if err := os.WriteFile(p, []byte(ts.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func runSearch(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errb bytes.Buffer
	err = runTaskSearch(context.Background(), args, &out, &errb)
	return out.String(), errb.String(), err
}

func searchJSON(t *testing.T, args ...string) []map[string]any {
	t.Helper()
	out, stderr, err := runSearch(t, append(args, "--json")...)
	if err != nil {
		t.Fatalf("search %v: %v (stderr: %s)", args, err, stderr)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	return got
}

func resultIDs(rows []map[string]any) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r["id"].(string))
	}
	return ids
}

func TestTaskSearch_LocalTaskSetMatching(t *testing.T) {
	dir := t.TempDir()
	writeSearchTaskSet(t, dir, searchFixtureTasks)
	label := filepath.Base(dir)

	cases := []struct {
		name  string
		query []string
		want  []string
	}{
		{"by id", []string{"db-back"}, []string{label + "/db-backup"}},
		{"by name", []string{"notifier"}, []string{label + "/slack-notify"}},
		{"by description", []string{"postgres"}, []string{label + "/db-backup"}},
		{"case-insensitive", []string{"HELLO", "wOrLd"}, []string{label + "/hello"}},
		{"all words must match", []string{"slack", "postgres"}, nil},
		{"words in one arg", []string{"nightly s3"}, []string{label + "/db-backup"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resultIDs(searchJSON(t, append(tc.query, "--taskset", dir)...))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTaskSearch_TextOutput(t *testing.T) {
	dir := t.TempDir()
	writeSearchTaskSet(t, dir, searchFixtureTasks)

	out, _, err := runSearch(t, "backup", "--taskset", dir)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want header + 1 row, got %q", out)
	}
	if f := strings.Fields(lines[0]); strings.Join(f, " ") != "ID NAME DESCRIPTION" {
		t.Fatalf("header = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], filepath.Base(dir)+"/db-backup ") || !strings.Contains(lines[1], "Nightly Backup") {
		t.Fatalf("row = %q", lines[1])
	}
}

func TestTaskSearch_NoMatchIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	writeSearchTaskSet(t, dir, searchFixtureTasks)

	out, stderr, err := runSearch(t, "nothing-matches-this", "--taskset", dir)
	if err != nil {
		t.Fatalf("no-match returned error: %v", err)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
	if !strings.Contains(stderr, "no tasks match") {
		t.Fatalf("stderr = %q", stderr)
	}

	out, _, err = runSearch(t, "nothing-matches-this", "--taskset", dir, "--json")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Fatalf("json no-match = %q, %v", out, err)
	}
}

func TestTaskSearch_JSONShape(t *testing.T) {
	dir := t.TempDir()
	writeSearchTaskSet(t, dir, searchFixtureTasks)

	rows := searchJSON(t, "hello", "--taskset", dir)
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	want := map[string]any{
		"source":      filepath.Base(dir),
		"id":          filepath.Base(dir) + "/hello",
		"name":        "Hello World",
		"description": "Greets the operator.",
		"location":    dir,
		"ref":         "",
		"commit":      "",
	}
	if len(rows[0]) != len(want) {
		t.Fatalf("keys = %v, want %v", rows[0], want)
	}
	for k, v := range want {
		if rows[0][k] != v {
			t.Errorf("%s = %v, want %v", k, rows[0][k], v)
		}
	}
}

func TestTaskSearch_BadUsage(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--json"},
		{"   "},
		{"--taskset"},
		{"x", "--bogus"},
	} {
		if _, _, err := runSearch(t, args...); err == nil {
			t.Errorf("args %q: want error", args)
		} else if !strings.Contains(err.Error(), "usage: dicode task search") {
			t.Errorf("args %q: error %q lacks usage", args, err)
		}
	}
}

func TestTaskSearch_ConfiguredSources(t *testing.T) {
	root := t.TempDir()
	tsPath := writeSearchTaskSet(t, filepath.Join(root, "tasks"), searchFixtureTasks)
	cfgPath := filepath.Join(root, "dicode.yaml")
	cfg := "data_dir: " + filepath.Join(root, "data") + "\n" +
		"spec:\n  entries:\n    mytasks:\n      ref:\n        path: " + tsPath + "\n" +
		"      overrides:\n        entries:\n          hello:\n            name: Renamed Greeter\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	rows := searchJSON(t, "backup", "--config", cfgPath)
	if got := resultIDs(rows); len(got) != 1 || got[0] != "mytasks/db-backup" {
		t.Fatalf("got %v", got)
	}
	if rows[0]["source"] != "mytasks" {
		t.Fatalf("source = %v", rows[0]["source"])
	}

	rows = searchJSON(t, "renamed", "--config", cfgPath)
	if got := resultIDs(rows); len(got) != 1 || got[0] != "mytasks/hello" {
		t.Fatalf("override name not applied: %v", rows)
	}
}

func TestTaskSearch_MissingConfig(t *testing.T) {
	_, _, err := runSearch(t, "x", "--config", filepath.Join(t.TempDir(), "dicode.yaml"))
	if err == nil || !strings.Contains(err.Error(), "--taskset") {
		t.Fatalf("err = %v", err)
	}
}

// gitTaskSetRepo is a bare repo whose main branch and tag v1 hold one TaskSet
// and whose branch next holds another.
func gitTaskSetRepo(t *testing.T) string {
	t.Helper()
	main := plumbing.NewBranchReferenceName("main")
	bare := filepath.Join(t.TempDir(), "community.git")
	if _, err := gogit.PlainInitWithOptions(bare, &gogit.PlainInitOptions{
		Bare: true, InitOptions: gogit.InitOptions{DefaultBranch: main},
	}); err != nil {
		t.Fatal(err)
	}
	wtPath := t.TempDir()
	repo, err := gogit.PlainInitWithOptions(wtPath, &gogit.PlainInitOptions{InitOptions: gogit.InitOptions{DefaultBranch: main}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateRemote(&gogitconfig.RemoteConfig{Name: "origin", URLs: []string{bare}}); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	commit := func(tasks []fixtureTask) {
		writeSearchTaskSet(t, wtPath, tasks)
		if err := wt.AddGlob("."); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Commit("tasks", &gogit.CommitOptions{
			All: true, Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()},
		}); err != nil {
			t.Fatal(err)
		}
	}

	commit(searchFixtureTasks[:1])
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateTag("v1", head.Hash(), nil); err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(&gogit.PushOptions{RemoteName: "origin", RefSpecs: []gogitconfig.RefSpec{
		"refs/heads/main:refs/heads/main", "refs/tags/v1:refs/tags/v1",
	}}); err != nil {
		t.Fatal(err)
	}

	next := plumbing.NewBranchReferenceName("next")
	if err := wt.Checkout(&gogit.CheckoutOptions{Branch: next, Create: true}); err != nil {
		t.Fatal(err)
	}
	commit(searchFixtureTasks)
	if err := repo.Push(&gogit.PushOptions{RemoteName: "origin", RefSpecs: []gogitconfig.RefSpec{
		"refs/heads/next:refs/heads/next",
	}}); err != nil {
		t.Fatal(err)
	}
	return gitops.TestFixtureRemoteURL(bare)
}

func allowFileRemotes(t *testing.T) {
	t.Helper()
	prev := remoteSchemeAllowed
	remoteSchemeAllowed = func(u string) bool { return strings.HasPrefix(u, "file://") || prev(u) }
	t.Cleanup(func() { remoteSchemeAllowed = prev })
}

func TestTaskSearch_RemoteGitTaskSetHonoursRef(t *testing.T) {
	allowFileRemotes(t)
	url := gitTaskSetRepo(t)

	// main and tag v1 hold only slack-notify; branch next adds db-backup.
	for _, tc := range []struct {
		suffix, ref string
		want        int
	}{
		{"", "main", 0},
		{"@v1", "v1", 0},
		{"@next", "next", 1},
	} {
		t.Run("ref="+tc.ref, func(t *testing.T) {
			rows := searchJSON(t, "backup", "--taskset", url+tc.suffix)
			if len(rows) != tc.want {
				t.Fatalf("rows = %v, want %d", rows, tc.want)
			}
			rows = searchJSON(t, "slack", "--taskset", url+tc.suffix)
			if len(rows) != 1 {
				t.Fatalf("slack rows = %v", rows)
			}
			r := rows[0]
			if r["id"] != "community/slack-notify" || r["source"] != "community" || r["ref"] != tc.ref || r["location"] != url {
				t.Fatalf("row = %v", r)
			}
			if c, _ := r["commit"].(string); len(c) != 40 {
				t.Fatalf("commit = %q", c)
			}
		})
	}

	_, stderr, err := runSearch(t, "slack", "--taskset", url+"@no-such-ref")
	if err == nil || !strings.Contains(stderr, "ref not found") {
		t.Fatalf("missing ref: err=%v stderr=%q", err, stderr)
	}
}

func TestTaskSearch_RemoteCleansUpTempDir(t *testing.T) {
	allowFileRemotes(t)
	url := gitTaskSetRepo(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	searchJSON(t, "slack", "--taskset", url)
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp dir not cleaned up: %v", entries)
	}
}

func TestTaskSearch_RedactsURLCredentials(t *testing.T) {
	const secret = "s3cr3t-token"
	allowFileRemotes(t)
	url := gitTaskSetRepo(t)
	withCreds := strings.Replace(url, "file://", "file://alice:"+secret+"@", 1)

	out, stderr, err := runSearch(t, "slack", "--json", "--taskset", withCreds)
	if strings.Contains(out, secret) || strings.Contains(stderr, secret) || (err != nil && strings.Contains(err.Error(), secret)) {
		t.Fatalf("credential leaked: out=%q stderr=%q err=%v", out, stderr, err)
	}

	unreachable := "https://alice:" + secret + "@127.0.0.1:1/org/repo.git@main"
	out, stderr, err = runSearch(t, "slack", "--taskset", unreachable)
	if err == nil {
		t.Fatal("want error for unreachable remote")
	}
	for _, s := range []string{out, stderr, err.Error()} {
		if strings.Contains(s, secret) {
			t.Fatalf("credential leaked: %q", s)
		}
	}
	if !strings.Contains(stderr, "https://127.0.0.1:1/org/repo.git") {
		t.Fatalf("stderr does not name the redacted URL: %q", stderr)
	}

	_, _, err = runSearch(t, "slack", "--taskset", "ftp://alice:"+secret+"@example.com/repo")
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("bad scheme err = %v", err)
	}
}

func TestSplitRefSuffix(t *testing.T) {
	for _, tc := range []struct{ in, url, ref string }{
		{"https://github.com/org/repo.git", "https://github.com/org/repo.git", ""},
		{"https://github.com/org/repo.git@v1.2.0", "https://github.com/org/repo.git", "v1.2.0"},
		{"https://user:p@ss@github.com/org/repo", "https://user:p@ss@github.com/org/repo", ""},
		{"https://user@github.com/org/repo@feature/x", "https://user@github.com/org/repo", "feature/x"},
		{"git@github.com:org/repo.git", "git@github.com:org/repo.git", ""},
		{"git@github.com:org/repo.git@main", "git@github.com:org/repo.git", "main"},
		{"https://github.com", "https://github.com", ""},
	} {
		url, ref := splitRefSuffix(tc.in)
		if url != tc.url || ref != tc.ref {
			t.Errorf("splitRefSuffix(%q) = %q, %q; want %q, %q", tc.in, url, ref, tc.url, tc.ref)
		}
	}
}

func TestTaskSearch_DispatchedUnderTask(t *testing.T) {
	if err := cmdTask(nil, []string{"search"}); err == nil || !strings.Contains(err.Error(), "usage: dicode task search") {
		t.Fatalf("cmdTask search = %v", err)
	}
}
