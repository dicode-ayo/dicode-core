// AI Agent built-in task.
//
// Calls an OpenAI-compatible chat completions API (OpenAI, Anthropic via
// openai-compat, Ollama, LM Studio, Together, ...) with a tool-use loop that
// lets the model call other dicode tasks as tools. Conversation history is
// persisted per session in KV and lazily compacted when it exceeds
// max_history_tokens.
//
// The task bare-returns { session_id, reply } so browser UIs can parse it
// as application/json. Never call output.html() here — that would override
// the content type.
import OpenAI from "npm:openai@4";

type Role = "system" | "user" | "assistant" | "tool";

interface StoredMessage {
  role: Role;
  content: string;
  tool_calls?: unknown[];
  tool_call_id?: string;
  name?: string;
}

interface SessionState {
  messages: StoredMessage[];
  summary?: string;
  created_at: number;
  updated_at: number;
}

interface TaskSummary {
  id: string;
  name: string;
  description?: string;
  params?: Array<{ name: string; type?: string; description?: string; required?: boolean }>;
}

// Rough token estimate (chars / 4) — good enough for deciding when to compact.
function estimateTokens(messages: StoredMessage[], summary?: string): number {
  let chars = summary ? summary.length : 0;
  for (const m of messages) {
    chars += m.content.length;
    if (m.tool_calls) chars += JSON.stringify(m.tool_calls).length;
  }
  return Math.ceil(chars / 4);
}

// Map a task id to a tool name. OpenAI tool names must match /^[a-zA-Z0-9_-]+$/.
function taskIdToToolName(id: string): string {
  return "task_" + id.replace(/[^a-zA-Z0-9_-]/g, "_");
}

// Convert dicode param list to a JSON Schema object for an OpenAI tool.
function paramsToJsonSchema(
  params: TaskSummary["params"],
): Record<string, unknown> {
  const properties: Record<string, unknown> = {};
  const required: string[] = [];
  if (params) {
    for (const p of params) {
      properties[p.name] = {
        type: p.type === "number" ? "number" : p.type === "boolean" ? "boolean" : "string",
        description: p.description ?? "",
      };
      if (p.required) required.push(p.name);
    }
  }
  return {
    type: "object",
    properties,
    required,
    additionalProperties: false,
  };
}

// Strip summarized turns and replace with a single system "summary" entry.
async function compactIfNeeded(
  session: SessionState,
  maxTokens: number,
  client: OpenAI,
  compactionModel: string,
): Promise<void> {
  if (estimateTokens(session.messages, session.summary) <= maxTokens) return;

  // Keep the last 4 turns verbatim; summarize everything older.
  const keep = 4;
  if (session.messages.length <= keep) return;

  const older = session.messages.slice(0, -keep);
  const recent = session.messages.slice(-keep);

  const transcript = older
    .map((m) => `${m.role.toUpperCase()}: ${m.content}`)
    .join("\n");

  const previousSummary = session.summary ?? "";
  const summaryResp = await client.chat.completions.create({
    model: compactionModel,
    messages: [
      {
        role: "system",
        content:
          "You summarize prior conversation turns into a terse bullet list capturing " +
          "facts, decisions, and open threads. Output only the summary — no preamble.",
      },
      {
        role: "user",
        content: previousSummary
          ? `Previous summary:\n${previousSummary}\n\nNew turns to fold in:\n${transcript}`
          : `Summarize these turns:\n${transcript}`,
      },
    ],
    max_tokens: 1024,
  });

  session.summary = summaryResp.choices[0]?.message?.content ?? previousSummary;
  session.messages = recent;
}

export default async function main({ params, kv, dicode }: DicodeSdk) {
  const prompt = (await params.get("prompt")) ?? "";
  if (!prompt) throw new Error("prompt param is required");

  // Hybrid session id: use provided or auto-generate
  let sessionId = (await params.get("session_id")) ?? "";
  if (!sessionId) sessionId = crypto.randomUUID();

  const skills = ((await params.get("skills")) ?? "")
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);

  // Provider config. Defaults match task.yaml, but we repeat them here so
  // the task is robust to the yaml defaults not being merged (and so anyone
  // reading task.ts can see the effective defaults in one place).
  const model = (await params.get("model")) || "llama3.2";
  const baseURL = (await params.get("base_url")) || "http://localhost:11434/v1";
  const apiKeyEnv = (await params.get("api_key_env")) || "OLLAMA_API_KEY";
  const systemPromptBase = (await params.get("system_prompt")) ?? "";
  const maxHistoryTokens = Number((await params.get("max_history_tokens")) ?? "80000");
  const compactionModel = (await params.get("compaction_model")) || model;

  // Local runtimes (Ollama, LM Studio) don't authenticate, but the OpenAI SDK
  // requires a non-empty apiKey string. If base_url looks local and the env
  // var isn't set, fall back to a placeholder so the task works out of the
  // box. Hosted providers still require a real key.
  const isLocal = /^https?:\/\/(localhost|127\.0\.0\.1)/i.test(baseURL);
  let apiKey = Deno.env.get(apiKeyEnv);
  if (!apiKey) {
    if (isLocal) {
      apiKey = "ollama"; // placeholder accepted by Ollama, LM Studio, etc.
    } else {
      throw new Error(`${apiKeyEnv} not set in task environment`);
    }
  }

  console.log(
    `ai-agent[${new Date().toISOString()}]: provider → model=${model} ` +
      `baseURL=${baseURL} (local=${isLocal}) session=${sessionId.slice(0, 8)}`,
  );

  const client = new OpenAI({ apiKey, baseURL });

  // Load or init session state from KV
  const key = `chat:${sessionId}`;
  const stored = (await kv.get(key)) as SessionState | null;
  const session: SessionState = stored ?? {
    messages: [],
    created_at: Date.now(),
    updated_at: Date.now(),
  };

  // Build tool list from list_tasks(), filtered by skills
  const allTasks = ((await dicode.list_tasks()) as TaskSummary[]) ?? [];
  const selfId = "buildin/ai-agent";
  const filtered = skills.length
    ? allTasks.filter((t) => skills.includes(t.id))
    : allTasks.filter((t) => t.id !== selfId);

  // Maintain a map from mangled tool name → original task id, so we can
  // resolve tool_calls from the model back to the real task.
  const toolNameToTaskId: Record<string, string> = {};
  const tools = filtered.map((t) => {
    const toolName = taskIdToToolName(t.id);
    toolNameToTaskId[toolName] = t.id;
    return {
      type: "function" as const,
      function: {
        name: toolName,
        description: t.description ?? t.name,
        parameters: paramsToJsonSchema(t.params),
      },
    };
  });

  // Append user turn
  session.messages.push({ role: "user", content: prompt });

  const MAX_TOOL_ITERATIONS = 10;
  let iterations = 0;

  while (iterations++ < MAX_TOOL_ITERATIONS) {
    await compactIfNeeded(session, maxHistoryTokens, client, compactionModel);

    const systemPrompt = session.summary
      ? `${systemPromptBase}\n\nPrior conversation summary:\n${session.summary}`
      : systemPromptBase;

    // deno-lint-ignore no-explicit-any
    const apiMessages: any[] = [
      { role: "system", content: systemPrompt },
      ...session.messages.map((m) => {
        // deno-lint-ignore no-explicit-any
        const out: any = { role: m.role, content: m.content };
        if (m.tool_calls) out.tool_calls = m.tool_calls;
        if (m.tool_call_id) out.tool_call_id = m.tool_call_id;
        if (m.name) out.name = m.name;
        return out;
      }),
    ];

    const resp = await client.chat.completions.create({
      model,
      messages: apiMessages,
      tools: tools.length ? tools : undefined,
      max_tokens: 4096,
    });

    const choice = resp.choices[0]?.message;
    if (!choice) throw new Error("empty response from model");

    session.messages.push({
      role: "assistant",
      content: choice.content ?? "",
      tool_calls: choice.tool_calls as unknown[] | undefined,
    });

    if (!choice.tool_calls || choice.tool_calls.length === 0) {
      break; // terminal assistant turn
    }

    // Execute each tool call and append the result as a "tool" message
    for (const call of choice.tool_calls) {
      if (call.type !== "function") continue;
      const taskId = toolNameToTaskId[call.function.name];
      let result: unknown;
      if (!taskId) {
        result = { error: `unknown tool: ${call.function.name}` };
      } else {
        try {
          const parsed = call.function.arguments
            ? JSON.parse(call.function.arguments)
            : {};
          // dicode.run_task expects Record<string, string> — stringify non-string values
          const stringified: Record<string, string> = {};
          for (const [k, v] of Object.entries(parsed)) {
            stringified[k] = typeof v === "string" ? v : JSON.stringify(v);
          }
          result = await dicode.run_task(taskId, stringified);
        } catch (e) {
          result = { error: e instanceof Error ? e.message : String(e) };
        }
      }
      session.messages.push({
        role: "tool",
        tool_call_id: call.id,
        content: JSON.stringify(result ?? null),
      });
    }
  }

  session.updated_at = Date.now();
  await kv.set(key, session);

  const last = session.messages[session.messages.length - 1];
  const reply = last?.role === "assistant" ? last.content : "";

  // Bare return → dicode serializes as application/json.
  // Do NOT call output.html() here; it would override Content-Type and the
  // browser UI would have to parse HTML instead of JSON.
  return { session_id: sessionId, reply };
}
