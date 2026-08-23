// AI Agent built-in task.
//
// Calls an OpenAI-compatible chat completions API (OpenAI, Anthropic via
// openai-compat, Ollama, LM Studio, Together, ...) with a tool-use loop that
// lets the model call other dicode tasks as tools.
//
// The task bare-returns { session_id, reply } so browser UIs can parse it
// as application/json. Never call output.html() here — that would override
// the content type.
//
// Two shapes over one turn core (runAgentTurn), sharing the suspend/resume
// envelope with the other ai-agent presets:
//   - One-shot (a non-empty `prompt`): run a single stateless turn and return
//     { session_id, reply }. No conversation is carried between one-shot calls.
//   - Chat loop (blank `prompt` on a fresh run): suspend to a `turn` step that
//     collects a message, runs one turn, and suspends back. The conversation
//     rides in the suspend `state` blob (resume_state), not KV. A blank message
//     ends it.
import OpenAI from "npm:openai@4";
import type { Dicode, DicodeSdk } from "../../sdk.ts";
import { chatStart, chatTurn, decideEntryMode, resolveSessionId } from "../ai-agent-core/chat.ts";

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

// Temperature admits 0 (the default, and the value that keeps a small model
// emitting structured tool calls rather than prose), so it cannot go through
// parsePositiveInt.
function parseTemperature(raw: string | null): number {
  // An absent param is the declared default, not a coercion of null.
  const n = raw === null || raw === "" ? 0 : Number(raw);
  if (!Number.isFinite(n) || n < 0 || n > 2) {
    throw new Error(
      `ai-agent: param temperature must be a number between 0 and 2, got ${JSON.stringify(raw)}`,
    );
  }
  return n;
}

type SkillsMode = "index" | "eager";

// An unrecognised skills_mode throws rather than falling back to a default: the
// two modes differ in what the model is told, and silently picking one would
// surface as bad output rather than as a configuration error.
function parseSkillsMode(raw: string | null): SkillsMode {
  const v = (raw ?? "").trim().toLowerCase() || "index";
  if (v !== "index" && v !== "eager") {
    throw new Error(
      `ai-agent: param skills_mode must be "index" or "eager", got ${JSON.stringify(raw)}`,
    );
  }
  return v;
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

// ── built-in tools ────────────────────────────────────────────────────────────
//
// Tools backed by this task's own dicode SDK rather than by firing another
// dicode task. Each one is gated on the capability its SDK call needs: a
// built-in is offered only when the run's grant carries `cap`, so a capability
// the taskset never declared stays invisible instead of surfacing as a
// permission error the model discovers by calling it.
//
// The table covers the authoring operations. Secrets, crypto, audit and the
// run-input retention sweep (list_expired / delete_input) stay off it: those
// belong to the task process, not to the model driving it.

// Values a built-in needs that come from the task's own params rather than from
// the model. A boundary the model can set alongside the value it bounds is not
// a boundary, so anything of that shape lives here.
interface BuiltinConfig {
  gitBranchPrefix: string;
  skills: Record<string, Skill>;
}

interface BuiltinTool {
  name: string;
  description: string;
  parameters: Record<string, unknown>;
  run(dicode: Dicode, args: Record<string, unknown>, cfg: BuiltinConfig): Promise<unknown>;
}

// A built-in offered only when the run's grant carries `cap`. Built-ins whose
// gate is something other than a dicode capability are plain BuiltinTools
// registered on their own terms.
interface CapBuiltinTool extends BuiltinTool {
  cap: string;
}

type SchemaProps = Record<string, Record<string, unknown>>;

function objectSchema(properties: SchemaProps, required: string[]): Record<string, unknown> {
  return { type: "object", properties, required, additionalProperties: false };
}

// Tool arguments arrive as whatever JSON the model emitted, so each accessor
// coerces rather than trusting the declared schema.
function argStr(args: Record<string, unknown>, key: string, fallback = ""): string {
  const v = args[key];
  if (v === undefined || v === null) return fallback;
  return typeof v === "string" ? v : String(v);
}

function argBool(args: Record<string, unknown>, key: string): boolean {
  const v = args[key];
  return typeof v === "string" ? v === "true" : Boolean(v);
}

function argStrList(args: Record<string, unknown>, key: string): string[] {
  const v = args[key];
  if (Array.isArray(v)) return v.map((e) => (typeof e === "string" ? e : String(e)));
  if (typeof v === "string" && v) return v.split(",").map((e) => e.trim()).filter(Boolean);
  return [];
}

function argLimit(args: Record<string, unknown>, key: string, fallback: number): number {
  const n = Number(args[key]);
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : fallback;
}

const BUILTIN_TOOLS: CapBuiltinTool[] = [
  {
    name: "dicode_list_tasks",
    cap: "tasks.list",
    description:
      "List every task registered in this dicode daemon, with each task's id, " +
      "name, description and parameters. Use it to find an existing task to " +
      "read as a pattern before writing a new one.",
    parameters: objectSchema({}, []),
    run: (dicode) => dicode.list_tasks(),
  },
  {
    name: "dicode_get_runs",
    cap: "runs.list",
    description:
      "Fetch recent runs of a task — status, timing and output — newest first. " +
      "Use it to see how a task behaved before changing it.",
    parameters: objectSchema({
      task_id: { type: "string", description: "Namespaced task id, e.g. 'buildin/git-pr'." },
      limit: { type: "integer", description: "How many runs to return (default 10)." },
    }, ["task_id"]),
    run: (dicode, args) =>
      dicode.get_runs(argStr(args, "task_id"), { limit: argLimit(args, "limit", 10) }),
  },
  {
    name: "dicode_test_task",
    cap: "tasks.test",
    description:
      "Run a task's sibling test file (task.test.ts / task.test.py) and return " +
      "the results. Refused for a task the approval gate is still holding " +
      "pending, because the test file runs with full host permissions.",
    parameters: objectSchema({
      task_id: { type: "string", description: "Namespaced task id to test." },
    }, ["task_id"]),
    run: (dicode, args) => dicode.tasks.test(argStr(args, "task_id")),
  },
  {
    name: "dicode_list_sources",
    cap: "sources.list",
    description:
      "List the configured sources: name, type, git URL, tracked branch, and " +
      "whether the source is already in dev mode. Use it to find the source " +
      "name dicode_set_dev_mode expects. Host paths are not returned; " +
      "dicode_set_dev_mode is what hands back a path to edit in.",
    parameters: objectSchema({}, []),
    run: (dicode) => dicode.sources.list(),
  },
  {
    name: "dicode_set_dev_mode",
    cap: "sources.set_dev_mode",
    description:
      "Enter or leave dev mode on a source. Entering with a branch clones the " +
      "source repo into a scratch directory and returns clone_path — edit files " +
      "there, not in the live source. The clone belongs to this session; leave " +
      "dev mode when done, which removes it locally and keeps the remote branch.",
    parameters: objectSchema({
      source: { type: "string", description: "Source name as it appears in dicode.yaml." },
      enabled: { type: "boolean", description: "true to enter dev mode, false to leave it." },
      branch: { type: "string", description: "Branch to check out in the clone." },
      base: { type: "string", description: "Branch to fork from when `branch` does not exist remotely." },
    }, ["source", "enabled"]),
    // Clone mode only. run_id names the clone directory and is fixed to this run
    // so two sessions cannot collide on one clone, and so leaving dev mode
    // reaches the same one. local_path is withheld: it redirects the daemon's
    // taskset resolution at an arbitrary path on the host, which would let a
    // caller decide what the daemon loads as tasks.
    run: (dicode, args) =>
      dicode.sources.set_dev_mode(argStr(args, "source"), {
        enabled: argBool(args, "enabled"),
        branch: argStr(args, "branch"),
        base: argStr(args, "base"),
        run_id: dicode.run_id,
      }),
  },
  {
    name: "dicode_get_run_input",
    cap: "runs.get_input",
    description:
      "Read the stored trigger payload of a run, plus the names of any fields " +
      "redaction removed. Reason from the logs alone when a field you need is " +
      "listed as redacted — the value is gone, not hidden.",
    parameters: objectSchema({
      run_id: { type: "string", description: "Id of the run whose input to read." },
    }, ["run_id"]),
    run: (dicode, args) => dicode.runs.get_input(argStr(args, "run_id")),
  },
  {
    name: "dicode_pin_run_input",
    cap: "runs.pin_input",
    description:
      "Exempt a run's stored input from retention sweeps so it survives while " +
      "you work on it. Unpin it when you are done.",
    parameters: objectSchema({
      run_id: { type: "string", description: "Id of the run whose input to pin." },
    }, ["run_id"]),
    run: (dicode, args) => dicode.runs.pin_input(argStr(args, "run_id")),
  },
  {
    name: "dicode_unpin_run_input",
    cap: "runs.unpin_input",
    description: "Return a pinned run input to normal retention.",
    parameters: objectSchema({
      run_id: { type: "string", description: "Id of the run whose input to unpin." },
    }, ["run_id"]),
    run: (dicode, args) => dicode.runs.unpin_input(argStr(args, "run_id")),
  },
  {
    name: "dicode_replay_run",
    cap: "runs.replay",
    description:
      "Re-fire a past run against its stored input and return the new run id. " +
      "Use it to check a fix against the payload that originally failed. The " +
      "replay is a fresh run: poll it with dicode_get_runs for its outcome.",
    parameters: objectSchema({
      run_id: { type: "string", description: "Id of the run to replay." },
      task_id: { type: "string", description: "Retarget the replay at a different task. Omit to replay the run's own task." },
    }, ["run_id"]),
    run: (dicode, args) =>
      dicode.runs.replay(argStr(args, "run_id"), argStr(args, "task_id") || undefined),
  },
  {
    name: "dicode_git_commit_push",
    cap: "git.commit_push",
    description:
      "Commit the working tree of a source's repo and push it to a branch, " +
      "returning the commit hash. The branch must start with the prefix this " +
      "agent is configured to push under; main and master cannot be pushed to.",
    parameters: objectSchema({
      source_id: { type: "string", description: "Source name as it appears in dicode.yaml." },
      message: { type: "string", description: "Commit message." },
      branch: { type: "string", description: "Branch to push to. Must start with the agent's configured branch prefix." },
      files: { type: "array", items: { type: "string" }, description: "Paths to stage. Omit to stage everything changed." },
      author_name: { type: "string", description: "Commit author name. Defaults to this agent." },
      author_email: { type: "string", description: "Commit author email. Defaults to this agent." },
      auth_token_env: { type: "string", description: "Env var holding the push token. Must be declared in this task's permissions.env." },
    }, ["source_id", "message", "branch"]),
    // allow_main is withheld: it is a per-call branch-protection bypass, not a
    // capability, and the branch to protect is not the caller's decision.
    // branch_prefix comes from config for the same reason — as an argument it
    // would be the caller bounding the caller.
    run: (dicode, args, cfg) =>
      dicode.git.commit_push(argStr(args, "source_id"), {
        message: argStr(args, "message"),
        branch: argStr(args, "branch"),
        branch_prefix: cfg.gitBranchPrefix,
        files: argStrList(args, "files"),
        author_name: argStr(args, "author_name", `dicode ${dicode.task_id}`),
        author_email: argStr(args, "author_email", "noreply@dicode.local"),
        auth_token_env: argStr(args, "auth_token_env"),
      }),
  },
];

// grantedBuiltins selects the built-ins this run may actually call. An empty
// `caps` — a daemon too old to report them — yields none.
function grantedBuiltins(caps: string[] | undefined): CapBuiltinTool[] {
  const granted = new Set(caps ?? []);
  return BUILTIN_TOOLS.filter((t) => granted.has(t.cap));
}

interface CompactionConfig {
  maxHistoryTokens: number; // trigger threshold (estimated tokens)
  keepTurns: number;        // last N turns kept verbatim; older turns get summarized
  summaryMaxTokens: number; // max_tokens budget for the summary call
  model: string;            // model used to generate the summary
  temperature: number;      // sampling temperature for the summary call
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
    temperature: cfg.temperature,
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

// ── skills ────────────────────────────────────────────────────────────────────
//
// A skill is a markdown file under skills_dir, named by the `skills` param.
// Its YAML frontmatter carries the one-line description the index advertises;
// the body is what dicode_read_skill hands back.

// Whitelist for skill file basenames: alphanumerics, dash, underscore, dot
// (for sub-extensions like "github.push"). No slashes, no leading dot, no
// empty string, no path traversal sequences.
const SKILL_NAME_RE = /^[A-Za-z0-9_][A-Za-z0-9_.\-]*$/;

// The index carries one line per skill, so a description long enough to wrap
// defeats the point of indexing at all.
const SKILL_DESCRIPTION_MAX = 300;

interface Skill {
  name: string;
  description: string;
  body: string;
  // Set when the file could not be read. The skill still appears in the index
  // and still answers dicode_read_skill — a model told a skill exists and then
  // silently denied it burns a turn guessing at what it said.
  error?: string;
}

const FRONTMATTER_RE = /^---\r?\n([\s\S]*?)\r?\n---[^\S\r\n]*(?:\r?\n|$)/;

function clip(s: string, max: number): string {
  return s.length <= max ? s : s.slice(0, max - 1).trimEnd() + "…";
}

// Read one top-level key out of a skill's YAML frontmatter. The frontmatter is
// a flat key/value block and this needs exactly two of its keys, so it reads
// them directly rather than pulling in a YAML parser. A folded or literal block
// scalar continues onto the following lines, which are joined back into one.
function frontmatterValue(block: string, key: string): string {
  const lines = block.split(/\r?\n/);
  const head = new RegExp(`^${key}:[^\\S\\r\\n]*(.*)$`);
  for (let i = 0; i < lines.length; i++) {
    const m = head.exec(lines[i]);
    if (!m) continue;
    // A lone block indicator ("|", ">-", …) means the value starts on the next
    // line; anything else on that line is the value itself.
    const parts = [/^[>|][-+0-9]*$/.test(m[1].trim()) ? "" : m[1]];
    for (let j = i + 1; j < lines.length && !/^[A-Za-z_][\w.\-]*[^\S\r\n]*:/.test(lines[j]); j++) {
      parts.push(lines[j].trim());
    }
    return parts.join(" ").trim();
  }
  return "";
}

// Fallback description for a skill with no frontmatter: its first line of prose,
// heading markers stripped.
function firstProse(body: string): string {
  for (const line of body.split(/\r?\n/)) {
    const t = line.replace(/^#+\s*/, "").trim();
    if (t) return t;
  }
  return "";
}

function parseSkill(name: string, text: string): Skill {
  const m = FRONTMATTER_RE.exec(text);
  const body = (m ? text.slice(m[0].length) : text).trim();
  const description = (m ? frontmatterValue(m[1], "description") : "") || firstProse(body);
  return { name, description, body };
}

async function loadSkills(skillsDir: string, names: string[]): Promise<Skill[]> {
  if (names.length === 0) return [];
  if (!skillsDir) {
    // Loud: a request for skills with no directory configured is almost
    // certainly a misconfiguration, not a user expectation.
    console.error(
      `ai-agent: skills requested but skills_dir is empty; nothing loaded: ${names.join(", ")}`,
    );
    return names.map((name) => ({ name, description: "", body: "", error: "skills_dir is empty" }));
  }
  const base = skillsDir.endsWith("/") ? skillsDir : skillsDir + "/";
  const skills: Skill[] = [];
  for (const name of names) {
    // Defensive: skill names must be plain filenames. Reject path separators,
    // traversal sequences, empty strings, and anything starting with '.'
    // (blocks `.env`-style probes).
    if (!SKILL_NAME_RE.test(name) || name.includes("..")) {
      console.error(`ai-agent: rejected invalid skill name ${JSON.stringify(name)}`);
      skills.push({ name, description: "", body: "", error: "invalid skill name" });
      continue;
    }
    const path = `${base}${name}.md`;
    try {
      skills.push(parseSkill(name, await Deno.readTextFile(path)));
    } catch (e) {
      // Log the full path so operators can distinguish a user typo
      // (wrong name) from a permissions/path misconfig (wrong skills_dir).
      const msg = e instanceof Error ? e.message : String(e);
      console.error(`ai-agent: failed to load skill ${name} from ${path}: ${msg}`);
      // The reason reaches the model, so it says which of the two it was
      // without the host path the raw error carries. Host paths are not the
      // model's to see — the same reason sources.list strips them and
      // set_dev_mode withholds local_path.
      const reason = e instanceof Deno.errors.NotFound ? "no such skill file" : "unreadable";
      skills.push({ name, description: "", body: "", error: reason });
    }
  }
  return skills;
}

// Every body inline, the shape skills_mode "eager" keeps.
function skillsEagerBlob(skills: Skill[]): string {
  return skills
    .map((s) => `# skill:${s.name}\n${s.error ? `(not loaded: ${s.error})` : s.body}`)
    .join("\n\n");
}

const READ_SKILL_TOOL_NAME = "dicode_read_skill";

// Name and description per skill, bodies behind dicode_read_skill.
function skillsIndexBlob(skills: Skill[]): string {
  const lines = skills.map((s) =>
    `- ${s.name} — ${
      s.error ? `(not loaded: ${s.error})` : clip(s.description || "(no description)", SKILL_DESCRIPTION_MAX)
    }`
  );
  return [
    "# Skills",
    "",
    `These skills carry the rules, schemas and workflows this daemon expects you to`,
    `follow. Call ${READ_SKILL_TOOL_NAME} for every skill whose subject touches the`,
    `request and read what it returns before you write a file or call another tool.`,
    `Their contents are not guessable from these descriptions.`,
    "",
    ...lines,
  ].join("\n");
}

// Skills are advertised and fetched on demand rather than spliced into every
// request. A skill's full text outweighs the operator's own system prompt —
// measured on an 8B model, eager-loading 22 KB of skill took a correct task
// manifest from 8/8 to 0/8 while leaving the tool-call protocol untouched: the
// model imitates the skill's examples instead of following its instructions
// (#757). The cost is also per-turn, since the system prompt is rebuilt on
// every iteration of the tool loop.
//
// Unlike BUILTIN_TOOLS this one is not gated on a dicode capability: it hands
// back files the operator's own `skills` param already named, which eager mode
// puts in the prompt unconditionally.
const READ_SKILL_TOOL: BuiltinTool = {
  name: READ_SKILL_TOOL_NAME,
  description:
    "Read the full text of one of the skills listed under '# Skills' in the " +
    "system prompt. Skills carry the schemas and mandatory workflows for this " +
    "daemon; read the relevant one before writing files or calling other tools.",
  parameters: objectSchema({
    name: { type: "string", description: "Skill name exactly as listed under '# Skills'." },
  }, ["name"]),
  // deno-lint-ignore require-await
  run: async (_dicode, args, cfg) => {
    const name = argStr(args, "name");
    const skill = cfg.skills[name];
    if (!skill) return { error: `unknown skill: ${name}`, available: Object.keys(cfg.skills) };
    if (skill.error) return { error: `skill ${name} not loaded: ${skill.error}` };
    return { name: skill.name, description: skill.description, body: skill.body };
  },
};

interface NotConfiguredResponse {
  session_id: string;
  reply: string;
  error: "not_configured";
  missing: string[];
  hint: string;
  task_dir: string;
}

// requireTaskId fails loud when the IPC handshake didn't populate task_id.
// dicode.task_id excludes this task from its own tool list; an empty value
// would silently turn that filter into a no-op, letting the agent call itself
// as a tool and recurse up to max_tool_iterations deep per turn.
function requireTaskId(dicode: Dicode): void {
  if (!dicode.task_id) {
    throw new Error(
      "ai-agent: dicode.task_id is empty — refusing to run without a self-identity. " +
        "This indicates the IPC handshake did not populate task_id; check the dicode daemon version.",
    );
  }
}

// AgentRuntime bundles everything one turn needs. Resolved fresh per run from
// params — each suspend/resume re-enters the file, so nothing is cached.
interface AgentRuntime {
  client: OpenAI;
  model: string;
  systemPromptBase: string;
  skillsBlob: string;
  // deno-lint-ignore no-explicit-any
  tools: any[];
  toolNameToTaskId: Record<string, string>;
  builtins: Record<string, BuiltinTool>;
  builtinCfg: BuiltinConfig;
  compactionCfg: CompactionConfig;
  maxToolIterations: number;
  responseMaxTokens: number;
  temperature: number;
  skillsMode: SkillsMode;
}

type ResolveResult =
  | { ok: true; runtime: AgentRuntime }
  | { ok: false; missing: string[]; hint: string };

// resolveAgentRuntime reads the provider config + tunables from params, resolves
// the API key, loads skills, and builds the tool list. Returns a structured
// not_configured result when model/base_url are missing (the caller shapes the
// response); throws on a bad tunable or a missing api key env var.
async function resolveAgentRuntime(
  params: DicodeSdk["params"],
  dicode: Dicode,
): Promise<ResolveResult> {
  requireTaskId(dicode);

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
  const temperature = parseTemperature(await params.get("temperature"));
  const compactionKeepTurns = parsePositiveInt(await params.get("compaction_keep_turns"), "compaction_keep_turns");

  const missing: string[] = [];
  if (!model) missing.push("model");
  if (!baseURL) missing.push("base_url");

  if (missing.length > 0) {
    return {
      ok: false,
      missing,
      hint:
        "This is the generic ai-agent buildin. It has no provider configured. " +
        "Either pass model/base_url/api_key_env as params, or use a " +
        "provider-specific sibling task (e.g. examples/ai-agent-ollama).",
    };
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

  // Load the skills named by the `skills` param. The directory comes from the
  // skills_dir param, whose default is populated by template expansion at
  // task-load time (${TASK_SET_DIR}/../skills by default; see
  // docs/task-template-vars.md). What reaches the system prompt is either the
  // index or every body, per skills_mode.
  const skillsDir = (await params.get("skills_dir")) ?? "";
  const skillsMode = parseSkillsMode(await params.get("skills_mode"));
  const skills = await loadSkills(skillsDir, skillNames);
  const skillsBlob = skills.length === 0
    ? ""
    : skillsMode === "eager"
    ? skillsEagerBlob(skills)
    : skillsIndexBlob(skills);

  const gitBranchPrefix = (await params.get("git_branch_prefix")) ?? "";

  const client = new OpenAI({ apiKey, baseURL });

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

  // Built-ins carry a dicode_ prefix and task tools a task_ one, so the two
  // families cannot collide however a task id mangles.
  const builtins: Record<string, BuiltinTool> = {};
  for (const b of grantedBuiltins(dicode.caps)) {
    builtins[b.name] = b;
    tools.push({
      type: "function" as const,
      function: { name: b.name, description: b.description, parameters: b.parameters },
    });
  }

  // The index is only actionable with the lookup tool alongside it, so the two
  // are registered together. Eager mode already carries every body in the
  // prompt and offering the tool there would just re-send them.
  if (skillsMode === "index" && skills.length > 0) {
    builtins[READ_SKILL_TOOL.name] = READ_SKILL_TOOL;
    tools.push({
      type: "function" as const,
      function: {
        name: READ_SKILL_TOOL.name,
        description: READ_SKILL_TOOL.description,
        parameters: READ_SKILL_TOOL.parameters,
      },
    });
  }

  // Counts come from the built list, so the line reports what the model was
  // actually handed.
  console.log(
    `ai-agent[${new Date().toISOString()}]: task=${dicode.task_id} ` +
      `run=${dicode.run_id.slice(0, 8)} model=${model} baseURL=${baseURL} ` +
      `task_tools=${filtered.length} builtins=${Object.keys(builtins).length} ` +
      `skills=${skills.length} skills_mode=${skillsMode} ` +
      `skills_chars=${skillsBlob.length}`,
  );

  return {
    ok: true,
    runtime: {
      client,
      model,
      systemPromptBase,
      skillsBlob,
      tools,
      toolNameToTaskId,
      builtins,
      builtinCfg: {
        gitBranchPrefix,
        skills: Object.fromEntries(skills.map((s) => [s.name, s])),
      },
      compactionCfg: {
        maxHistoryTokens,
        keepTurns: compactionKeepTurns,
        summaryMaxTokens: compactionMaxTokens,
        model: compactionModel,
        temperature,
      },
      maxToolIterations,
      responseMaxTokens,
      temperature,
      skillsMode,
    },
  };
}

// runAgentTurn runs the tool-use loop for a single user message against a
// resolved runtime, mutating `session` in place and returning the reply. The
// caller owns the session's lifetime: one-shot runs a fresh empty session; the
// chat loop carries it forward in resume_state.
async function runAgentTurn(
  session: SessionState,
  message: string,
  rt: AgentRuntime,
  dicode: Dicode,
): Promise<string> {
  const { client, model, systemPromptBase, skillsBlob, skillsMode, tools, toolNameToTaskId, builtins, builtinCfg, compactionCfg, maxToolIterations, responseMaxTokens, temperature } = rt;

  // Append user turn
  session.messages.push({ role: "user", content: message });

  // Wrap the loop in try/finally so `session` is stamped with updated_at on
  // exit even when an API call, tool dispatch, or compaction throws mid-loop.
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

      // The index goes before the operator's own prompt, so the operator's
      // instructions are the last thing the model reads before the request.
      // Measured on an 8B model, n=6, everything else held fixed: the same
      // index placed after took structured tool calls from 6/6 to 0/6 — the
      // model narrated the plan it had just been told to follow instead of
      // executing it. Eager bodies keep their position, so opting back into
      // eager reproduces the prompt it produced before.
      const parts: string[] = skillsMode === "index"
        ? [skillsBlob, systemPromptBase]
        : [systemPromptBase, skillsBlob];
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
          temperature,
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
        const builtin = builtins[call.function.name];
        const taskId = toolNameToTaskId[call.function.name];
        let result: unknown;
        if (!builtin && !taskId) {
          console.error(`ai-agent: unknown tool '${call.function.name}' requested`);
          result = { error: `unknown tool: ${call.function.name}` };
        } else {
          const label = builtin ? builtin.name : taskId;
          try {
            const parsed = call.function.arguments
              ? JSON.parse(call.function.arguments)
              : {};
            if (builtin) {
              result = await builtin.run(dicode, parsed, builtinCfg);
            } else {
              // dicode.run_task expects Record<string, string> — stringify non-string values
              const stringified: Record<string, string> = {};
              for (const [k, v] of Object.entries(parsed)) {
                stringified[k] = typeof v === "string" ? v : JSON.stringify(v);
              }
              result = await dicode.run_task(taskId, stringified);
            }
          } catch (e) {
            const msg = e instanceof Error ? e.message : String(e);
            console.error(`ai-agent: tool call failed tool=${label} call=${call.id}: ${msg}`);
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
  }

  const last = session.messages[session.messages.length - 1];
  return last?.role === "assistant" ? last.content : "";
}

export default async function main({ params, dicode }: DicodeSdk) {
  requireTaskId(dicode);

  const prompt = (await params.get("prompt")) ?? "";

  if (decideEntryMode(prompt) === "chat-start") {
    // Blank prompt on a fresh run → open the chat loop. The conversation rides
    // in the suspend `state` blob (resume_state), seeded empty; steps.turn runs
    // each turn. A blank message ends it.
    await chatStart(dicode, { messages: [] }, "Message the agent — leave blank to end.");
    return; // unreachable — suspend() never returns
  }

  return await oneShotTurn({ prompt, params, dicode });
}

// oneShotTurn runs a single stateless turn: it tags the run's group and returns
// the { session_id, reply } shape the browser UIs parse as application/json.
// Multi-turn conversation lives on the chat loop (carried in resume_state), not
// here — one-shot calls share no history.
async function oneShotTurn(
  { prompt, params, dicode }: {
    prompt: string;
    params: DicodeSdk["params"];
    dicode: Dicode;
  },
): Promise<unknown> {
  // Hybrid session id: use provided or auto-generate. A caller-supplied value
  // ends up in the `chat:<id>` run-group label (and is echoed back as the
  // continuation handle), so validate it the same way claude-cli validates its
  // carried session/chat ids — an off-shape value is treated as absent rather
  // than passed through.
  const sessionId = resolveSessionId(await params.get("session_id") ?? undefined, "ai-agent: session_id", { autoMint: true });

  // Tag the run so the WebUI collapses every turn of a single chat into
  // one expandable row in the run list (#112). Tool-call children are
  // already linked via parent_run_id by the engine (#116), so the run
  // detail view can render this turn as a timeline of sub-runs (#113).
  await dicode.set_group(`chat:${sessionId}`);

  const resolved = await resolveAgentRuntime(params, dicode);
  if (!resolved.ok) {
    // Same non-empty guarantee as the success path below, for the same
    // reason: a downstream pipeline stage reads reply and task_dir through
    // ${input.output.<field>}, which fails the dispatch on a null or absent
    // field. Returning the misconfiguration as prose is also what lets the
    // hint reach the caller at all — an input-reference error would replace
    // it with the resolver's own message.
    const response: NotConfiguredResponse = {
      session_id: sessionId,
      reply: `not configured — missing ${resolved.missing.join(", ")}. ${resolved.hint}`,
      error: "not_configured",
      missing: resolved.missing,
      hint: resolved.hint,
      task_dir: await params.get("task_dir") || "unknown",
    };
    return response;
  }

  const session: SessionState = {
    messages: [],
    created_at: Date.now(),
    updated_at: Date.now(),
  };
  const reply = await runAgentTurn(session, prompt, resolved.runtime, dicode);

  // Bare return → dicode serializes as application/json.
  // Do NOT call output.html() here; it would override Content-Type and the
  // browser UI would have to parse HTML instead of JSON.
  //
  // reply and task_dir are both guaranteed non-empty: a downstream pipeline
  // stage reaches them through ${input.output.<field>}, which fails the
  // dispatch on an empty string. A turn that ends on tool calls alone would
  // otherwise take the whole pipeline down with it.
  return {
    session_id: sessionId,
    reply: reply || "(the model returned no text this turn)",
    task_dir: await params.get("task_dir") || "unknown",
  };
}

export const steps = {
  // One chat turn: run one OpenAI turn for the submitted message. The envelope
  // owns the suspend/resume loop; runTurn here carries the cumulative
  // {messages, summary} forward in the suspend state (not KV).
  turn(ctx: DicodeSdk) {
    const { params, dicode } = ctx;
    return chatTurn(ctx, async ({ message, state }) => {
      const resolved = await resolveAgentRuntime(params, dicode);
      if (!resolved.ok) {
        return { ok: false, error: "not_configured", missing: resolved.missing, hint: resolved.hint };
      }
      const carried = (state ?? {}) as { messages?: StoredMessage[]; summary?: string };
      const session: SessionState = {
        messages: carried.messages ?? [],
        summary: carried.summary,
        created_at: Date.now(),
        updated_at: Date.now(),
      };
      const reply = await runAgentTurn(session, message, resolved.runtime, dicode);
      return { ok: true, reply, state: { messages: session.messages, summary: session.summary } };
    });
  },
};
