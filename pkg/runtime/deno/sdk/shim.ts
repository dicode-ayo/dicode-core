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

export interface Output {
  html:  (content: string, opts?: OutputOptions) => Promise<void>;
  text:  (content: string)                        => Promise<void>;
  image: (mime: string | null, content: string)   => Promise<void>;
  file:  (name: string, content: string, mime?: string) => Promise<void>;
}

export interface MCP {
  list_tools: (name: string)                             => Promise<unknown>;
  call:       (name: string, tool: string, args?: Record<string, unknown>) => Promise<unknown>;
}

export interface Dicode {
  run_task:       (taskID: string, params?: Record<string, string>)  => Promise<unknown>;
  list_tasks:     ()                                                   => Promise<unknown>;
  get_runs:       (taskID: string, opts?: { limit?: number })         => Promise<unknown>;
  get_config:     (section: string)                                    => Promise<unknown>;
  secrets_set:    (key: string, value: string)                        => Promise<void>;
  secrets_delete: (key: string)                                        => Promise<void>;
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

const output: Output = {
  html:  (content, opts) => __fire__({ method: "output", contentType: "text/html",                    content, data: opts?.data ?? null }),
  text:  (content)       => __fire__({ method: "output", contentType: "text/plain",                   content }),
  image: (mime, content) => __fire__({ method: "output", contentType: mime ?? "image/png",            content }),
  file:  (name, content, mime) => __fire__({ method: "output", contentType: mime ?? "application/octet-stream", content, data: { filename: name } }),
};

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
  run_task:       (taskID, params)  => __call__({ method: "dicode.run_task",       taskID, params: params ?? {} }),
  list_tasks:     ()                => __call__({ method: "dicode.list_tasks" }),
  get_runs:       (taskID, opts)    => __call__({ method: "dicode.get_runs",        taskID, limit: opts?.limit ?? 10 }),
  get_config:     (section)         => __call__({ method: "dicode.get_config",      section }),
  secrets_set:    (key, value)      => __call__({ method: "dicode.secrets_set",     key, stringValue: value }) as Promise<void>,
  secrets_delete: (key)             => __call__({ method: "dicode.secrets_delete",  key }) as Promise<void>,
};

// ── exports ───────────────────────────────────────────────────────────────────
// Named exports consumed by the per-run wrapper that calls the user's main().

// __flush__ drains the write queue before the connection is closed.
// The runner awaits this on exit so fire-and-forget log writes are not lost.
async function __flush__(): Promise<void> { await __wq__; }

export { params, kv, input, output, mcp, dicode, __setReturn__, __conn__, __flush__ };
