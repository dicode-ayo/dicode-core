// dicode SDK shim — imported by the per-run wrapper before calling main().
// Provides: log, params, env, kv, input, output, __setReturn__, mcp, dicode.
//
// Protocol: length-prefixed JSON over a single persistent Unix socket.
//   Frame:  [4-byte little-endian length][JSON bytes]
//
// Handshake (first exchange after connect):
//   Client → { token: "<DICODE_TOKEN>" }
//   Server → { proto: 1, caps: ["log", "params.read", ...] }
//
// After handshake, same request/response pattern as before:
//   Fire-and-forget (no id):  log, kv.set, kv.delete, output
//   Request/response (id):    params, input, kv.get, kv.list, return, dicode.*, mcp.*

const __enc__ = new TextEncoder();
const __dec__ = new TextDecoder();
const __conn__ = await Deno.connect({
  transport: "unix",
  path: Deno.env.get("DICODE_SOCKET"),
});

// ── framing helpers ───────────────────────────────────────────────────────────

// Read exactly n bytes from the connection.
async function __readExact__(n) {
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

// Read one length-prefixed message and return the parsed object.
async function __readMsg__() {
  const hdr = await __readExact__(4);
  const size = hdr[0] | (hdr[1] << 8) | (hdr[2] << 16) | (hdr[3] << 24);
  const body = await __readExact__(size);
  return JSON.parse(__dec__.decode(body));
}

// Write one length-prefixed message.
let __wq__ = Promise.resolve();
function __writeMsg__(obj) {
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
  __wq__ = __wq__.then(() => __conn__.write(frame));
}

// ── handshake ─────────────────────────────────────────────────────────────────

__writeMsg__({ token: Deno.env.get("DICODE_TOKEN") });
const __hsResp__ = await __readMsg__();
if (__hsResp__.error) {
  throw new Error(`ipc handshake failed: ${__hsResp__.error}`);
}
// __hsResp__.caps contains the granted capability list (informational).

// ── read loop ─────────────────────────────────────────────────────────────────

const __pending__ = new Map(); // id → resolve
let __nid__ = 0;

(async () => {
  while (true) {
    let msg;
    try { msg = await __readMsg__(); } catch { break; }
    const resolve = __pending__.get(msg.id);
    if (resolve) { __pending__.delete(msg.id); resolve(msg); }
  }
})();

// ── call helpers ──────────────────────────────────────────────────────────────

function __call__(req) {
  const id = String(++__nid__);
  __writeMsg__({ ...req, id });
  return new Promise((resolve, reject) =>
    __pending__.set(id, (msg) =>
      msg.error ? reject(new Error(msg.error)) : resolve(msg.result),
    ),
  );
}

function __fire__(req) {
  __writeMsg__(req);
  return Promise.resolve();
}

// ── log ───────────────────────────────────────────────────────────────────────

function __fmt__(msg, args) {
  if (!args.length) return String(msg);
  return [String(msg), ...args.map((a) =>
    typeof a === "object" ? JSON.stringify(a) : String(a),
  )].join(" ");
}

const log = {
  info:  (msg, ...args) => __fire__({ method: "log", level: "info",  message: __fmt__(msg, args) }),
  warn:  (msg, ...args) => __fire__({ method: "log", level: "warn",  message: __fmt__(msg, args) }),
  error: (msg, ...args) => __fire__({ method: "log", level: "error", message: __fmt__(msg, args) }),
  debug: (msg, ...args) => __fire__({ method: "log", level: "debug", message: __fmt__(msg, args) }),
};

// ── params ────────────────────────────────────────────────────────────────────

let __params_fetch__ = null;
function __getParams__() {
  if (!__params_fetch__) __params_fetch__ = __call__({ method: "params" });
  return __params_fetch__;
}
const params = {
  get: async (key) => ((await __getParams__()) ?? {})[key] ?? null,
  all: () => __getParams__().then((p) => p ?? {}),
};

// ── env ───────────────────────────────────────────────────────────────────────

const env = {
  get: (key) => Deno.env.get(key) ?? null,
};

// ── kv ────────────────────────────────────────────────────────────────────────

const kv = {
  get:    (key)         => __call__({ method: "kv.get", key }),
  set:    (key, value)  => __fire__({ method: "kv.set", key, value }),
  delete: (key)         => __fire__({ method: "kv.delete", key }),
  list:   (prefix = "") => __call__({ method: "kv.list", prefix }),
};

// ── input ─────────────────────────────────────────────────────────────────────

const input = await __call__({ method: "input" });

// ── output ────────────────────────────────────────────────────────────────────

const output = {
  html:  (content, opts) => __fire__({ method: "output", contentType: "text/html",                    content, data: opts?.data ?? null }),
  text:  (content)       => __fire__({ method: "output", contentType: "text/plain",                   content }),
  image: (mime, content) => __fire__({ method: "output", contentType: mime ?? "image/png",            content }),
  file:  (name, content, mime) => __fire__({ method: "output", contentType: mime ?? "application/octet-stream", content, data: { filename: name } }),
};

// ── return ────────────────────────────────────────────────────────────────────

async function __setReturn__(val) {
  await __call__({ method: "return", value: val ?? null });
}

// ── mcp ───────────────────────────────────────────────────────────────────────

const mcp = {
  list_tools: (name)             => __call__({ method: "mcp.list_tools", mcpName: name }),
  call:       (name, tool, args) => __call__({ method: "mcp.call",       mcpName: name, tool, args: args ?? {} }),
};

// ── dicode ────────────────────────────────────────────────────────────────────

const dicode = {
  run_task:   (taskID, params)  => __call__({ method: "dicode.run_task",   taskID, params: params ?? {} }),
  list_tasks: ()                => __call__({ method: "dicode.list_tasks" }),
  get_runs:   (taskID, opts)    => __call__({ method: "dicode.get_runs",   taskID, limit: opts?.limit ?? 10 }),
  get_config: (section)         => __call__({ method: "dicode.get_config", section }),
};

// ── exports ───────────────────────────────────────────────────────────────────
// Named exports consumed by the per-run wrapper that calls the user's main().

// __flush__ drains the write queue before the connection is closed.
// The runner awaits this on exit so fire-and-forget log writes are not lost.
async function __flush__() { await __wq__; }

export { log, params, env, kv, input, output, mcp, dicode, __setReturn__, __conn__, __flush__ };
