// The SDK has no http.register wrapper, so this opens a second IPC connection
// with the run's token and speaks the length-prefixed JSON protocol directly.
const PATTERN = '/hooks/test-http-register';
const enc = new TextEncoder();
const dec = new TextDecoder();

async function readExact(conn, n) {
  const buf = new Uint8Array(n);
  let off = 0;
  while (off < n) {
    const got = await conn.read(buf.subarray(off));
    if (got === null) throw new Error('ipc: connection closed');
    off += got;
  }
  return buf;
}

async function readMsg(conn) {
  const hdr = await readExact(conn, 4);
  const size = hdr[0] | (hdr[1] << 8) | (hdr[2] << 16) | (hdr[3] << 24);
  return JSON.parse(dec.decode(await readExact(conn, size)));
}

async function writeMsg(conn, obj) {
  const body = enc.encode(JSON.stringify(obj));
  const frame = new Uint8Array(4 + body.length);
  new DataView(frame.buffer).setUint32(0, body.length, true);
  frame.set(body, 4);
  let off = 0;
  while (off < frame.length) off += await conn.write(frame.subarray(off));
}

async function connect() {
  const addr = Deno.env.get('DICODE_SOCKET');
  if (Deno.build.os === 'windows') {
    const sep = addr.lastIndexOf(':');
    return await Deno.connect({ hostname: addr.slice(0, sep), port: Number(addr.slice(sep + 1)) });
  }
  return await Deno.connect({ transport: 'unix', path: addr });
}

export default async function main() {
  const conn = await connect();
  await writeMsg(conn, { token: Deno.env.get('DICODE_TOKEN') });
  const hs = await readMsg(conn);
  if (hs.error) throw new Error('handshake: ' + hs.error);
  await writeMsg(conn, { id: 'reg', method: 'http.register', pattern: PATTERN });
  for (;;) {
    const msg = await readMsg(conn);
    if (msg.id === 'reg') {
      if (msg.error) throw new Error('http.register: ' + msg.error);
      console.log('registered ' + PATTERN);
      continue;
    }
    if (!msg.requestID) continue;
    const body = JSON.stringify({ servedBy: 'http-register-guarded', method: msg.httpMethod, path: msg.path });
    await writeMsg(conn, {
      method: 'http.respond',
      requestID: msg.requestID,
      status: 200,
      respHeaders: { 'Content-Type': 'application/json' },
      respBody: btoa(body),
    });
  }
}
