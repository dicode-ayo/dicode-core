/**
 * Generic AI agent task
 *
 * Runs a tool-use loop against any OpenAI-compatible provider, using dicode
 * tasks as tools. Each tool call invokes dicode.run_task() and feeds the
 * result back into the conversation until the model stops calling tools.
 *
 * Setup:
 *   Configure the AI provider (one-time):
 *     dicode config set ai.baseURL  https://api.openai.com/v1   # or Ollama: http://localhost:11434/v1
 *     dicode config set ai.model    gpt-4o                       # or ollama: llama3.2
 *     dicode config set ai.apiKey   sk-...                       # not needed for Ollama
 *
 * Trigger via webhook (auth required):
 *   curl -X POST http://localhost:8080/hooks/ai \
 *        -H "Content-Type: application/json" \
 *        -H "Authorization: Bearer <session-token>" \
 *        -d '{"prompt": "List running tasks", "skills": ""}'
 *
 * Or open /hooks/ai in the dicode UI to interact via the browser.
 *
 * Globals provided by the dicode runtime:
 *   params  — Map<string, string> of incoming request params
 *   log     — { info, warn, error } streaming log functions
 *   dicode  — { run_task, list_tasks, get_config } task orchestration
 *   output  — { html } for rich browser output
 */

import OpenAI from "npm:openai";

// ── Provider config ────────────────────────────────────────────────────────────
const aiCfg = await dicode.get_config("ai");
const client = new OpenAI({
  baseURL: aiCfg.baseURL || undefined,
  apiKey: aiCfg.apiKey || "ollama", // ollama doesn't need a real key
});

// ── Skill filtering ────────────────────────────────────────────────────────────
const skillFilter = ((await params.get("skills")) || "")
  .split(",")
  .map((s: string) => s.trim())
  .filter(Boolean);

const allTasks = await dicode.list_tasks();
const skillTasks = skillFilter.length
  ? allTasks.filter((t: any) => skillFilter.includes(t.id))
  : allTasks;

// ── Map dicode tasks → OpenAI tool definitions ─────────────────────────────────
const tools: OpenAI.ChatCompletionTool[] = skillTasks.map((t: any) => ({
  type: "function" as const,
  function: {
    name: t.id.replace(/[^a-z0-9_]/gi, "_"),
    description: t.description || t.name,
    parameters: {
      type: "object",
      properties: Object.fromEntries(
        (t.params || []).map((p: any) => [
          p.name,
          {
            type: p.type === "number" ? "number" : "string",
            description: p.description || "",
          },
        ]),
      ),
      required: (t.params || [])
        .filter((p: any) => p.required)
        .map((p: any) => p.name),
    },
  },
}));

const prompt = await params.get("prompt");
const messages: OpenAI.ChatCompletionMessageParam[] = [
  { role: "user", content: prompt },
];

await log.info(`agent starting with ${tools.length} tools available`);

// ── Agentic tool-use loop ──────────────────────────────────────────────────────
while (true) {
  const response = await client.chat.completions.create({
    model: aiCfg.model,
    messages,
    tools: tools.length ? tools : undefined,
  });

  const msg = response.choices[0].message;
  messages.push(msg);

  if (!msg.tool_calls?.length) break; // no more tool calls — done

  for (const tc of msg.tool_calls) {
    const taskID = tc.function.name.replace(/_/g, "-");
    const args = JSON.parse(tc.function.arguments || "{}");
    await log.info(`calling task: ${taskID}`, args);
    const result = await dicode.run_task(taskID, args);
    await log.info(`task ${taskID} → ${result.status}`);
    messages.push({
      role: "tool",
      tool_call_id: tc.id,
      content: JSON.stringify(result.returnValue ?? result),
    });
  }
}

// ── Extract final answer ───────────────────────────────────────────────────────
const lastMsg = messages.findLast((m: any) => m.role === "assistant");
const finalText =
  typeof lastMsg?.content === "string" ? lastMsg.content : "";

output.html(
  `<pre style="white-space:pre-wrap;font-family:inherit">${finalText}</pre>`,
);
return { result: finalText, steps: messages.length };
