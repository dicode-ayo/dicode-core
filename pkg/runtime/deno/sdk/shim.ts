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

export interface OAuthAuthURL {
  url:        string;
  session_id: string;
  provider:   string;
  timestamp:  number;
  relay_uuid: string;
}

export interface OAuthStoreResult {
  provider: string;
  secrets:  string[];
}

export interface ProviderStatus {
  provider:    string;
  has_token:   boolean;
  expires_at?: string;
  scope?:      string;
  token_type?: string;
}

// OAuth broker bridge.
// - build_auth_url and store_token are functional only inside the auth-start
//   (oauth_init) and auth-relay (oauth_store) built-in tasks respectively.
// - list_status is callable from any task that opts in via permissions.dicode.oauth_status
//   (e.g. the auth-providers dashboard); it returns metadata only and never
//   surfaces plaintext tokens.
// Plaintext tokens never cross this boundary: store_token decrypts, parses, and
// writes to secrets entirely daemon-side, and only returns which secret names
// were written; list_status reads <P>_ACCESS_TOKEN only to set a presence flag
// and discards the value.
export interface DicodeOAuth {
  build_auth_url: (provider: string, scope?: string) => Promise<OAuthAuthURL>;
  store_token:    (envelope: unknown)                => Promise<OAuthStoreResult>;
  list_status:    (providers: string[])              => Promise<ProviderStatus[]>;
}

export interface Dicode {
  // task_id: the fully-namespaced id of the currently-running task (e.g.
  // "buildin/ai-agent"). Populated from the IPC handshake so task code can
  // self-identify without guessing from its directory name.
  task_id:        string;
  // run_id: the id of the current run (uuid). Same source as task_id.
  run_id:         string;
  run_task:       (taskID: string, params?: Record<string, string>)  => Promise<unknown>;
  list_tasks:     ()                                                   => Promise<unknown>;
  get_runs:       (taskID: string, opts?: { limit?: number })         => Promise<unknown>;
  secrets_set:    (key: string, value: string)                        => Promise<void>;
  secrets_delete: (key: string)                                        => Promise<void>;
  oauth:          DicodeOAuth;
  runs: {
    list_expired: (opts?: { before_ts?: number }) => Promise<unknown>;
    delete_input: (runID: string)                 => Promise<unknown>;
    pin_input:    (runID: string)                 => Promise<unknown>;
    unpin_input:  (runID: string)                 => Promise<unknown>;
    get_input:    (runID: string)                 => Promise<unknown>;
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
  run_task:       (taskID, params)  => __call__({ method: "dicode.run_task",       taskID, params: params ?? {} }),
  list_tasks:     ()                => __call__({ method: "dicode.list_tasks" }),
  get_runs:       (taskID, opts)    => __call__({ method: "dicode.get_runs",        taskID, limit: opts?.limit ?? 10 }),
  secrets_set:    (key, value)      => __call__({ method: "dicode.secrets_set",     key, stringValue: value }) as Promise<void>,
  secrets_delete: (key)             => __call__({ method: "dicode.secrets_delete",  key }) as Promise<void>,
  oauth: {
    build_auth_url: (provider, scope) =>
      __call__({ method: "dicode.oauth.build_auth_url", provider, scope: scope ?? "" }) as Promise<OAuthAuthURL>,
    store_token: (envelope) =>
      __call__({ method: "dicode.oauth.store_token", envelope }) as Promise<OAuthStoreResult>,
    list_status: (providers) =>
      __call__({ method: "dicode.oauth.list_status", providers }) as Promise<ProviderStatus[]>,
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
  },
};

// ── exports ───────────────────────────────────────────────────────────────────
// Named exports consumed by the per-run wrapper that calls the user's main().

// __flush__ drains the write queue before the connection is closed.
// The runner awaits this on exit so fire-and-forget log writes are not lost.
async function __flush__(): Promise<void> { await __wq__; }

export { params, kv, input, output, mcp, dicode, __setReturn__, __conn__, __flush__ };
