#!/usr/bin/env bash
#
# Analyse and prune stale git worktrees and branches (local + remote).
#
#   prune-stale-refs.sh                 # analyse only, emit a JSON plan on stdout
#   prune-stale-refs.sh --apply         # execute the plan
#   prune-stale-refs.sh --apply --plan FILE
#                                       # execute a previously emitted plan verbatim
#
# Analysis and execution are separate phases on purpose: the plan is a value you
# can review, approve, and hand back. `--apply --plan FILE` never re-derives
# anything, so what a human approved is exactly what runs.
#
# A ref is safe to delete when its content is already on the default branch:
#   - its PR is MERGED (covers squash-merges, which rewrite commit ids and so
#     defeat `git branch --merged` and `git cherry` alike), or
#   - it carries zero commits beyond origin/<default>.
#
# Never deleted, regardless of the above:
#   - the default branch
#   - any branch that is the head of an OPEN PR
#   - the release-please branch (reused across releases, so old merged release
#     PRs make it look safe while the current release PR still needs it)
#   - a worktree with uncommitted work, or a branch holding unmerged commits
#
# Requires: git, gh (authenticated), jq.

set -euo pipefail

DEFAULT_BRANCH="${DEFAULT_BRANCH:-main}"
RELEASE_BRANCH="${RELEASE_BRANCH:-release-please--branches--main}"
# Files git leaves dirty that never represent real work. Space-separated.
IGNORE_DIRTY="${IGNORE_DIRTY:-dicode.lock}"

APPLY=0
PLAN_FILE=""
INCLUDE_LOCKED=0

while [ $# -gt 0 ]; do
  case "$1" in
    --apply)          APPLY=1 ;;
    --plan)           PLAN_FILE="${2:?--plan needs a file}"; shift ;;
    --include-locked) INCLUDE_LOCKED=1 ;;
    -h|--help)        sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
  shift
done

for bin in git gh jq; do
  command -v "$bin" >/dev/null || { echo "missing dependency: $bin" >&2; exit 1; }
done

REPO_ROOT=$(git rev-parse --show-toplevel)
cd "$REPO_ROOT"

# ---------------------------------------------------------------- apply-a-plan

if [ -n "$PLAN_FILE" ]; then
  [ "$APPLY" -eq 1 ] || { echo "--plan is only meaningful with --apply" >&2; exit 2; }
  [ -r "$PLAN_FILE" ] || { echo "cannot read plan: $PLAN_FILE" >&2; exit 1; }
  plan=$(cat "$PLAN_FILE")
else
  plan=""
fi

# ------------------------------------------------------------------- analysis

analyse() {
  git fetch origin "$DEFAULT_BRANCH" --quiet
  git remote prune origin >/dev/null 2>&1 || true

  local prs protected
  prs=$(gh pr list --state all --limit 1000 --json headRefName,state,number)

  # Protected = default + release branch + every OPEN PR's head branch.
  protected=$(jq -r --arg d "$DEFAULT_BRANCH" --arg r "$RELEASE_BRANCH" '
    [ $d, $r ] + [ .[] | select(.state=="OPEN") | .headRefName ] | unique | .[]' <<<"$prs")

  is_protected() { printf '%s\n' "$protected" | grep -qxF "$1"; }

  # branch -> newest PR state
  pr_state() {
    jq -r --arg b "$1" 'map(select(.headRefName==$b)) | sort_by(.number) | last | .state // "NONE"' <<<"$prs"
  }

  ahead_of_main() { git rev-list --count "origin/$DEFAULT_BRANCH..$1" 2>/dev/null || echo 999; }

  # ---- worktrees
  local wt_del="[]" wt_keep="[]"
  local wt br locked dirty state reason
  while IFS= read -r line; do
    case "$line" in
      worktree\ *) wt=${line#worktree }; br=""; locked=0 ;;
      branch\ *)   br=${line#branch refs/heads/} ;;
      locked*)     locked=1 ;;
      "")
        [ -n "${wt:-}" ] || continue
        [ "$wt" = "$REPO_ROOT" ] && { wt=""; continue; }

        dirty=$(git -C "$wt" status --porcelain 2>/dev/null \
                 | grep -vE " ($(echo "$IGNORE_DIRTY" | tr ' ' '|'))$" | wc -l | tr -d ' ')
        state=$( [ -n "$br" ] && pr_state "$br" || echo NONE )
        reason=""

        if [ -n "$br" ] && is_protected "$br"; then reason="protected branch"
        elif [ "$dirty" -gt 0 ];                then reason="$dirty uncommitted file(s)"
        elif [ "$locked" -eq 1 ] && [ "$INCLUDE_LOCKED" -eq 0 ]; then reason="locked (pass --include-locked)"
        elif [ "$state" = "MERGED" ];           then reason=""
        elif [ -z "$br" ];                      then reason=""            # detached, clean
        elif [ "$(ahead_of_main "$br")" -eq 0 ]; then reason=""
        else reason="$(ahead_of_main "$br") unmerged commit(s), PR=$state"
        fi

        if [ -z "$reason" ]; then
          wt_del=$(jq --arg p "$wt" --arg b "$br" '. + [{path:$p,branch:$b}]' <<<"$wt_del")
        else
          wt_keep=$(jq --arg p "$wt" --arg b "$br" --arg r "$reason" '. + [{path:$p,branch:$b,reason:$r}]' <<<"$wt_keep")
        fi
        wt=""
        ;;
    esac
  done < <(git worktree list --porcelain; echo)

  # ---- local branches
  local loc_del="[]" loc_keep="[]" n
  while IFS= read -r br; do
    [ "$br" = "$DEFAULT_BRANCH" ] && continue
    if is_protected "$br"; then
      loc_keep=$(jq --arg b "$br" '. + [{branch:$b,reason:"protected"}]' <<<"$loc_keep"); continue
    fi
    # A branch checked out in a surviving worktree cannot be deleted.
    if git worktree list --porcelain | grep -qxF "branch refs/heads/$br"; then
      if ! jq -e --arg b "$br" 'any(.[]; .branch==$b)' >/dev/null <<<"$wt_del"; then
        loc_keep=$(jq --arg b "$br" '. + [{branch:$b,reason:"checked out in a retained worktree"}]' <<<"$loc_keep"); continue
      fi
    fi
    state=$(pr_state "$br"); n=$(ahead_of_main "$br")
    if [ "$state" = "MERGED" ] || [ "$n" -eq 0 ]; then
      loc_del=$(jq --arg b "$br" '. + [$b]' <<<"$loc_del")
    else
      loc_keep=$(jq --arg b "$br" --arg r "$n unmerged commit(s), PR=$state" '. + [{branch:$b,reason:$r}]' <<<"$loc_keep")
    fi
  done < <(git for-each-ref --format='%(refname:short)' refs/heads/)

  # ---- remote branches: MERGED PR only. A no-PR remote branch may be someone
  # else's in-flight work, so "0 commits ahead" is not sufficient there.
  local rem_del="[]"
  while IFS= read -r br; do
    is_protected "$br" && continue
    [ "$(pr_state "$br")" = "MERGED" ] && rem_del=$(jq --arg b "$br" '. + [$b]' <<<"$rem_del")
  done < <(git ls-remote --heads origin | sed 's#.*refs/heads/##')

  jq -n --argjson wd "$wt_del" --argjson wk "$wt_keep" \
        --argjson ld "$loc_del" --argjson lk "$loc_keep" --argjson rd "$rem_del" \
        --arg d "$DEFAULT_BRANCH" '{
     default_branch:$d,
     worktrees:{delete:$wd, keep:$wk},
     local_branches:{delete:$ld, keep:$lk},
     remote_branches:{delete:$rd},
     summary:{worktrees_delete:($wd|length), worktrees_keep:($wk|length),
              local_delete:($ld|length),   local_keep:($lk|length),
              remote_delete:($rd|length)}
   }'
}

[ -n "$plan" ] || plan=$(analyse)

if [ "$APPLY" -eq 0 ]; then
  printf '%s\n' "$plan"
  exit 0
fi

# -------------------------------------------------------------------- execute

# Re-assert the invariants against the plan we are about to run. A plan handed
# back after an approval pause may have been authored anywhere; the guards below
# are the last thing standing between it and `git push --delete`.
guard=$(jq -r --arg d "$DEFAULT_BRANCH" --arg r "$RELEASE_BRANCH" '
  [ (.local_branches.delete // [])[], (.remote_branches.delete // [])[] ]
  | map(select(. == $d or . == $r)) | .[]' <<<"$plan")
[ -z "$guard" ] || { echo "refusing: plan targets a protected branch: $guard" >&2; exit 1; }

open_heads=$(gh pr list --state open --limit 200 --json headRefName --jq '.[].headRefName')
if [ -n "$open_heads" ]; then
  clash=$(jq -r '[ (.local_branches.delete // [])[], (.remote_branches.delete // [])[] ] | .[]' <<<"$plan" \
          | grep -xF -f <(printf '%s\n' "$open_heads") || true)
  [ -z "$clash" ] || { echo "refusing: plan targets an open PR's branch: $clash" >&2; exit 1; }
fi

jq -r '.worktrees.delete[]? | .path' <<<"$plan" | while IFS= read -r p; do
  [ -n "$p" ] || continue
  git worktree unlock "$p" 2>/dev/null || true
  git worktree remove --force "$p" && echo "worktree removed: $p"
done
git worktree prune

mapfile -t locals < <(jq -r '.local_branches.delete[]?' <<<"$plan")
if [ "${#locals[@]}" -gt 0 ]; then
  # -D not -d: squash-merged branches are not ancestors of main, so -d refuses them.
  git branch -D "${locals[@]}" >/dev/null && echo "local branches deleted: ${#locals[@]}"
fi

mapfile -t remotes < <(jq -r '.remote_branches.delete[]?' <<<"$plan")
if [ "${#remotes[@]}" -gt 0 ]; then
  # Chunked: one push per ~50 refs keeps the request small and a failure local.
  i=0
  while [ "$i" -lt "${#remotes[@]}" ]; do
    chunk=("${remotes[@]:$i:50}")
    git push origin --delete "${chunk[@]}" >/dev/null 2>&1 \
      && echo "remote branches deleted: ${#chunk[@]}" \
      || echo "warning: a remote chunk failed (already gone?)" >&2
    i=$((i + 50))
  done
fi

git remote prune origin >/dev/null 2>&1 || true
echo "done"
