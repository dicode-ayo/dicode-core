# git-pr (buildin)

Opens a GitHub pull request via the `gh` CLI. Used by the auto-fix flow's
`dicode-auto-fix` skill, which currently calls this task by name (`git-pr`).
Swapping in a different PR task today requires editing the skill prompt
(`tasks/skills/dicode-auto-fix.md`) and the auto-fix task's `dicode.tasks`
permission in `tasks/buildin/taskset.yaml` — there is no `params.pr_task`
override read by any Go or task code yet.

## PAT prerequisite

Create a fine-grained Personal Access Token scoped to:
- Repository: only the repo you want auto-fix to PR into
- Permissions: **Contents → Read & write**, **Pull requests → Read & write**

Set as `GH_TOKEN_AUTOFIX` in dicode's secrets store. Do NOT reuse your
default `gh auth login` token — that often has admin scope and the
binary-name `permissions.run: [gh]` cannot prevent `gh repo delete`,
`gh secret set`, etc. The PAT is the defense.
