// render.ts — turn a notification's fields into Telegram message text.
//
// Pure: no fetch, no SDK, no env.

/** One notification, normalized. A field is absent when the caller did not send
 *  it — absence and an explicitly empty value are not distinguished, since no
 *  source has a meaning for the latter. */
export interface NotifyFields {
  title?: string;
  body?: string;
  priority?: string;
  event?: string;
  status?: string;
  task_id?: string;
  run_id?: string;
  hash?: string;
  approve_url?: string;
  resume_url?: string;
  output?: string;
}

export interface RenderedMessage {
  text: string;
  /** Telegram's disable_notification — delivered without a sound/vibration. */
  silent: boolean;
}

/** Telegram rejects a sendMessage whose text exceeds 4096 UTF-16 code units. */
export const MAX_TEXT = 4096;

/** Caps applied before escaping, so an escape-heavy value can still exceed them
 *  by up to 5x downstream; clamp() is what actually bounds the wire payload. */
const TITLE_MAX = 200;
const BODY_MAX = 1500;
const OUTPUT_MAX = 800;

const SILENT_PRIORITIES = new Set(["min", "low"]);

/**
 * Escape for Telegram's HTML parse mode, which defines exactly three reserved
 * characters. MarkdownV2 would need ~18, and a task ID like
 * `buildin/ai-agent-claude-cli` carries one of them — a missed escape fails the
 * send with HTTP 400 precisely when the notification matters.
 */
export function escapeHtml(s: string): string {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

function cut(s: string, max: number): string {
  return s.length > max ? s.slice(0, max - 1) + "…" : s;
}

function stringify(v: unknown): string {
  if (v === undefined || v === null) return "";
  if (typeof v === "string") return v;
  try {
    return JSON.stringify(v) ?? "";
  } catch {
    return String(v);
  }
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === "object" && !Array.isArray(v);
}

/**
 * Merge a run's params with its chain `input` into one field set. Params win;
 * `input` supplies the failure-chain path, whose engine-stamped keys are
 * camelCase (taskID/runID) where the hook params are snake_case.
 */
export function collectFields(params: Record<string, string>, input: unknown): NotifyFields {
  const rec = isRecord(input) ? input : {};

  const pick = (...names: string[]): string | undefined => {
    for (const n of names) {
      const v = (params[n] ?? "").trim();
      if (v) return v;
    }
    for (const n of names) {
      const v = stringify(rec[n]).trim();
      if (v) return v;
    }
    return undefined;
  };

  return {
    title: pick("title"),
    body: pick("body", "message"),
    priority: pick("priority"),
    event: pick("event"),
    status: pick("status"),
    task_id: pick("task_id", "taskID"),
    run_id: pick("run_id", "runID"),
    hash: pick("hash"),
    approve_url: pick("approve_url"),
    resume_url: pick("resume_url"),
    output: pick("output"),
  };
}

/** Which of the notification sources this field set came from. Derived once so
 *  the title and the body cannot disagree about it. */
type Kind = "approval" | "failure" | "suspended" | "status" | "unknown";

function classify(f: NotifyFields): Kind {
  if (f.approve_url !== undefined || f.hash !== undefined) return "approval";
  if (f.status === "failure") return "failure";
  if (f.event === "suspended") return "suspended";
  if (f.status !== undefined) return "status";
  return "unknown";
}

function derive(f: NotifyFields, kind: Kind): { title: string; body: string } {
  const who = f.task_id ?? "a task";
  switch (kind) {
    case "approval":
      return {
        title: "dicode: task pending approval",
        body: `${who} is held pending approval and will not run until it is approved.`,
      };
    case "failure":
      return { title: "dicode: task failed", body: `${who} finished with status ${f.status}.` };
    case "suspended":
      return {
        title: "dicode: an agent needs your reply",
        body: `${who} is paused for your input.`,
      };
    case "status":
      return { title: `dicode: task ${f.status}`, body: `${who} finished with status ${f.status}.` };
    default:
      return { title: "dicode notification", body: "" };
  }
}

/**
 * Join lines under Telegram's length cap. Every line is self-contained HTML, so
 * dropping whole trailing lines can never split a tag or an entity the way a
 * character-level truncation would.
 */
function clamp(lines: string[], max: number): string {
  const kept: string[] = [];
  let len = 0;
  for (const line of lines) {
    const cost = kept.length === 0 ? line.length : line.length + 1;
    // The "…" marker costs a separator plus itself, so its budget is reserved
    // rather than spent past the cap this function exists to hold.
    if (len + cost > max - 2) {
      kept.push("…");
      break;
    }
    kept.push(line);
    len += cost;
  }
  return kept.join("\n");
}

/** Render one notification. Never throws, whatever subset of fields arrived. */
export function renderMessage(f: NotifyFields): RenderedMessage {
  const derived = derive(f, classify(f));

  const lines: string[] = [`<b>${escapeHtml(cut(f.title ?? derived.title, TITLE_MAX))}</b>`];

  const body = f.body ?? derived.body;
  if (body) lines.push("", escapeHtml(cut(body, BODY_MAX)));

  const detail: string[] = [];
  if (f.task_id !== undefined) detail.push(`Task: <code>${escapeHtml(f.task_id)}</code>`);
  if (f.run_id !== undefined) detail.push(`Run: <code>${escapeHtml(f.run_id)}</code>`);
  if (f.status !== undefined) detail.push(`Status: ${escapeHtml(f.status)}`);
  // The content hash is what re-pends an approval; without it, two holds on the
  // same task are indistinguishable. A prefix is enough to tell them apart.
  if (f.hash !== undefined) detail.push(`Content: <code>${escapeHtml(f.hash.slice(0, 12))}</code>`);
  // The suspend hook inlines the resume link in its body; a second copy is noise.
  if (f.approve_url !== undefined && !body.includes(f.approve_url)) {
    detail.push(`Approve: ${escapeHtml(f.approve_url)}`);
  }
  if (f.resume_url !== undefined && !body.includes(f.resume_url)) {
    detail.push(`Resume: ${escapeHtml(f.resume_url)}`);
  }
  if (detail.length > 0) lines.push("", ...detail);

  if (f.output !== undefined) lines.push("", `<pre>${escapeHtml(cut(f.output, OUTPUT_MAX))}</pre>`);

  return { text: clamp(lines, MAX_TEXT), silent: SILENT_PRIORITIES.has(f.priority ?? "") };
}
