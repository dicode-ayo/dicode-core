#!/usr/bin/env bash
#
# Shell-level tests for prune-stale-refs.sh's guards, run against a throwaway
# repo. The task's task.test.ts stubs Deno.Command, so nothing there exercises
# the script itself — and the script is what does the deleting.
#
#   ./prune-stale-refs.test.sh
#
# `gh` is stubbed on PATH so no network call is made and the open-PR guard is
# driven from a fixture.

set -uo pipefail

SCRIPT="$(cd "$(dirname "$0")" && pwd)/prune-stale-refs.sh"
PASS=0
FAIL=0

ok()   { PASS=$((PASS + 1)); printf '  ok   %s\n' "$1"; }
bad()  { FAIL=$((FAIL + 1)); printf '  FAIL %s\n     %s\n' "$1" "${2:-}"; }

# A repo with a default branch and a `gh` stub that reports no PRs.
new_repo() {
  local d
  d=$(mktemp -d)
  git -C "$d" init -q -b main
  git -C "$d" config user.email t@t.t
  git -C "$d" config user.name t
  git -C "$d" commit -q --allow-empty -m init
  # `origin/main` must resolve: point the repo at itself.
  git -C "$d" remote add origin "$d"
  git -C "$d" fetch -q origin 2>/dev/null || true
  git -C "$d" update-ref refs/remotes/origin/main refs/heads/main

  mkdir -p "$d/.bin"
  cat > "$d/.bin/gh" <<'STUB'
#!/usr/bin/env bash
# `gh pr list ... --json ...` → empty array; nothing else is called.
echo '[]'
STUB
  chmod +x "$d/.bin/gh"
  printf '%s' "$d"
}

run_apply() { # repo, plan-json -> output; never exits the harness
  local d="$1" plan="$2"
  printf '%s' "$plan" > "$d/plan.json"
  ( cd "$d" && PATH="$d/.bin:$PATH" bash "$SCRIPT" --apply --plan "$d/plan.json" 2>&1 )
}

# ---------------------------------------------------------------- guard tests

t_refuses_default_branch() {
  local d out; d=$(new_repo)
  out=$(run_apply "$d" '{"local_branches":{"delete":["main"]},"remote_branches":{"delete":[]},"worktrees":{"delete":[]}}')
  if grep -q "protected branch: main" <<<"$out" && git -C "$d" show-ref -q refs/heads/main; then
    ok "refuses a plan targeting the default branch"
  else
    bad "refuses a plan targeting the default branch" "$out"
  fi
  rm -rf "$d"
}

t_refuses_release_branch() {
  local d out; d=$(new_repo)
  git -C "$d" branch "release-please--branches--main"
  out=$(run_apply "$d" '{"local_branches":{"delete":["release-please--branches--main"]},"remote_branches":{"delete":[]},"worktrees":{"delete":[]}}')
  if grep -q "protected branch" <<<"$out" && git -C "$d" show-ref -q refs/heads/release-please--branches--main; then
    ok "refuses a plan targeting the release branch"
  else
    bad "refuses a plan targeting the release branch" "$out"
  fi
  rm -rf "$d"
}

# A ref may legally be named `-D`. Without `--`, git parses it as a flag: the
# branch survives and, worse, the flag changes what the command does.
t_dash_named_branch_is_deleted_not_parsed_as_flag() {
  local d out; d=$(new_repo)
  git -C "$d" commit -q --allow-empty -m keep
  git -C "$d" update-ref refs/heads/-D HEAD
  git -C "$d" branch victim
  out=$(run_apply "$d" '{"local_branches":{"delete":["victim","-D"]},"remote_branches":{"delete":[]},"worktrees":{"delete":[]}}')
  if ! git -C "$d" show-ref -q "refs/heads/-D" && ! git -C "$d" show-ref -q refs/heads/victim; then
    ok "a branch named -D is deleted as a ref, not swallowed as a flag"
  else
    bad "a branch named -D is deleted as a ref, not swallowed as a flag" "$out"
  fi
  rm -rf "$d"
}

t_refuses_unregistered_worktree_path() {
  local d out victim; d=$(new_repo)
  victim=$(mktemp -d); touch "$victim/precious"
  out=$(run_apply "$d" "{\"local_branches\":{\"delete\":[]},\"remote_branches\":{\"delete\":[]},\"worktrees\":{\"delete\":[{\"path\":\"$victim\"}]}}")
  if grep -q "not a worktree of this repo" <<<"$out" && [ -f "$victim/precious" ]; then
    ok "refuses a worktree path that is not registered to this repo"
  else
    bad "refuses a worktree path that is not registered to this repo" "$out"
  fi
  rm -rf "$d" "$victim"
}

t_refuses_dirty_worktree() {
  local d out; d=$(new_repo)
  git -C "$d" worktree add -q -b wt "$d/../wt-$$" 2>/dev/null
  local wt="$d/../wt-$$"
  echo "uncommitted" > "$wt/scratch.txt"
  out=$(run_apply "$d" "{\"local_branches\":{\"delete\":[]},\"remote_branches\":{\"delete\":[]},\"worktrees\":{\"delete\":[{\"path\":\"$(cd "$wt" && pwd)\"}]}}")
  if grep -q "uncommitted work" <<<"$out" && [ -f "$wt/scratch.txt" ]; then
    ok "refuses a worktree holding uncommitted work"
  else
    bad "refuses a worktree holding uncommitted work" "$out"
  fi
  git -C "$d" worktree remove --force "$wt" 2>/dev/null
  rm -rf "$d" "$wt"
}

t_refuses_locked_worktree_without_flag() {
  local d out; d=$(new_repo)
  local wt="$d/../wtl-$$"
  git -C "$d" worktree add -q -b wtl "$wt" 2>/dev/null
  git -C "$d" worktree lock "$wt"
  out=$(run_apply "$d" "{\"include_locked\":false,\"local_branches\":{\"delete\":[]},\"remote_branches\":{\"delete\":[]},\"worktrees\":{\"delete\":[{\"path\":\"$(cd "$wt" && pwd)\"}]}}")
  if grep -q "locked worktree" <<<"$out" && [ -d "$wt" ]; then
    ok "refuses a locked worktree unless include_locked was set"
  else
    bad "refuses a locked worktree unless include_locked was set" "$out"
  fi
  git -C "$d" worktree unlock "$wt" 2>/dev/null
  git -C "$d" worktree remove --force "$wt" 2>/dev/null
  rm -rf "$d" "$wt"
}

t_analysis_keeps_default_branch() {
  local d out; d=$(new_repo)
  git -C "$d" branch stale
  out=$( cd "$d" && PATH="$d/.bin:$PATH" bash "$SCRIPT" 2>/dev/null )
  if [ "$(jq -r '[.local_branches.delete[]] | index("main") // "absent"' <<<"$out")" = "absent" ]; then
    ok "analysis never puts the default branch in the delete list"
  else
    bad "analysis never puts the default branch in the delete list" "$out"
  fi
  rm -rf "$d"
}

echo "prune-stale-refs.sh guards:"
t_refuses_default_branch
t_refuses_release_branch
t_dash_named_branch_is_deleted_not_parsed_as_flag
t_refuses_unregistered_worktree_path
t_refuses_dirty_worktree
t_refuses_locked_worktree_without_flag
t_analysis_keeps_default_branch

echo
echo "$PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
