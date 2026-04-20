# Agent Skill

A **skill** is a markdown file that lives in the shared `tasks/skills/` directory and gives an AI agent running inside a dicode task the context it needs to work effectively in this repo. Skills are eager-loaded into the agent's system prompt at run time — nothing is embedded in the dicode binary.

The canonical task-developer skill lives at `tasks/skills/dicode-task-dev.md`. It documents the mandatory task-development workflow (validate → test → commit), the `task.yaml` schema, available SDK globals, the test harness, and common mistakes to avoid.

---

## How skills are loaded

Skills are consumed by the `buildin/ai-agent` task (and any override of it). The base task exposes two params:

```yaml
params:
  skills:
    default: ""
    description: "Comma-separated skill md file names (without .md) to concatenate into the system prompt."
  skills_dir:
    default: "${TASK_SET_DIR}/../skills"
    description: "Absolute path to the directory holding skill .md files."
```

At task-load time `${TASK_SET_DIR}` expands to the directory containing the root `taskset.yaml` that loaded the agent — for the built-in taskset that's `tasks/buildin/`, so `skills_dir` resolves to `tasks/skills/`.

Set `skills: "dicode-task-dev,dicode-basics"` on a call (or as a preset default) and the ai-agent reads both files, concatenates them onto its `system_prompt`, and the model starts the conversation with that context already loaded.

---

## Preset: `buildin/dicodai`

The `dicodai` preset (defined in `tasks/buildin/taskset.yaml` as an override of `./ai-agent/task.yaml`) ships with:

- `skills: "dicode-task-dev,dicode-basics"` — both skills preloaded
- A task-development-tuned `system_prompt`
- OpenAI defaults (`model: "gpt-4o"`, `base_url: "https://api.openai.com/v1"`, `api_key_env: "OPENAI_API_KEY"`)
- Webhook at `/hooks/ai/dicodai`

That means with only `OPENAI_API_KEY` set, the WebUI task-detail "AI" chat panel works out of the box.

---

## Adding your own skill

1. Drop a markdown file into `tasks/skills/`, e.g. `tasks/skills/github-flow.md`.
2. Start it with YAML frontmatter so `kind:Task skills_dir` tooling can list it:

   ```yaml
   ---
   name: github-flow
   description: How this repo ships code — branch naming, CI, review rules.
   ---
   ```

3. Reference it by filename (without `.md`) from any ai-agent task or override:

   ```yaml
   params:
     skills: "dicode-task-dev,github-flow"
   ```

Skills are plain markdown — no special templating language, no compilation step.

---

## Using with a custom agent outside dicode

If you're writing your own agent (e.g. a Claude Code session, a local chat UI), the same file is the recommended system-prompt fragment. Point your agent at `tasks/skills/dicode-task-dev.md` and read the file at startup:

```bash
cat tasks/skills/dicode-task-dev.md >> CLAUDE.md
```

The skill is the entire document — it was written to stand on its own without further scaffolding.
