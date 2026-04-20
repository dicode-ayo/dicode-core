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

// Parse a numeric param into a finite positive integer, throwing a clear
// error on anything else. Every numeric tunable has a task.yaml default,
// but the runtime's param-merge only applies defaults when the param is
// absent — a caller passing "" or "garbage" overrides the default and
// Number() would collapse it to 0 or NaN, silently breaking the agent
// (a `while (0 < NaN)` loop never runs; a `<= NaN` compaction check never
// fires). Fail loud so misconfigurations can't masquerade as empty replies
// or runaway history growth.
function parsePositiveInt(raw: string | null, name: string): number {
  const n = Number(raw);
  if (!Number.isFinite(n) || n <= 0 || Math.floor(n) !== n) {
    throw new Error(
      `ai-agent: param ${name} must be a positive integer, got ${JSON.stringify(raw)}`,
    );
  }
  return n;
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
    // Loud: a request for skills with no directory configured is almost
    // certainly a misconfiguration, not a user expectation. The model
    // also sees a clear marker so it doesn't hallucinate around it.
    console.error(
      `ai-agent: skills requested but skills_dir is empty; nothing loaded: ${names.join(", ")}`,
    );
    return `# skills\n(skills_dir is empty; nothing loaded: ${names.join(", ")})`;
  }
  const base = skillsDir.endsWith("/") ? skillsDir : skillsDir + "/";
  const chunks: string[] = [];
  for (const name of names) {
    // Defensive: skill names must be plain filenames. Reject path separators,
    // traversal sequences, empty strings, and anything starting with '.'
    // (blocks `.env`-style probes).
    if (!SKILL_NAME_RE.test(name) || name.includes("..")) {
      console.error(`ai-agent: rejected invalid skill name ${JSON.stringify(name)}`);
      chunks.push(`# skill:${name}\n(rejected: invalid skill name)\n`);
      continue;
    }
    const path = `${base}${name}.md`;
    try {
      const body = await Deno.readTextFile(path);
      chunks.push(`# skill:${name}\n${body.trim()}`);
    } catch (e) {
      // Log the full path so operators can distinguish a user typo
      // (wrong name) from a permissions/path misconfig (wrong skills_dir).
      const msg = e instanceof Error ? e.message : String(e);
      console.error(`ai-agent: failed to load skill ${name} from ${path}: ${msg}`);
      chunks.push(
        `# skill:${name}\n(not loaded: ${msg})`,
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
  // Self-identity check before anything else. dicode.task_id is populated
  // from the IPC handshake and is used below to exclude this task from its
  // own tool list. An empty value would silently turn that filter into a
  // no-op, letting the agent call itself as a tool and recurse up to
  // max_tool_iterations deep per turn. Fail loud instead.
  if (!dicode.task_id) {
    throw new Error(
      "ai-agent: dicode.task_id is empty — refusing to run without a self-identity. " +
        "This indicates the IPC handshake did not populate task_id; check the dicode daemon version.",
    );
  }

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
  const compactionModel = (await params.get("compaction_model")) || model;

  // Tunables. Every one has a task.yaml default, but the runtime merges
  // defaults only when the param was absent — a caller passing an explicit
  // empty or non-numeric value overrides the default and Number() collapses
  // it to 0 or NaN, which then silently breaks the loop (e.g.
  // `while(0 < NaN)` never runs, `estimateTokens <= NaN` never compacts).
  // Validate explicitly so a bad param surfaces as a loud error instead of
  // an empty reply or runaway history.
  const maxHistoryTokens = parsePositiveInt(await params.get("max_history_tokens"), "max_history_tokens");
  const maxToolIterations = parsePositiveInt(await params.get("max_tool_iterations"), "max_tool_iterations");
  const responseMaxTokens = parsePositiveInt(await params.get("response_max_tokens"), "response_max_tokens");
  const compactionMaxTokens = parsePositiveInt(await params.get("compaction_max_tokens"), "compaction_max_tokens");
  const compactionKeepTurns = parsePositiveInt(await params.get("compaction_keep_turns"), "compaction_keep_turns");

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

  // The entire agent loop is wrapped in try/finally so the session — and
  // all the tool round-trips it successfully completed — is persisted to
  // KV even if an API call, tool dispatch, or compaction throws. Before
  // this guard, a single rate-limit blip mid-loop silently discarded the
  // whole turn plus any prior successful tool calls.
  try {
    let iterations = 0;
    while (iterations++ < maxToolIterations) {
      // A compaction failure (e.g. the summary model is rate-limited) is
      // non-fatal: the next request will likely surface a context-length
      // error from the main model, which is a clearer signal than tearing
      // down the whole turn here. Log loudly so operators can diagnose.
      try {
        await compactIfNeeded(session, compactionCfg, client);
      } catch (e) {
        console.error(
          `ai-agent: compaction failed, continuing uncompacted: ${e instanceof Error ? e.message : String(e)}`,
        );
      }

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

      let resp;
      try {
        resp = await client.chat.completions.create({
          model,
          messages: apiMessages,
          tools: tools.length ? tools : undefined,
          max_tokens: responseMaxTokens,
        });
      } catch (e) {
        // OpenAI SDK APIError carries the parsed response body and rate-limit
        // headers. Log them before rethrowing so operators can distinguish
        // "our-key quota exhausted" (x-ratelimit-remaining:0) from "upstream
        // provider 429/503" (message contains "Provider returned error") from
        // "malformed request" (400 + schema details).
        const err = e as {
          status?: number;
          message?: string;
          error?: unknown;
          headers?: Record<string, string>;
        };
        if (err && typeof err === "object" && err.status) {
          const rlHeaders: Record<string, string> = {};
          if (err.headers) {
            for (const [k, v] of Object.entries(err.headers)) {
              if (/^(x-ratelimit-|retry-after$)/i.test(k)) rlHeaders[k] = v;
            }
          }
          console.error(
            `ai-agent: upstream ${err.status} — body=${JSON.stringify(err.error)} ` +
              `rlHeaders=${JSON.stringify(rlHeaders)}`,
          );
        }
        throw e;
      }

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

      // Execute each tool call and append the result as a "tool" message.
      // Tool failures are caught per-call so one broken tool can't derail
      // the whole turn, but we also console.error the failure so the
      // operator sees which tool broke. The model still receives the
      // error as a structured result and can recover on the next turn.
      for (const call of choice.tool_calls) {
        if (call.type !== "function") continue;
        const taskId = toolNameToTaskId[call.function.name];
        let result: unknown;
        if (!taskId) {
          console.error(`ai-agent: unknown tool '${call.function.name}' requested`);
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
            const msg = e instanceof Error ? e.message : String(e);
            console.error(`ai-agent: tool call failed task=${taskId} call=${call.id}: ${msg}`);
            result = { error: msg };
          }
        }
        session.messages.push({
          role: "tool",
          tool_call_id: call.id,
          content: JSON.stringify(result ?? null),
        });
      }
    }
  } finally {
    session.updated_at = Date.now();
    // Best-effort persistence. If KV itself is broken the task was going
    // to fail anyway, and we don't want the KV error to shadow the real
    // error from the loop body. Log and move on.
    try {
      await kv.set(key, session);
    } catch (e) {
      console.error(
        `ai-agent: failed to persist session ${sessionId.slice(0, 8)} to KV: ${e instanceof Error ? e.message : String(e)}`,
      );
    }
  }

  const last = session.messages[session.messages.length - 1];
  const reply = last?.role === "assistant" ? last.content : "";

  // Bare return → dicode serializes as application/json.
  // Do NOT call output.html() here; it would override Content-Type and the
  // browser UI would have to parse HTML instead of JSON.
  return { session_id: sessionId, reply };
}
