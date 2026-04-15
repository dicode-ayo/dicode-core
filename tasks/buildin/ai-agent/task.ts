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

// Map a dicode param type to a JSON Schema type. Unknown types fall back
// to "string" so the tool schema is always valid even for param types we
// don't explicitly recognise.
function dicodeTypeToJsonSchemaType(t: string | undefined): string {
  switch (t) {
    case "number":
    case "integer":
    case "boolean":
      return t;
    case "array":
    case "object":
      return t;
    default:
      return "string";
  }
}

// Convert dicode param list to a JSON Schema object for an OpenAI tool.
function paramsToJsonSchema(
  params: TaskSummary["params"],
): Record<string, unknown> {
  const properties: Record<string, unknown> = {};
  const required: string[] = [];
  if (params) {
    for (const p of params) {
      const prop: Record<string, unknown> = {
        type: dicodeTypeToJsonSchemaType(p.type),
        description: p.description ?? "",
      };
      // JSON Schema requires `items` on array types. We don't have
      // element-type info from dicode params, so fall back to string —
      // the agent can coerce at call time.
      if (prop.type === "array") prop.items = { type: "string" };
      properties[p.name] = prop;
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

interface CompactionConfig {
  maxHistoryTokens: number; // trigger threshold (estimated tokens)
  keepTurns: number;        // last N turns kept verbatim; older turns get summarized
  summaryMaxTokens: number; // max_tokens budget for the summary call
  model: string;            // model used to generate the summary
}

// Strip summarized turns and replace with a single system "summary" entry.
async function compactIfNeeded(
  session: SessionState,
  cfg: CompactionConfig,
  client: OpenAI,
): Promise<void> {
  if (estimateTokens(session.messages, session.summary) <= cfg.maxHistoryTokens) return;

  if (session.messages.length <= cfg.keepTurns) {
    // Budget already exceeded but we have nothing to compact — a single
    // turn is larger than the whole history budget. Log so an operator
    // can diagnose; the next API call will likely fail with a context
    // length error, which is the right signal to the caller.
    console.warn(
      `ai-agent: history over budget (~${estimateTokens(session.messages, session.summary)} tokens > ${cfg.maxHistoryTokens}) ` +
        `but only ${session.messages.length} turns present; skipping compaction. ` +
        `Consider raising max_history_tokens or splitting the prompt.`,
    );
    return;
  }

  const older = session.messages.slice(0, -cfg.keepTurns);
  const recent = session.messages.slice(-cfg.keepTurns);

  const transcript = older
    .map((m) => `${m.role.toUpperCase()}: ${m.content}`)
    .join("\n");

  const previousSummary = session.summary ?? "";
  const summaryResp = await client.chat.completions.create({
    model: cfg.model,
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
    max_tokens: cfg.summaryMaxTokens,
  });

  session.summary = summaryResp.choices[0]?.message?.content ?? previousSummary;
  session.messages = recent;
}

// Whitelist for skill file basenames: alphanumerics, dash, underscore, dot
// (for sub-extensions like "github.push"). No slashes, no leading dot, no
// empty string, no path traversal sequences.
const SKILL_NAME_RE = /^[A-Za-z0-9_][A-Za-z0-9_.\-]*$/;

async function loadSkills(skillsDir: string, names: string[]): Promise<string> {
  if (names.length === 0) return "";
  if (!skillsDir) {
    return `# skills\n(skills_dir is empty; nothing loaded: ${names.join(", ")})`;
  }
  const base = skillsDir.endsWith("/") ? skillsDir : skillsDir + "/";
  const chunks: string[] = [];
  for (const name of names) {
    // Defensive: skill names must be plain filenames. Reject path separators,
    // traversal sequences, empty strings, and anything starting with '.'
    // (blocks `.env`-style probes).
    if (!SKILL_NAME_RE.test(name) || name.includes("..")) {
      chunks.push(`# skill:${name}\n(rejected: invalid skill name)\n`);
      continue;
    }
    const path = `${base}${name}.md`;
    try {
      const body = await Deno.readTextFile(path);
      chunks.push(`# skill:${name}\n${body.trim()}`);
    } catch (e) {
      chunks.push(
        `# skill:${name}\n(not loaded: ${e instanceof Error ? e.message : String(e)})`,
      );
    }
  }
  return chunks.join("\n\n");
}

interface NotConfiguredResponse {
  session_id: string;
  reply: null;
  error: "not_configured";
  missing: string[];
  hint: string;
}

export default async function main({ params, kv, dicode }: DicodeSdk) {
  const prompt = (await params.get("prompt")) ?? "";
  if (!prompt) throw new Error("prompt param is required");

  // Hybrid session id: use provided or auto-generate
  let sessionId = (await params.get("session_id")) ?? "";
  if (!sessionId) sessionId = crypto.randomUUID();

  // Tools: dicode task ids the agent may call. Empty = all except self.
  const toolFilter = ((await params.get("tools")) ?? "")
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);

  // Skills: md file names (without .md) to concatenate into the system prompt.
  const skillNames = ((await params.get("skills")) ?? "")
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);

  // Provider config. No defaults in task.ts — the buildin is generic and
  // restrictive; provider-specific sibling tasks set defaults via taskset
  // overrides. If required fields are missing, return a structured
  // not_configured response instead of throwing.
  const model = (await params.get("model")) ?? "";
  const baseURL = (await params.get("base_url")) ?? "";
  const apiKeyEnv = (await params.get("api_key_env")) ?? "";
  const systemPromptBase = (await params.get("system_prompt")) ?? "";
  const maxHistoryTokens = Number(await params.get("max_history_tokens"));
  const compactionModel = (await params.get("compaction_model")) || model;

  // Tunables — all have task.yaml defaults, no fallbacks here. Fail loud
  // if the caller sends empty/non-numeric values rather than silently
  // substituting different magic numbers from two sources.
  const maxToolIterations = Number(await params.get("max_tool_iterations"));
  const responseMaxTokens = Number(await params.get("response_max_tokens"));
  const compactionMaxTokens = Number(await params.get("compaction_max_tokens"));
  const compactionKeepTurns = Number(await params.get("compaction_keep_turns"));

  const missing: string[] = [];
  if (!model) missing.push("model");
  if (!baseURL) missing.push("base_url");

  if (missing.length > 0) {
    const response: NotConfiguredResponse = {
      session_id: sessionId,
      reply: null,
      error: "not_configured",
      missing,
      hint:
        "This is the generic ai-agent buildin. It has no provider configured. " +
        "Either pass model/base_url/api_key_env as params, or use a " +
        "provider-specific sibling task (e.g. examples/ai-agent-ollama).",
    };
    return response;
  }

  // API key resolution is purely param-driven, with no URL sniffing.
  //
  //   api_key_env == ""      → provider does not authenticate (Ollama,
  //                             LM Studio, any OpenAI-compat server that
  //                             ignores the key). Pass a non-empty literal
  //                             because the OpenAI SDK rejects "".
  //   api_key_env == "FOO"   → FOO must be present in the task env, else
  //                             throw. No fallback, no URL-based exceptions
  //                             — the caller explicitly asked for a key.
  let apiKey: string;
  if (!apiKeyEnv) {
    apiKey = "unused";
  } else {
    const fromEnv = Deno.env.get(apiKeyEnv);
    if (!fromEnv) {
      throw new Error(
        `${apiKeyEnv} not set in task environment (api_key_env="${apiKeyEnv}"). ` +
          `For providers that don't authenticate, leave api_key_env empty.`,
      );
    }
    apiKey = fromEnv;
  }

  // Load skills eagerly and splice them into the system prompt. The
  // directory comes from the skills_dir param, whose default is populated
  // by template expansion at task-load time (${TASK_SET_DIR}/../skills by
  // default; see docs/task-template-vars.md).
  const skillsDir = (await params.get("skills_dir")) ?? "";
  const skillsBlob = await loadSkills(skillsDir, skillNames);

  console.log(
    `ai-agent[${new Date().toISOString()}]: task=${dicode.task_id} ` +
      `run=${dicode.run_id.slice(0, 8)} model=${model} baseURL=${baseURL} ` +
      `session=${sessionId.slice(0, 8)} tools=${toolFilter.length || "*"} ` +
      `skills=${skillNames.length}`,
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

  // Build tool list from list_tasks(), filtered by toolFilter. When no
  // explicit allowlist is supplied we exclude exactly this task's own id
  // (sourced from the SDK handshake) to prevent one-step self-recursion.
  // Presets that reuse this task.ts under a different id (ai-agent-ollama,
  // ai-agent-openai, …) get the correct exclusion for free because each
  // run reports its own namespaced task_id.
  const allTasks = ((await dicode.list_tasks()) as TaskSummary[]) ?? [];
  const selfID = dicode.task_id;
  const filtered = toolFilter.length
    ? allTasks.filter((t) => toolFilter.includes(t.id))
    : allTasks.filter((t) => t.id !== selfID);

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

  const compactionCfg: CompactionConfig = {
    maxHistoryTokens,
    keepTurns: compactionKeepTurns,
    summaryMaxTokens: compactionMaxTokens,
    model: compactionModel,
  };

  let iterations = 0;
  while (iterations++ < maxToolIterations) {
    await compactIfNeeded(session, compactionCfg, client);

    const parts: string[] = [systemPromptBase];
    if (skillsBlob) parts.push(skillsBlob);
    if (session.summary) parts.push(`Prior conversation summary:\n${session.summary}`);
    const systemPrompt = parts.filter(Boolean).join("\n\n");

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
      max_tokens: responseMaxTokens,
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
