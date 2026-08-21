# Agent Skill

A **skill** is a markdown file that lives in the shared `tasks/skills/` directory and gives an AI agent running inside a dicode task the context it needs to work effectively in this repo. Skills live on disk and are read at run time — nothing is embedded in the dicode binary. The agent is told each skill's name and description up front and pulls the body in when it needs it.

The canonical task-developer skill lives at `tasks/skills/dicode-task-dev.md`. It documents the mandatory task-development workflow (validate → test → commit), the `task.yaml` schema, available SDK globals, the test harness, and common mistakes to avoid.

---

## How skills are loaded

Skills are consumed by the `buildin/ai-agent` task (and any override of it). The base task exposes three params:

```yaml
params:
  skills:
    default: ""
    description: "Comma-separated skill md file names (without .md) to make available to the model."
  skills_mode:
    default: "index"
    description: "index = advertise name + description, body via the dicode_read_skill tool. eager = concatenate every body into the system prompt."
  skills_dir:
    default: "${TASK_SET_DIR}/../skills"
    description: "Absolute path to the directory holding skill .md files."
```

At task-load time `${TASK_SET_DIR}` expands to the directory containing the root `taskset.yaml` that loaded the agent — for the built-in taskset that's `tasks/buildin/`, so `skills_dir` resolves to `tasks/skills/`.

Set `skills: "dicode-task-dev,dicode-basics"` on a call (or as a preset default) and the ai-agent reads both files. Under the default `index` mode it appends a short catalogue to its `system_prompt` —

```
# Skills

These skills carry the rules, schemas and workflows this daemon expects you to
follow. Call dicode_read_skill for every skill whose subject touches the
request and read what it returns before you write a file or call another tool.
Their contents are not guessable from these descriptions.

- dicode-task-dev — Mandatory workflow, rules, and conventions for developing dicode tasks — trigger/permissions schema, test format, and common mistakes.
- dicode-basics — Core concepts an agent should know about dicode — tasks, triggers, KV, and the relationship between tasks and the tools it can call.
```

— and offers a `dicode_read_skill` tool that hands back a named skill's full body. This is why the frontmatter `description` matters: it is the only thing the model sees before deciding to read.

`skills_mode: eager` restores the older behaviour, concatenating every body into the system prompt on every turn. See [ai-agent.md](ai-agent.md#skills-prompt-markdown) for why `index` is the default.

---

## Preset: `buildin/dicodai`

The `dicodai` preset (defined in `tasks/buildin/taskset.yaml` as an override of `./ai-agent/task.yaml`) ships with:

- `skills: "dicode-task-dev,dicode-basics"` — both skills available to look up
- A task-development-tuned `system_prompt`
- OpenAI defaults (`model: "gpt-4o"`, `base_url: "https://api.openai.com/v1"`, `api_key_env: "OPENAI_API_KEY"`)
- Webhook at `/hooks/ai/dicodai`

That means with only `OPENAI_API_KEY` set, the WebUI task-detail "AI" chat panel works out of the box.

---

## Adding your own skill

1. Drop a markdown file into `tasks/skills/`, e.g. `tasks/skills/github-flow.md`.
2. Start it with YAML frontmatter. The `description` is what the agent sees in the index, so write it as the answer to "should I read this for the request in front of me?":

   ```yaml
   ---
   name: github-flow
   description: How this repo ships code — branch naming, CI, review rules.
   ---
   ```

   A skill with no frontmatter is indexed by its first line of prose instead.

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
