# Memory Index

- [Use TypeScript for Deno task scripts](feedback_ts_files.md) — always create task.ts, not task.js, for Deno runtime tasks
- [Always check CI and update PR after commits](feedback_ci_pr_checks.md) — after any push: check CI, update PR description, update docs/ files
- [Update agent skill when relevant capabilities change](feedback_update_skills.md) — update pkg/agent/skill.md whenever a new trigger field, JS global, task.yaml field, or security pattern is added
- [Always format before committing](feedback_format_before_commit.md) — run `go fmt ./...` before every commit or CI lint will fail
