package taskset

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── #740: a writable source must not be able to exfiltrate the daemon's env
// credentials via a git ref's auth.token_env, and a ref registered as a task
// must not be able to smuggle itself in as a TaskSet. ──────────────────────

// TestGatedTokenEnv is a pure unit test of the trust decision resolveRef
// delegates to: a git ref's auth.token_env is only ever honoured when the ref
// was declared in operator-owned config (allowAuth=true) — never for a ref
// discovered while resolving an already-resolved sub-tree.
func TestGatedTokenEnv(t *testing.T) {
	cases := []struct {
		name          string
		allowAuth     bool
		tokenEnv      string
		wantEffective string
		wantBlocked   bool
	}{
		{"root ref with auth is honoured", true, "GH_TOKEN", "GH_TOKEN", false},
		{"root ref with no auth", true, "", "", false},
		{"nested ref with auth is stripped", false, "GH_TOKEN", "", true},
		{"nested ref with no auth", false, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			effective, blocked := gatedTokenEnv(tc.allowAuth, tc.tokenEnv)
			if effective != tc.wantEffective || blocked != tc.wantBlocked {
				t.Errorf("gatedTokenEnv(%v, %q) = (%q, %v), want (%q, %v)",
					tc.allowAuth, tc.tokenEnv, effective, blocked, tc.wantEffective, tc.wantBlocked)
			}
		})
	}
}

// resolveBodyGitRefAuth builds a single-entry TaskSetSpec whose entry is a git
// ref (pointing at a local bare repo fixture, so the clone itself always
// succeeds regardless of auth) with auth.token_env set, resolves it via
// resolveBody with the given allowAuth, and returns the results/failures plus
// how many "ignoring ref.auth.token_env" warnings were logged.
//
// A taskset.yaml loaded through the normal LoadTaskSet/Resolve path can't
// carry a fixture's file:// URL — ValidateRefURL rejects that scheme outright
// (by design, #486) — so this drives resolveBody directly, same package,
// isolating exactly the allowAuth behavior #740 added.
func resolveBodyGitRefAuth(t *testing.T, allowAuth bool) (results []*ResolvedTask, failures []ResolveFailure, warnings int) {
	t.Helper()
	bare := newSeededBareRepo(t)
	bare.commit(t, "task.yaml",
		"kind: Task\napiVersion: dicode/v1\nname: remote\nruntime: deno\ntrigger:\n  manual: true\n",
		"add task")

	ts := &TaskSetSpec{
		Spec: TaskSetBody{
			Entries: map[string]*Entry{
				"remote": {
					Ref: &Ref{
						URL:    bare.url,
						Branch: "main",
						Path:   "task.yaml",
						Auth:   RefAuth{TokenEnv: "DICODE_TEST_TOKEN_740"},
					},
				},
			},
		},
	}

	logger, logs := newObservedLogger()
	r := NewResolver(t.TempDir(), false, logger)
	fakeTSPath := filepath.Join(t.TempDir(), "taskset.yaml")
	results, failures, err := r.resolveBody(context.Background(), "infra", fakeTSPath, ts, nil, nil, nil, "", "", allowAuth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return results, failures, logs.FilterMessageSnippet("ignoring ref.auth.token_env").Len()
}

// TestResolve_RootTaskSetGitRefAuth_Honoured proves a git ref declared
// directly in a source's root taskset.yaml — operator-owned config
// (allowAuth=true) — still gets its auth.token_env honoured (no regression
// from #740's fix).
func TestResolve_RootTaskSetGitRefAuth_Honoured(t *testing.T) {
	results, failures, warnings := resolveBodyGitRefAuth(t, true)
	if len(failures) != 0 {
		t.Fatalf("resolve failures: %+v", failures)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if warnings != 0 {
		t.Errorf("a root-taskset entry's auth.token_env must be honoured, not stripped (%d warnings logged)", warnings)
	}
}

// TestResolve_NestedTaskSetGitRefAuth_Blocked is the #740 regression test:
// a git ref inside a resolved sub-tree (reached via a KindTaskSet entry, not
// declared directly in the source's root taskset — allowAuth=false) must have
// its auth.token_env stripped rather than handed to the clone — otherwise a
// writable source could smuggle in a nested taskset.yaml naming any daemon
// env var as a credential and exfiltrate it to a host of its choosing on the
// next reconcile, ahead of the approval gate. Before the fix, resolveRef
// passed ref.Auth.TokenEnv through unconditionally regardless of allowAuth
// (the parameter didn't exist), so this test would have found 0 "ignoring
// ref.auth.token_env" warnings.
func TestResolve_NestedTaskSetGitRefAuth_Blocked(t *testing.T) {
	results, failures, warnings := resolveBodyGitRefAuth(t, false)
	if len(failures) != 0 {
		t.Fatalf("resolve failures: %+v", failures)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if warnings != 1 {
		t.Errorf("a sub-tree entry's auth.token_env must be stripped and logged exactly once, got %d warnings", warnings)
	}
}

// TestRepoCloneDir_TrustTiersNeverShareADirectory is a direct unit test of
// the partitioning repoCloneDir provides: the same (url, branch) resolved
// under both trust tiers must land in two different directories.
func TestRepoCloneDir_TrustTiersNeverShareADirectory(t *testing.T) {
	dataDir := t.TempDir()
	url := "https://example.com/private-repo.git"
	branch := "main"

	trusted := repoCloneDir(dataDir, url, gitTarget{Kind: refBranch, Name: branch}, true)
	untrusted := repoCloneDir(dataDir, url, gitTarget{Kind: refBranch, Name: branch}, false)
	if trusted == untrusted {
		t.Fatalf("trusted and untrusted dirs for the same (url, branch) must differ, both got %q", trusted)
	}
}

// TestRepoCloneDir_NoCrossTierCollision is the regression test for a
// collision a security review found in this fix's second attempt: hashing
// `url + "@" + branch` (+ "@untrusted" for the low-trust tier) is not
// injective. An attacker who fully controls their own untrusted ref's
// (url, branch) could choose values whose concatenated-plus-suffix string
// reproduced an operator's real trusted "url@branch" seed byte-for-byte —
// landing both tiers' clones in the physically SAME directory despite the
// repoKey map correctly treating them as different cache entries, and (per
// the review) opening a path to either read the operator's authenticated
// clone or redirect the operator's next credentialed pull at an
// attacker-controlled "origin" already present in that shared directory.
//
// This reproduces the exact shape flagged: trusted (url, "@untrusted") used
// to hash identically to untrusted (url, "") under the old scheme — for the
// same url, seed_trusted = url+"@"+"@untrusted" and seed_untrusted =
// url+"@"+""+"@untrusted", both equal to url+"@@untrusted" byte-for-byte.
// (A branch literally containing "@" is unusual but not rejected anywhere on
// this path — ValidateBranchName is wired to the separate dev-mode clone
// path only, not general resolution — so it was a real, reachable
// collision, not merely a hypothetical one.) repoCloneDir now hashes url and
// branch independently before combining, so reconstructing one tier's
// directory from the other requires a SHA-256 preimage, not a
// string-concatenation trick.
func TestRepoCloneDir_NoCrossTierCollision(t *testing.T) {
	dataDir := t.TempDir()
	url := "https://example.com/private-repo.git"

	trusted := repoCloneDir(dataDir, url, gitTarget{Kind: refBranch, Name: "@untrusted"}, true)
	untrusted := repoCloneDir(dataDir, url, gitTarget{}, false)
	if trusted == untrusted {
		t.Fatalf("trusted dir for branch %q collided with untrusted dir for empty branch: %q", "@untrusted", trusted)
	}
}

// TestEnsureClone_UntrustedCannotReuseAuthenticatedCache is the regression
// test for a cache bypass found reviewing #740's first fix attempt: the
// clone-dedup cache was keyed only by (URL, Branch), so once an
// allowAuth=true resolution had cloned a repo (e.g. the operator's private
// root repo — routinely warmed by Resolve()/Pull() before any entry
// resolves), an allowAuth=false entry naming that SAME (url, branch) — no
// auth.token_env of its own required — would be handed that
// already-authenticated directory straight out of the cache. gatedTokenEnv
// stripping the token on the way in never mattered, because ensureClone
// returned the cached dir before tokenEnv was even consulted. ensureClone
// must never let a lower-trust call reuse a higher-trust call's clone.
func TestEnsureClone_UntrustedCannotReuseAuthenticatedCache(t *testing.T) {
	bare := newSeededBareRepo(t)
	bare.commit(t, "secret.txt", "operator-private content", "seed")

	r := newResolver(t)

	trustedDir, err := r.ensureClone(context.Background(), bare.url, gitTarget{Kind: refBranch, Name: "main"}, 0, "GH_TOKEN", true)
	if err != nil {
		t.Fatalf("trusted ensureClone: %v", err)
	}

	untrustedDir, err := r.ensureClone(context.Background(), bare.url, gitTarget{Kind: refBranch, Name: "main"}, 0, "", false)
	if err != nil {
		t.Fatalf("untrusted ensureClone: %v", err)
	}

	if untrustedDir == trustedDir {
		t.Fatalf("untrusted resolution reused the trusted clone's directory (%q) — the cache bypass is not fixed", trustedDir)
	}
	if _, err := os.Stat(filepath.Join(untrustedDir, "secret.txt")); err != nil {
		t.Errorf("untrusted clone should still succeed on its own against a repo with no real auth requirement: %v", err)
	}
}

// TestResolve_TaskRefResolvingAsTaskSet_Rejected is the second #740
// regression test: a ref whose configured path explicitly names "task.yaml"
// — the exact shape taskset.AddTaskEntry writes for every scaffolded task —
// must not be allowed to resolve as kind: TaskSet just because the file at
// that path declares it. Before the fix, DetectKind's answer alone decided
// routing, so this exact YAML would flatten "infra/evil/inner" into the
// result set instead of failing.
func TestResolve_TaskRefResolvingAsTaskSet_Rejected(t *testing.T) {
	repoDir := t.TempDir()

	evilDir := filepath.Join(repoDir, "evil")
	if err := os.MkdirAll(evilDir, 0755); err != nil {
		t.Fatal(err)
	}
	innerTaskDir := writeTaskDir(t, evilDir, "inner")
	// A file at a ".../task.yaml" path — exactly the shape AddTaskEntry
	// writes for a scaffolded task — but its content smuggles kind: TaskSet.
	writeFile(t, evilDir, "task.yaml", `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: evil
spec:
  entries:
    inner:
      ref:
        path: `+filepath.Join(innerTaskDir, "task.yaml")+`
`)

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    evil:
      ref:
        path: ./evil/task.yaml
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)

	r := newResolver(t)
	results, failures, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("a task ref smuggling kind: TaskSet must not flatten into results, got %d: %+v", len(results), results)
	}
	if len(failures) != 1 {
		t.Fatalf("want exactly 1 resolve failure, got %d: %+v", len(failures), failures)
	}
	if !strings.Contains(failures[0].Error.Error(), "TaskSet") {
		t.Errorf("failure should explain the kind mismatch, got: %v", failures[0].Error)
	}
}

// ── #753: the two residuals #748 left behind — an operator allowlist for
// token_env, and mustBeTask matching only a literal "task.yaml" basename. ──

// TestTokenEnvAllowed is a pure unit test of the allowlist decision
// ensureClone delegates to: an empty/nil allowlist is unrestricted (matching
// behavior before this allowlist existed), while a non-empty allowlist is
// exhaustive.
func TestTokenEnvAllowed(t *testing.T) {
	cases := []struct {
		name      string
		envVar    string
		allowlist []string
		want      bool
	}{
		{"no allowlist configured", "GH_TOKEN", nil, true},
		{"empty allowlist configured", "GH_TOKEN", []string{}, true},
		{"listed", "GH_TOKEN", []string{"GH_TOKEN", "OTHER"}, true},
		{"not listed", "OPENAI_API_KEY", []string{"GH_TOKEN"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tokenEnvAllowed(tc.envVar, tc.allowlist); got != tc.want {
				t.Errorf("tokenEnvAllowed(%q, %v) = %v, want %v", tc.envVar, tc.allowlist, got, tc.want)
			}
		})
	}
}

// TestEnsureClone_TokenEnvAllowlist_BlocksUnlistedVar is the #753 regression
// test for the first residual: even a ref trusted enough to carry auth
// (allowAuth=true) must have its token_env stripped when the operator has
// configured an allowlist that doesn't name it. Before the fix, Resolver had
// no allowedTokenEnvs concept at all, so gatedTokenEnv's allowAuth=true
// result passed straight through regardless of which variable was named.
func TestEnsureClone_TokenEnvAllowlist_BlocksUnlistedVar(t *testing.T) {
	bare := newSeededBareRepo(t)
	bare.commit(t, "marker.txt", "content", "seed")

	logger, logs := newObservedLogger()
	r := NewResolver(t.TempDir(), false, logger)
	r.SetAllowedTokenEnvs([]string{"GH_TOKEN"})

	dir, err := r.ensureClone(context.Background(), bare.url, gitTarget{Kind: refBranch, Name: "main"}, 0, "OPENAI_API_KEY", true)
	if err != nil {
		t.Fatalf("ensureClone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "marker.txt")); err != nil {
		t.Errorf("clone should still succeed unauthenticated against a repo with no real auth requirement: %v", err)
	}
	if n := logs.FilterMessageSnippet("not in the operator's source_security.allowed_token_envs allowlist").Len(); n != 1 {
		t.Errorf("want exactly 1 allowlist-block warning, got %d", n)
	}
}

// TestEnsureClone_TokenEnvAllowlist_PermitsListedVar proves the allowlist
// only blocks — it never breaks a variable the operator did list, and an
// unset allowlist stays fully permissive (no regression from #740's
// allowAuth=true path).
func TestEnsureClone_TokenEnvAllowlist_PermitsListedVar(t *testing.T) {
	bare := newSeededBareRepo(t)
	bare.commit(t, "marker.txt", "content", "seed")

	cases := []struct {
		name      string
		allowlist []string
	}{
		{"listed explicitly", []string{"GH_TOKEN"}},
		{"no allowlist configured", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger, logs := newObservedLogger()
			r := NewResolver(t.TempDir(), false, logger)
			r.SetAllowedTokenEnvs(tc.allowlist)

			if _, err := r.ensureClone(context.Background(), bare.url, gitTarget{Kind: refBranch, Name: "main"}, 0, "GH_TOKEN", true); err != nil {
				t.Fatalf("ensureClone: %v", err)
			}
			if n := logs.FilterMessageSnippet("allowed_token_envs allowlist").Len(); n != 0 {
				t.Errorf("token_env named in (or with no) allowlist must not be blocked, got %d warnings", n)
			}
		})
	}
}

// TestResolveRef_MustBeTask_AcceptsTaskYml is the #753 regression test for
// the extension half of the second residual: a ref path ending in "task.yml"
// — which task.LoadDirWithVars and write-task-file's assertTaskDocument both
// accept — must be treated exactly like "task.yaml" by the mustBeTask guard.
// Before the fix, mustBeTask compared only against the literal "task.yaml"
// basename, so a "task.yml" ref smuggling kind: TaskSet would have flattened
// straight into the result set instead of being refused.
func TestResolveRef_MustBeTask_AcceptsTaskYml(t *testing.T) {
	repoDir := t.TempDir()
	writeFile(t, repoDir, "task.yml", `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: evil
spec:
  entries: {}
`)

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    evil:
      ref:
        path: ./task.yml
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)

	r := newResolver(t)
	results, failures, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("a task.yml ref smuggling kind: TaskSet must not flatten into results, got %d: %+v", len(results), results)
	}
	if len(failures) != 1 {
		t.Fatalf("want exactly 1 resolve failure, got %d: %+v", len(failures), failures)
	}
	if !strings.Contains(failures[0].Error.Error(), "TaskSet") {
		t.Errorf("failure should explain the kind mismatch, got: %v", failures[0].Error)
	}
}

// TestResolveRef_MustBeTask_DirectoryValuedRefResolvesToTaskFile is the
// #753 regression test for the directory half of the second residual: a ref
// whose configured path names a DIRECTORY, not a file, must still trigger the
// mustBeTask guard once resolveYAMLPath's own probe lands it on a task.yaml
// inside that directory. Before the fix, mustBeTask was computed from the
// ref's literal (pre-resolution) basename — "evil", not "task.yaml" — so this
// exact shape resolved as kind: TaskSet unchecked.
func TestResolveRef_MustBeTask_DirectoryValuedRefResolvesToTaskFile(t *testing.T) {
	repoDir := t.TempDir()
	evilDir := filepath.Join(repoDir, "evil")
	if err := os.MkdirAll(evilDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, evilDir, "task.yaml", `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: evil
spec:
  entries: {}
`)

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    evil:
      ref:
        path: ./evil
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)

	r := newResolver(t)
	results, failures, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("a directory-valued ref resolving to a task file and smuggling kind: TaskSet must not flatten into results, got %d: %+v", len(results), results)
	}
	if len(failures) != 1 {
		t.Fatalf("want exactly 1 resolve failure, got %d: %+v", len(failures), failures)
	}
	if !strings.Contains(failures[0].Error.Error(), "TaskSet") {
		t.Errorf("failure should explain the kind mismatch, got: %v", failures[0].Error)
	}
}

// TestResolveRef_MustBeTask_DirectoryValuedRefWithOnlyTaskYml is a CodeRabbit
// review finding on #753: resolveYAMLPath's directory probe originally only
// checked taskset.yaml and task.yaml, not task.yml, even though isTaskFileName
// (and the loader, and write-task-file) treat task.yml as an equal alternative
// to task.yaml. A directory ref containing ONLY a task.yml stayed an
// unresolved directory, so mustBeTask never got a chance to fire — this pins
// resolveYAMLPath actually finding it, matching the sibling task.yaml test
// above.
func TestResolveRef_MustBeTask_DirectoryValuedRefWithOnlyTaskYml(t *testing.T) {
	repoDir := t.TempDir()
	evilDir := filepath.Join(repoDir, "evil")
	if err := os.MkdirAll(evilDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, evilDir, "task.yml", `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: evil
spec:
  entries: {}
`)

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    evil:
      ref:
        path: ./evil
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)

	r := newResolver(t)
	results, failures, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("a directory-valued ref resolving to a task.yml file and smuggling kind: TaskSet must not flatten into results, got %d: %+v", len(results), results)
	}
	if len(failures) != 1 {
		t.Fatalf("want exactly 1 resolve failure, got %d: %+v", len(failures), failures)
	}
	if !strings.Contains(failures[0].Error.Error(), "TaskSet") {
		t.Errorf("failure should explain the kind mismatch, got: %v", failures[0].Error)
	}
}

// TestResolveYAMLPath_ProbesTaskYml is a direct unit test of resolveYAMLPath's
// directory-probe order: taskset.yaml, then task.yaml, then task.yml.
func TestResolveYAMLPath_ProbesTaskYml(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "task.yml", "kind: Task\n")
	got := resolveYAMLPath(dir)
	want := filepath.Join(dir, "task.yml")
	if got != want {
		t.Errorf("resolveYAMLPath(%q) = %q, want %q", dir, got, want)
	}
}

// TestIsTaskFileName is a direct unit test of the basename predicate the
// mustBeTask fix relies on.
func TestIsTaskFileName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"task.yaml", true},
		{"task.yml", true},
		{"taskset.yaml", false},
		{"Task.yaml", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isTaskFileName(tc.name); got != tc.want {
			t.Errorf("isTaskFileName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestResolveRef_MustBeTask_ConfiguredTaskFilePathShadowedByDirectory is the
// regression test for a bypass a security review of #753's first draft
// found: computing mustBeTask ONLY from the resolved path let a writable
// source shadow a scaffolded ".../task.yaml" ref path — the exact shape
// taskEntryRefPath writes — with a DIRECTORY of that same name containing a
// nested taskset.yaml. resolveYAMLPath silently probes into it and returns
// the nested file, whose basename is "taskset.yaml", clearing mustBeTask
// even though the ref was declared exactly the trusted shape. mustBeTask
// must also honor the ref's own CONFIGURED path, not only the resolved one.
func TestResolveRef_MustBeTask_ConfiguredTaskFilePathShadowedByDirectory(t *testing.T) {
	repoDir := t.TempDir()
	// "task.yaml" is a DIRECTORY, not a file — shadowing the scaffolded
	// ".//task.yaml" ref shape with a nested kind: TaskSet.
	taskYamlDir := filepath.Join(repoDir, "task.yaml")
	if err := os.MkdirAll(taskYamlDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, taskYamlDir, "taskset.yaml", `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: evil
spec:
  entries: {}
`)

	tsContent := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: infra
spec:
  entries:
    evil:
      ref:
        path: ./task.yaml
`
	tsPath := writeTaskSetFile(t, repoDir, "taskset.yaml", tsContent)

	r := newResolver(t)
	results, failures, err := r.Resolve(context.Background(), "infra", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("a ref configured as .../task.yaml, shadowed on disk by a directory holding kind: TaskSet, must not flatten into results, got %d: %+v", len(results), results)
	}
	if len(failures) != 1 {
		t.Fatalf("want exactly 1 resolve failure, got %d: %+v", len(failures), failures)
	}
	if !strings.Contains(failures[0].Error.Error(), "TaskSet") {
		t.Errorf("failure should explain the kind mismatch, got: %v", failures[0].Error)
	}
}

// TestPull_TokenEnvAllowlist_BlocksUnlistedVar is the regression test for
// the second security-review finding: the token_env allowlist was wired
// into ensureClone (nested refs) but not Pull, which fetches a SOURCE'S
// ROOT ref — the primary case source_security.allowed_token_envs documents
// itself as covering. Before the fix, Pull passed ref.Auth.TokenEnv straight
// to syncClone with no allowlist check at all.
func TestPull_TokenEnvAllowlist_BlocksUnlistedVar(t *testing.T) {
	bare := newSeededBareRepo(t)
	bare.commit(t, "marker.txt", "content", "seed")

	logger, logs := newObservedLogger()
	r := NewResolver(t.TempDir(), false, logger)
	r.SetAllowedTokenEnvs([]string{"GH_TOKEN"})

	dir, err := r.Pull(context.Background(), &Ref{URL: bare.url, Branch: "main", Auth: RefAuth{TokenEnv: "OPENAI_API_KEY"}})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "marker.txt")); err != nil {
		t.Errorf("pull should still succeed unauthenticated against a repo with no real auth requirement: %v", err)
	}
	if n := logs.FilterMessageSnippet("not in the operator's source_security.allowed_token_envs allowlist").Len(); n != 1 {
		t.Errorf("want exactly 1 allowlist-block warning, got %d", n)
	}
}

// TestPull_TokenEnvAllowlist_PermitsListedVar proves the allowlist on Pull's
// root-ref path only blocks — a variable the operator did list still works.
func TestPull_TokenEnvAllowlist_PermitsListedVar(t *testing.T) {
	bare := newSeededBareRepo(t)
	bare.commit(t, "marker.txt", "content", "seed")

	logger, logs := newObservedLogger()
	r := NewResolver(t.TempDir(), false, logger)
	r.SetAllowedTokenEnvs([]string{"GH_TOKEN"})

	if _, err := r.Pull(context.Background(), &Ref{URL: bare.url, Branch: "main", Auth: RefAuth{TokenEnv: "GH_TOKEN"}}); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if n := logs.FilterMessageSnippet("allowed_token_envs allowlist").Len(); n != 0 {
		t.Errorf("token_env named in allowlist must not be blocked on Pull's root-ref path, got %d warnings", n)
	}
}

// TestRepoCloneDir_EveryKindAndTierGetsItsOwnDirectory is what cloneDirDomain's
// doc comment points at: every (refKind, trust tier) bucket must hash into its
// own directory for one and the same ref name. `branch: v1.0.0` and
// `tag: v1.0.0` name different commits, so serving one out of the other's
// directory would hand a pinned source whatever the branch has moved to — and
// serving an untrusted resolution out of a trusted one's directory is the #740
// credential leak.
//
// Adding a refKind without giving it a domain in cloneDirDomain collapses two
// buckets onto one directory, and fails here.
func TestRepoCloneDir_EveryKindAndTierGetsItsOwnDirectory(t *testing.T) {
	dataDir := t.TempDir()
	url := "https://example.com/repo.git"
	const name = "v1.0.0"

	seen := make(map[string]string)
	for _, kind := range []refKind{refBranch, refTag} {
		for _, allowAuth := range []bool{true, false} {
			bucket := fmt.Sprintf("%s/allowAuth=%v", kind, allowAuth)
			dir := repoCloneDir(dataDir, url, gitTarget{Kind: kind, Name: name}, allowAuth)
			if prev, dup := seen[dir]; dup {
				t.Errorf("%s and %s share clone directory %q for ref name %q", bucket, prev, dir, name)
			}
			seen[dir] = bucket
		}
	}
}
