# git-pr (buildin)

Opens a GitHub pull request via the `gh` CLI. Used by the auto-fix flow —
auto-fix invokes whichever PR task is named in `params.pr_task` (this is
the reference implementation, replaceable by installing your own task and
pointing auto-fix at it).

## PAT prerequisite

Create a fine-grained Personal Access Token scoped to:
- Repository: only the repo you want auto-fix to PR into
- Permissions: **Contents → Read & write**, **Pull requests → Read & write**

Set as `GH_TOKEN_AUTOFIX` in dicode's secrets store. Do NOT reuse your
default `gh auth login` token — that often has admin scope and the
binary-name `permissions.run: [gh]` cannot prevent `gh repo delete`,
`gh secret set`, etc. The PAT is the defense.
