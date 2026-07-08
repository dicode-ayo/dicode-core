// dicode SDK shim — imported by the per-run wrapper before calling main().
// Provides: params, env, kv, input, output, __setReturn__, mcp, dicode.
//
// Logging: use console.log/warn/error/debug — the runtime captures stdout as
// "info" and stderr as "error" in the run log. No separate log global needed.
//
// Protocol: length-prefixed JSON over a single persistent Unix socket.
//   Frame:  [4-byte little-endian length][JSON bytes]
//
// Handshake (first exchange after connect):
//   Client → { token: "<DICODE_TOKEN>" }
//   Server → { proto: 1, caps: ["params.read", ...] }
//
// After handshake, same request/response pattern as before:
//   Fire-and-forget (no id):  kv.set, kv.delete, output
//   Request/response (id):    params, input, kv.get, kv.list, return, dicode.*, mcp.*

// ── types ─────────────────────────────────────────────────────────────────────

interface IpcRequest {
  method: string;
  id?: string;
  [key: string]: unknown;
}

interface IpcResponse {
  id?: string;
  result?: unknown;
  error?: string;
}

interface HandshakeResponse {
  proto: number;
  caps: string[];
  task_id?: string;
  run_id?: string;
  error?: string;
}

export interface Params {
  get: (key: string) => Promise<string | null>;
  all: () => Promise<Record<string, string>>;
}

export interface KV {
  get:    (key: string)                => Promise<unknown>;
  set:    (key: string, value: unknown) => Promise<void>;
  delete: (key: string)                => Promise<void>;
  list:   (prefix?: string)            => Promise<Record<string, unknown>>;
}

export interface OutputOptions {
  data?: Record<string, unknown> | null;
}

// Secret output flag — when true, the daemon treats `value` as a flat
// Record<string, string> and routes it to the resolver awaiting this
// task. Values are also fed to the run-log redactor and the run log
// records keys with [redacted] placeholders only. Issue #119.
export interface SecretOutputOptions {
  secret: true;
}

export interface Output {
  html:  (content: string, opts?: OutputOptions) => Promise<void>;
  text:  (content: string)                        => Promise<void>;
  image: (mime: string | null, content: string)   => Promise<void>;
  file:  (name: string, content: string, mime?: string) => Promise<void>;
  // Provider-task entry point (issue #119). Throws synchronously if
  // `value` is not a flat Record<string,string>.
  (value: Record<string, string>, opts: SecretOutputOptions): Promise<void>;
}

export interface MCP {
  list_tools: (name: string)                             => Promise<unknown>;
  call:       (name: string, tool: string, args?: Record<string, unknown>) => Promise<unknown>;
}

export interface DicodeCrypto {
  encrypt: (context: string, plaintext: Uint8Array) => Promise<Uint8Array>;
  decrypt: (context: string, ciphertext: Uint8Array) => Promise<Uint8Array>;
}

export interface DicodeSecrets {
  // has returns true if a secret with the given key exists in the secrets
  // store. Never returns the value. Requires permissions.dicode.secrets_has.
  has: (key: string) => Promise<boolean>;
}

// AuditEvent is one row of the security audit log. Params are already
// sanitized at write time — secret-shaped values were redacted before
// storage, so no value here is a raw secret.
export interface AuditEvent {
  id:          string;
  ts:          string;
  event_type:  string;
  actor_kind:  string;
  actor_id:    string;
  target_kind: string;
  target_id:   string;
  params?:     string;
  run_id?:     string;
  allowed:     boolean;
  reason?:     string;
}

export interface AuditQueryResult {
  events:      AuditEvent[];
  next_cursor: string;
}

export interface DicodeAudit {
  // query reads the security audit trail. Pass `after` (the next_cursor
  // from a prior result) with order:"asc" to walk the log forward
  // exactly-once. Requires permissions.dicode.audit_query.
  query: (opts?: {
    after?:      string;
    limit?:      number;
    order?:      "asc" | "desc";
    taskID?:     string;
    actor?:      string;
    eventType?:  string;
  }) => Promise<AuditQueryResult>;
}

// TaskSummary is what dicode.list_tasks() returns per task. The IPC server
// trims the spec to fields already exposed via /api/tasks; filesystem paths
// and permission details are deliberately not surfaced.
export interface TaskSummary {
  id:           string;
  name:         string;
  description?: string;
  params?:      unknown;
  // template: optional namespaced marker (e.g. "dicode.io/oauth-app").
  // Set in the base task.yaml; propagates to every entry that inherits
  // via ref.path. Use this to discover template instances at runtime.
  template?:    string;
  // webhook: webhook path for tasks with a webhook trigger.
  webhook?:     string;
  enabled:      boolean;
}

// A JSON Schema (draft 2020-12) object describing the input a suspended task
// collects before resume (#512). The daemon validates the submission against it
// server-side. Loosely typed — any valid JSON Schema is accepted.
export type JSONSchema = {
  type?: string;
  title?: string;
  description?: string;
  properties?: Record<string, JSONSchema>;
  required?: string[];
  enum?: unknown[];
  default?: unknown;
  format?: string;
  [keyword: string]: unknown;
};

// Argument to dicode.suspend() (#512). `state` is an opaque JSON blob echoed
// back as ctx.resume_state on resume; `schema` is the JSON Schema the submitted
// input is validated against; `deadline` is an optional Unix-ms TTL (default
// applied by the engine).
export interface SuspendRequest {
  state?: unknown;
  schema: JSONSchema;
  deadline?: number;
}

export interface Dicode {
  // task_id: the fully-namespaced id of the currently-running task (e.g.
  // "buildin/ai-agent"). Populated from the IPC handshake so task code can
  // self-identify without guessing from its directory name.
  task_id:        string;
  // run_id: the id of the current run (uuid). Same source as task_id.
  run_id:         string;
  run_task:       (taskID: string, params?: Record<string, string>, opts?: { mcpContext?: boolean })  => Promise<unknown>;
  list_tasks:     (opts?: { mcpContext?: boolean })                     => Promise<TaskSummary[]>;
  get_runs:       (taskID: string, opts?: { limit?: number })         => Promise<unknown>;
  // set_group labels the current run with a free-text string used by the
  // WebUI to collapse same-group siblings (#116). Last write wins; only
  // affects the current run.
  set_group:      (label: string)      => Promise<void>;
  // suspend pauses the run: it hands the runtime the state blob + schema, then
  // never resolves — it throws internally so the process exits cleanly and
  // the run ends as `suspended`. On resume the task is re-run with
  // ctx.resume_state / ctx.resume_input populated (#512).
  suspend:        (req: SuspendRequest) => Promise<never>;
  secrets_set:    (key: string, value: string) => Promise<void>;
  secrets_delete: (key: string)                => Promise<void>;
  secrets:        DicodeSecrets;
  crypto:         DicodeCrypto;
  audit:          DicodeAudit;
  runs: {
    list_expired: (opts?: { before_ts?: number }) => Promise<unknown>;
    delete_input: (runID: string)                 => Promise<unknown>;
    pin_input:    (runID: string)                 => Promise<unknown>;
    unpin_input:  (runID: string)                 => Promise<unknown>;
    get_input:    (runID: string)                 => Promise<unknown>;
    replay:       (runID: string, taskName?: string) => Promise<unknown>;
  };
  tasks: {
    test: (taskID: string) => Promise<unknown>;
  };
  sources: {
    set_dev_mode: (name: string, opts: {
      enabled: boolean;
      local_path?: string;
      branch?: string;
      base?: string;
      run_id?: string;
    }) => Promise<unknown>;
  };
  git: {
    commit_push: (sourceID: string, opts: {
      message: string;
      branch: string;
      branch_prefix?: string;
      allow_main?: boolean;
      files?: string[];
      author_name: string;
      author_email: string;
      auth_token_env?: string;
    }) => Promise<{ commit: string }>;
  };
}

// ── connection ────────────────────────────────────────────────────────────────

const __enc__ = new TextEncoder();
const __dec__ = new TextDecoder();
const __conn__ = await Deno.connect({
  transport: "unix",
  path: Deno.env.get("DICODE_SOCKET")!,
});

// ── framing helpers ───────────────────────────────────────────────────────────

async function __readExact__(n: number): Promise<Uint8Array> {
  const buf = new Uint8Array(n);
  let offset = 0;
  while (offset < n) {
    const chunk = new Uint8Array(n - offset);
    const read = await __conn__.read(chunk);
    if (read === null) throw new Error("ipc: connection closed");
    buf.set(chunk.slice(0, read), offset);
    offset += read;
  }
  return buf;
}

async function __readMsg__(): Promise<IpcResponse | HandshakeResponse> {
  const hdr = await __readExact__(4);
  const size = hdr[0] | (hdr[1] << 8) | (hdr[2] << 16) | (hdr[3] << 24);
  const body = await __readExact__(size);
  return JSON.parse(__dec__.decode(body));
}

let __wq__: Promise<void> = Promise.resolve();
function __writeMsg__(obj: IpcRequest | { token: string }): void {
  const body = __enc__.encode(JSON.stringify(obj));
  const hdr = new Uint8Array(4);
  const len = body.length;
  hdr[0] = len & 0xff;
  hdr[1] = (len >> 8) & 0xff;
  hdr[2] = (len >> 16) & 0xff;
  hdr[3] = (len >> 24) & 0xff;
  const frame = new Uint8Array(4 + len);
  frame.set(hdr);
  frame.set(body, 4);
  __wq__ = __wq__.then(() => { __conn__.write(frame); });
}

// ── handshake ─────────────────────────────────────────────────────────────────

__writeMsg__({ token: Deno.env.get("DICODE_TOKEN")! });
const __hsResp__ = await __readMsg__() as HandshakeResponse;
if (__hsResp__.error) {
  throw new Error(`ipc handshake failed: ${__hsResp__.error}`);
}
// __hsResp__.caps contains the granted capability list (informational).

// ── read loop ─────────────────────────────────────────────────────────────────

const __pending__ = new Map<string, (msg: IpcResponse) => void>();
let __nid__ = 0;

(async () => {
  while (true) {
    let msg: IpcResponse;
    try { msg = await __readMsg__() as IpcResponse; } catch { break; }
    if (msg.id) {
      const resolve = __pending__.get(msg.id);
      if (resolve) { __pending__.delete(msg.id); resolve(msg); }
    }
  }
})();

// ── call helpers ──────────────────────────────────────────────────────────────

function __call__(req: IpcRequest): Promise<unknown> {
  const id = String(++__nid__);
  __writeMsg__({ ...req, id });
  return new Promise((resolve, reject) =>
    __pending__.set(id, (msg) =>
      msg.error ? reject(new Error(msg.error)) : resolve(msg.result),
    ),
  );
}

function __fire__(req: IpcRequest): Promise<void> {
  __writeMsg__(req);
  return Promise.resolve();
}

// console.log/info/warn/error/debug go to stdout/stderr as normal.
// The runtime captures stdout as "info" and stderr as "error" in the run log.

// ── base64 helpers (used by dicode.crypto) ───────────────────────────────────

function __b64enc__(bytes: Uint8Array): string {
  let bin = "";
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin);
}

function __b64dec__(s: string): Uint8Array {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

// ── params ────────────────────────────────────────────────────────────────────

let __params_fetch__: Promise<Record<string, string>> | null = null;
function __getParams__(): Promise<Record<string, string>> {
  if (!__params_fetch__) __params_fetch__ = __call__({ method: "params" }) as Promise<Record<string, string>>;
  return __params_fetch__;
}
const params: Params = {
  get: async (key) => ((await __getParams__()) ?? {})[key] ?? null,
  all: () => __getParams__().then((p) => p ?? {}),
};

// Env vars: use Deno.env.get("VAR") directly. The --allow-env flag is already
// scoped to vars declared in task.yaml, so the security boundary is unchanged.

// ── kv ────────────────────────────────────────────────────────────────────────

const kv: KV = {
  get:    (key)         => __call__({ method: "kv.get", key }),
  set:    (key, value)  => __fire__({ method: "kv.set", key, value }),
  delete: (key)         => __fire__({ method: "kv.delete", key }),
  list:   (prefix = "") => __call__({ method: "kv.list", prefix }) as Promise<Record<string, unknown>>,
};

// ── input ─────────────────────────────────────────────────────────────────────

const input = await __call__({ method: "input" });

// ── resume (#95) ────────────────────────────────────────────────────────────────
// When this run is a resume of a suspended one, the runtime injects the prior
// state blob and the user's form submission. Both are null on a first run.

const __resume__ = await __call__({ method: "resume" }) as
  { state?: unknown; input?: unknown } | null;
const resume_state: unknown = __resume__?.state ?? undefined;
const resume_input: unknown = __resume__?.input ?? undefined;

// SuspendSignal is thrown by dicode.suspend() after the payload is delivered
// over IPC. The per-run wrapper catches it (via __isSuspend__) and exits the
// process cleanly (exit 0) — a suspend is not a failure.
class SuspendSignal extends Error {
  constructor() {
    super("dicode.suspend");
    this.name = "SuspendSignal";
  }
}
function __isSuspend__(e: unknown): boolean {
  return e instanceof SuspendSignal;
}

// Set the instant before the SuspendSignal is thrown (payload already recorded
// server-side). If main() returns normally with this set, a user try/catch
// swallowed the signal and kept executing — the wrapper turns that into a loud
// failure rather than a contradictory "suspended run that also returned".
let __suspendRequested__ = false;
function __wasSuspendRequested__(): boolean { return __suspendRequested__; }

// ── output ────────────────────────────────────────────────────────────────────

function __outputCallable__(value: Record<string, string>, _opts: SecretOutputOptions): Promise<void> {
  // Validate flat string map up front so the failure surface is the
  // SDK call site, not "the daemon dropped it silently".
  for (const [k, v] of Object.entries(value)) {
    if (typeof v !== "string") {
      return Promise.reject(new Error(
        `dicode.output(map, { secret: true }): value for key ${JSON.stringify(k)} is not a string`));
    }
  }
  return __fire__({ method: "output", secret: true, secretMap: value });
}

const __outputObj__ = {
  html:  (content: string, opts?: OutputOptions) => __fire__({ method: "output", contentType: "text/html",                     content, data: opts?.data ?? null }),
  text:  (content: string)                       => __fire__({ method: "output", contentType: "text/plain",                    content }),
  image: (mime: string | null, content: string)  => __fire__({ method: "output", contentType: mime ?? "image/png",             content }),
  file:  (name: string, content: string, mime?: string) => __fire__({ method: "output", contentType: mime ?? "application/octet-stream", content, data: { filename: name } }),
};

// Synthesize a callable+method object. JavaScript functions ARE objects,
// so attach the four methods as properties on the function.
const output: Output = Object.assign(__outputCallable__, __outputObj__) as unknown as Output;

// ── return ────────────────────────────────────────────────────────────────────

async function __setReturn__(val?: unknown): Promise<void> {
  await __call__({ method: "return", value: val ?? null });
}

// ── mcp ───────────────────────────────────────────────────────────────────────

const mcp: MCP = {
  list_tools: (name)             => __call__({ method: "mcp.list_tools", mcpName: name }),
  call:       (name, tool, args) => __call__({ method: "mcp.call",       mcpName: name, tool, args: args ?? {} }),
};

// ── dicode ────────────────────────────────────────────────────────────────────

const dicode: Dicode = {
  task_id:        __hsResp__.task_id ?? "",
  run_id:         __hsResp__.run_id ?? "",
  run_task:       (taskID, params, opts)  => __call__({ method: "dicode.run_task", taskID, params: params ?? {}, mcpContext: opts?.mcpContext ?? false }),
  list_tasks:     (opts)                => __call__({ method: "dicode.list_tasks", mcpContext: opts?.mcpContext ?? false }) as Promise<TaskSummary[]>,
  get_runs:       (taskID, opts)    => __call__({ method: "dicode.get_runs",        taskID, limit: opts?.limit ?? 10 }),
  set_group:      (label)           => __call__({ method: "dicode.set_group",       group: String(label ?? "") }) as Promise<void>,
  suspend:        async (req) => {
    // Await the ack so the payload is recorded before we throw — the read
    // loop resolves this once the daemon captured state/schema/deadline. The
    // call then never resolves normally: it always throws.
    await __call__({
      method: "dicode.suspend",
      state: req.state ?? null,
      schema: req.schema,
      deadline: req.deadline ?? 0,
    });
    __suspendRequested__ = true;
    throw new SuspendSignal();
  },
  secrets_set:    (key, value)      => __call__({ method: "dicode.secrets_set",     key, stringValue: value }) as Promise<void>,
  secrets_delete: (key)             => __call__({ method: "dicode.secrets_delete",  key }) as Promise<void>,
  secrets: {
    has: (key) => __call__({ method: "dicode.secrets.has", key }) as Promise<boolean>,
  },
  audit: {
    query: (opts) =>
      __call__({
        method: "dicode.audit.query",
        after: opts?.after ?? "",
        limit: opts?.limit ?? 0,
        order: opts?.order ?? "desc",
        taskID: opts?.taskID ?? "",
        actor: opts?.actor ?? "",
        event_type: opts?.eventType ?? "",
      }) as Promise<AuditQueryResult>,
  },
  runs: {
    list_expired: (opts) =>
      __call__({ method: "dicode.runs.list_expired", before_ts: opts?.before_ts ?? 0 }),
    delete_input: (runID) =>
      __call__({ method: "dicode.runs.delete_input", runID }),
    pin_input: (runID) =>
      __call__({ method: "dicode.runs.pin_input", runID }),
    unpin_input: (runID) =>
      __call__({ method: "dicode.runs.unpin_input", runID }),
    get_input: (runID) =>
      __call__({ method: "dicode.runs.get_input", runID }),
    replay: (runID, taskName) =>
      __call__({ method: "dicode.runs.replay", runID, taskID: taskName ?? "" }),
  },
  tasks: {
    test: (taskID) =>
      __call__({ method: "dicode.tasks.test", taskID }),
  },
  sources: {
    set_dev_mode: (name, opts) =>
      __call__({
        method: "dicode.sources.set_dev_mode",
        name,
        enabled: opts.enabled,
        local_path: opts.local_path ?? "",
        branch: opts.branch ?? "",
        base: opts.base ?? "",
        run_id: opts.run_id ?? "",
      }),
  },
  git: {
    commit_push: (sourceID, opts) =>
      __call__({
        method: "dicode.git.commit_push",
        source_id: sourceID,
        commit_message: opts.message,
        branch: opts.branch,
        branch_prefix: opts.branch_prefix ?? "",
        allow_main: opts.allow_main ?? false,
        files: opts.files ?? [],
        author_name: opts.author_name,
        author_email: opts.author_email,
        auth_token_env: opts.auth_token_env ?? "",
      }) as Promise<{ commit: string }>,
  },
  crypto: {
    encrypt: async (context, plaintext) => {
      const res = await __call__({
        method: "dicode.crypto.encrypt",
        context,
        plaintext_b64: __b64enc__(plaintext),
      }) as { ciphertext_b64: string };
      return __b64dec__(res.ciphertext_b64);
    },
    decrypt: async (context, ciphertext) => {
      const res = await __call__({
        method: "dicode.crypto.decrypt",
        context,
        ciphertext_b64: __b64enc__(ciphertext),
      }) as { plaintext_b64: string };
      return __b64dec__(res.plaintext_b64);
    },
  },
};

// ── exports ───────────────────────────────────────────────────────────────────
// Named exports consumed by the per-run wrapper that calls the user's main().

// __flush__ drains the write queue before the connection is closed.
// The runner awaits this on exit so fire-and-forget log writes are not lost.
async function __flush__(): Promise<void> { await __wq__; }

export { params, kv, input, resume_state, resume_input, output, mcp, dicode, __setReturn__, __conn__, __flush__, __isSuspend__, __wasSuspendRequested__ };
