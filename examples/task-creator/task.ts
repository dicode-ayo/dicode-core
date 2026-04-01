// task-creator — generate a new dicode task from a plain-language description.
//
// The task uses OpenAI tool-use with a `write_task_files` tool to produce
// valid task.yaml + task.ts content in a single model call, then returns
// the generated files so the user can commit them to their task source.
//
// NOTE: There is no POST /api/dev/tasks endpoint in dicode to create tasks
// programmatically — task directories must be created by the user and
// registered via taskset.yaml or the --tasks flag. This task therefore
// generates the file contents and renders them for copy/paste or download.
//
// Required AI config in dicode.yaml:
//   ai:
//     baseURL: http://localhost:11434/v1   # Ollama, or omit for OpenAI
//     model: qwen2.5-coder:7b             # any model with tool-use support
//     apiKey: ollama                       # or real OpenAI key

import OpenAI from "npm:openai";

const aiCfg = await dicode.get_config("ai");
const client = new OpenAI({
  baseURL: aiCfg.baseURL || undefined,
  apiKey: aiCfg.apiKey || "ollama",
});

const description = String(await params.get("description") ?? "");
const taskID = String(await params.get("task_id") ?? "");

if (!description) throw new Error("description param is required");
if (!taskID) throw new Error("task_id param is required");
if (!/^[a-z0-9-]+$/.test(taskID)) {
  throw new Error("task_id must be a lowercase slug, e.g. send-weekly-report");
}

await log.info(`Generating task files for: ${taskID}`);

const response = await client.chat.completions.create({
  model: aiCfg.model,
  messages: [
    {
      role: "system",
      content: `You are a dicode task generator. Generate valid task.yaml and task.ts files.

task.yaml must start with:
  apiVersion: dicode/v1
  kind: Task

Required task.yaml fields: name, description, runtime (always "deno"), trigger.
Optional task.yaml fields: params, env, timeout, webhook_secret.

Trigger options:
  trigger:
    manual: true            # for manual/one-off tasks
  trigger:
    cron: "0 9 * * 1"      # cron schedule (quartz syntax)
  trigger:
    webhook: /hooks/<slug>  # HTTP webhook

task.ts uses these dicode shim globals — never import them, they are always available:
  log.info(msg) / log.warn(msg) / log.error(msg)  — structured logging
  params.get(name)                                 — task param value (async)
  env.get(name)                                    — environment variable
  kv.get(key) / kv.set(key, value)                — persistent key-value store
  output.html(htmlString)                          — set rich HTML output
  http.post(url, { headers?, body? })              — outbound HTTP POST
  dicode.get_config("ai")                          — AI config { baseURL, model, apiKey }

Always return a JSON-serializable value from the task (use "return { ... }").
Use "await" when calling params.get, env.get, kv.get, kv.set, log.*, and http.*.
Write clean TypeScript with explicit types.`,
    },
    {
      role: "user",
      content: `Create a dicode task with id "${taskID}" that does the following:\n\n${description}`,
    },
  ],
  tools: [
    {
      type: "function" as const,
      function: {
        name: "write_task_files",
        description:
          "Write the task.yaml and task.ts files for the new dicode task",
        parameters: {
          type: "object",
          properties: {
            task_yaml: {
              type: "string",
              description: "Full contents of task.yaml",
            },
            task_ts: {
              type: "string",
              description: "Full contents of task.ts",
            },
          },
          required: ["task_yaml", "task_ts"],
        },
      },
    },
  ],
  tool_choice: "required",
});

const tc = response.choices[0].message.tool_calls?.[0];
if (!tc) {
  await log.error("AI did not invoke write_task_files");
  return { error: "no tool call from AI" };
}

let parsed: { task_yaml: string; task_ts: string };
try {
  parsed = JSON.parse(tc.function.arguments);
} catch (e) {
  await log.error(`Failed to parse AI tool arguments: ${e}`);
  return { error: "invalid JSON from AI tool call" };
}

const { task_yaml, task_ts } = parsed;

await log.info(`Task files generated for: ${taskID}`);

// Escape HTML entities so code renders correctly inside <pre> blocks.
function escapeHTML(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

return output.html(`
<div style="font-family:system-ui,sans-serif;max-width:720px;padding:1.5rem">
  <h2 style="margin:0 0 .5rem">Generated task: <code>${escapeHTML(taskID)}</code></h2>
  <p style="color:#666;font-size:.875rem;margin:0 0 1.5rem">
    Copy these files into <code>tasks/${escapeHTML(taskID)}/</code> and reload dicode.
  </p>

  <h3 style="margin:0 0 .4rem;font-size:1rem">task.yaml</h3>
  <pre style="background:#1e1e2e;color:#cdd6f4;padding:1rem;border-radius:8px;
              white-space:pre-wrap;word-break:break-all;font-size:.82rem;overflow-x:auto">${escapeHTML(task_yaml)}</pre>

  <h3 style="margin:1rem 0 .4rem;font-size:1rem">task.ts</h3>
  <pre style="background:#1e1e2e;color:#cdd6f4;padding:1rem;border-radius:8px;
              white-space:pre-wrap;word-break:break-all;font-size:.82rem;overflow-x:auto">${escapeHTML(task_ts)}</pre>
</div>
`);
