// pool_shim.js — bootstrap shim for warm Deno pool processes.
//
// This script is run by each pre-warmed Deno process. It:
//   1. Reads the pool socket path from DICODE_POOL_SOCKET env var.
//   2. Connects to the pool Unix socket and waits for a dispatch message.
//   3. On receipt of { socketPath, token, script } evaluates the script
//      with all SDK globals injected (same as the normal shim).
//
// The Go pool sends one dispatch per connection, then closes it.
// After execution the Deno process exits naturally.

const __poolSocket__ = Deno.env.get("DICODE_POOL_SOCKET");
if (!__poolSocket__) {
  throw new Error("pool shim: DICODE_POOL_SOCKET not set");
}

// ── framing (same protocol as ipc/server.go) ────────────────────────────────

const __enc__ = new TextEncoder();
const __dec__ = new TextDecoder();

async function __readExact__(conn, n) {
  const buf = new Uint8Array(n);
  let offset = 0;
  while (offset < n) {
    const chunk = new Uint8Array(n - offset);
    const read = await conn.read(chunk);
    if (read === null) throw new Error("pool shim: connection closed");
    buf.set(chunk.slice(0, read), offset);
    offset += read;
  }
  return buf;
}

async function __readMsg__(conn) {
  const hdr = await __readExact__(conn, 4);
  const size = hdr[0] | (hdr[1] << 8) | (hdr[2] << 16) | (hdr[3] << 24);
  const body = await __readExact__(conn, size);
  return JSON.parse(__dec__.decode(body));
}

async function __writeMsg__(conn, obj) {
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
  await conn.write(frame);
}

// ── wait for dispatch ────────────────────────────────────────────────────────

const __poolConn__ = await Deno.connect({ transport: "unix", path: __poolSocket__ });
await __writeMsg__(__poolConn__, { ready: true });
const __dispatch__ = await __readMsg__(__poolConn__);
__poolConn__.close();

if (!__dispatch__ || !__dispatch__.socketPath || !__dispatch__.token || !__dispatch__.script) {
  throw new Error("pool shim: invalid dispatch message");
}

// Override the env vars the SDK shim reads so it connects to the task socket.
// Deno.env.set is available because we requested --allow-env.
Deno.env.set("DICODE_SOCKET", __dispatch__.socketPath);
Deno.env.set("DICODE_TOKEN", __dispatch__.token);

// ── evaluate the script (shim + user code) ──────────────────────────────────

await eval(__dispatch__.script);
