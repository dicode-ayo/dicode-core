package taskset

import (
	"context"
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

	trustedDir, err := r.ensureClone(context.Background(), bare.url, "main", 0, "GH_TOKEN", true)
	if err != nil {
		t.Fatalf("trusted ensureClone: %v", err)
	}

	untrustedDir, err := r.ensureClone(context.Background(), bare.url, "main", 0, "", false)
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
