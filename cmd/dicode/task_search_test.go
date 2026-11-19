package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/go-git/go-git/v5/storage/filesystem"

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

func TestIsNoDaemonTaskSearch(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"task", "search"}, true},
		{[]string{"task", "search", "slack", "--json"}, true},
		{[]string{"task"}, false},
		{[]string{"task", "test", "search"}, false},
		{[]string{"search"}, false},
	} {
		if got := isNoDaemonTaskSearch(tc.args); got != tc.want {
			t.Errorf("isNoDaemonTaskSearch(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestTaskSearch_FlagValues(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"x", "--config="}, "--config requires a value"},
		{[]string{"x", "--config", ""}, "--config requires a value"},
		{[]string{"x", "--taskset", "--json"}, "--taskset requires a value"},
		{[]string{"x", "--config", "--json"}, "--config requires a value"},
	} {
		_, _, err := runSearch(t, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("args %q: err = %v, want %q", tc.args, err, tc.want)
		}
	}
}

func TestExplicitSearchTargets_DisambiguatesLabels(t *testing.T) {
	root := t.TempDir()
	var paths []string
	for _, sub := range []string{"a", "b", "c"} {
		p := filepath.Join(root, sub, "tasks")
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	tasks2 := filepath.Join(root, "d", "tasks-2")
	if err := os.MkdirAll(tasks2, 0o755); err != nil {
		t.Fatal(err)
	}
	targets, err := explicitSearchTargets([]string{
		"https://example.com/a/tasks.git", paths[0], tasks2, paths[1], "https://example.com/b/tasks.git",
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tg := range targets {
		got = append(got, tg.label)
		if !tg.untrusted {
			t.Errorf("%s: --taskset target must be untrusted", tg.location)
		}
	}
	if want := "tasks,tasks-2,tasks-2-2,tasks-3,tasks-4"; strings.Join(got, ",") != want {
		t.Fatalf("labels = %v, want %s", got, want)
	}
}

func TestExplicitSearchTargets_OneRemotePredicate(t *testing.T) {
	prev := remoteSchemeAllowed
	t.Cleanup(func() { remoteSchemeAllowed = prev })

	remoteSchemeAllowed = func(u string) bool { return u == "fixture-remote" || prev(u) }
	targets, err := explicitSearchTargets([]string{"fixture-remote"})
	if err != nil || len(targets) != 1 || targets[0].ref.URL != "fixture-remote" {
		t.Fatalf("widened predicate not used to classify: targets=%+v err=%v", targets, err)
	}

	remoteSchemeAllowed = func(string) bool { return false }
	_, err = explicitSearchTargets([]string{"git@github.com:org/repo.git"})
	if err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("SCP value with a predicate that rejects it should be a local path: err=%v", err)
	}
}

func TestTaskSearch_SameBasenameTaskSetsKeepDistinctIDs(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a", "tasks")
	b := filepath.Join(root, "b", "tasks")
	writeSearchTaskSet(t, a, searchFixtureTasks[2:])
	writeSearchTaskSet(t, b, searchFixtureTasks[2:])

	rows := searchJSON(t, "hello", "--taskset", a, "--taskset", b)
	if got := strings.Join(resultIDs(rows), ","); got != "tasks-2/hello,tasks/hello" {
		t.Fatalf("ids = %s", got)
	}
}

// commitBareRepo creates a bare repo whose main branch holds what write puts
// in the worktree, and returns the bare dir.
func commitBareRepo(t *testing.T, write func(dir string)) string {
	t.Helper()
	main := plumbing.NewBranchReferenceName("main")
	bare := filepath.Join(t.TempDir(), "repo.git")
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
	write(wtPath)
	if err := wt.AddGlob("."); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("seed", &gogit.CommitOptions{
		All: true, Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(&gogit.PushOptions{RemoteName: "origin"}); err != nil {
		t.Fatal(err)
	}
	return bare
}

func bareHead(t *testing.T, bare string) string {
	t.Helper()
	repo, err := gogit.PlainOpen(bare)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	return head.Hash().String()
}

type fixedLoader struct{ s storer.Storer }

func (l fixedLoader) Load(*transport.Endpoint) (storer.Storer, error) { return l.s, nil }

// httpGitRemote serves bare over smart HTTP on loopback, permits loopback in
// the SSRF guard, and records every Authorization header it receives.
func httpGitRemote(t *testing.T, bare string) (url string, authSeen func() []string) {
	t.Helper()
	srv := server.NewServer(fixedLoader{filesystem.NewStorage(osfs.New(bare), cache.NewObjectLRUDefault())})
	var mu sync.Mutex
	var seen []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			mu.Lock()
			seen = append(seen, a)
			mu.Unlock()
		}
		ep, err := transport.NewEndpoint("http://fixture/repo.git")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		sess, err := srv.NewUploadPackSession(ep, nil)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs"):
			ar, err := sess.AdvertisedReferencesContext(r.Context())
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_, _ = io.WriteString(w, "001e# service=git-upload-pack\n0000")
			_ = ar.Encode(w)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
			req := packp.NewUploadPackRequest()
			if err := req.Decode(r.Body); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			resp, err := sess.UploadPack(r.Context(), req)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			_ = resp.Encode(w)
		default:
			http.NotFound(w, r)
		}
	})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	allow, err := gitops.ParseAllowlist([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	gitops.SetInternalHostAllowlist(allow)
	t.Cleanup(func() { gitops.SetInternalHostAllowlist(nil) })

	return ts.URL + "/repo.git", func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// singleTaskRepo is a bare repo whose root is one task.
func singleTaskRepo(t *testing.T) string {
	t.Helper()
	return commitBareRepo(t, func(dir string) {
		yml := "apiVersion: dicode/v1\nkind: Task\nname: Nested Remote\ndescription: pulled from another repo\nruntime: deno\ntrigger:\n  manual: true\n"
		if err := os.WriteFile(filepath.Join(dir, "task.yaml"), []byte(yml), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "task.ts"), []byte("export default async () => {};\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
}

// appendEntry adds a raw entry to a taskset.yaml written by writeSearchTaskSet.
func appendEntry(t *testing.T, tsPath, entry string) {
	t.Helper()
	f, err := os.OpenFile(tsPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(entry); err != nil {
		t.Fatal(err)
	}
}

func TestTaskSearch_CommitIsRootTaskSetHead(t *testing.T) {
	allowFileRemotes(t)
	nestedURL, _ := httpGitRemote(t, singleTaskRepo(t))
	root := commitBareRepo(t, func(dir string) {
		p := writeSearchTaskSet(t, dir, searchFixtureTasks[2:])
		appendEntry(t, p, "    nested:\n      ref:\n        url: "+nestedURL+"\n        path: task.yaml\n")
	})

	rows := searchJSON(t, "e", "--taskset", gitops.TestFixtureRemoteURL(root))
	if got := strings.Join(resultIDs(rows), ","); got != "repo/hello,repo/nested" {
		t.Fatalf("ids = %s", got)
	}
	want := bareHead(t, root)
	for _, r := range rows {
		if r["commit"] != want {
			t.Errorf("%s: commit = %v, want root HEAD %s", r["id"], r["commit"], want)
		}
	}
}

const hostileTokenEnv = "DICODE_SEARCH_TEST_TOKEN"

// hostileTaskSet writes a local TaskSet whose entry asks for hostileTokenEnv
// to be sent to remote.
func hostileTaskSet(t *testing.T, remote string) string {
	t.Helper()
	dir := t.TempDir()
	p := writeSearchTaskSet(t, dir, searchFixtureTasks[2:])
	appendEntry(t, p, "    grab:\n      ref:\n        url: "+remote+"\n        path: task.yaml\n        auth:\n          token_env: "+hostileTokenEnv+"\n")
	return dir
}

func TestTaskSearch_ExplicitTaskSetNeverSendsEntryCredentials(t *testing.T) {
	t.Setenv(hostileTokenEnv, "operator-secret")
	remote, authSeen := httpGitRemote(t, singleTaskRepo(t))
	dir := hostileTaskSet(t, remote)

	rows := searchJSON(t, "nested", "--taskset", dir)
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	if got := authSeen(); len(got) != 0 {
		t.Fatalf("--taskset sent credentials to the TaskSet's own remote: %v", got)
	}
}

func TestTaskSearch_ConfiguredSourceTokenEnvs(t *testing.T) {
	t.Setenv(hostileTokenEnv, "operator-secret")
	for _, tc := range []struct {
		name      string
		allowlist string
		wantAuth  bool
	}{
		{"unrestricted", "", true},
		{"listed", "  allowed_token_envs: [" + hostileTokenEnv + "]\n", true},
		{"not listed", "  allowed_token_envs: [SOME_OTHER_TOKEN]\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote, authSeen := httpGitRemote(t, singleTaskRepo(t))
			dir := hostileTaskSet(t, remote)
			root := t.TempDir()
			cfgPath := filepath.Join(root, "dicode.yaml")
			cfg := "data_dir: " + filepath.Join(root, "data") + "\n" +
				"source_security:\n  allow_internal_hosts: [127.0.0.1]\n" + tc.allowlist +
				"spec:\n  entries:\n    mine:\n      ref:\n        path: " + filepath.Join(dir, "taskset.yaml") + "\n"
			if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}

			if rows := searchJSON(t, "nested", "--config", cfgPath); len(rows) != 1 {
				t.Fatalf("rows = %v", rows)
			}
			if got := len(authSeen()) > 0; got != tc.wantAuth {
				t.Fatalf("credentials sent = %v, want %v", got, tc.wantAuth)
			}
		})
	}
}
