// Unit tests for the pure parts of shim.ts. Importing the shim opens the IPC
// connection, so a throwaway server answers the handshake and the two startup
// calls (input, resume). Run via TestDenoSDK in sdk_test.go.

function assertEquals(got: unknown, want: unknown, msg = ""): void {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  if (g !== w) throw new Error(`${msg} got ${g}, want ${w}`);
}

function assertThrows(fn: () => unknown, _cls: unknown, contains: string): void {
  try {
    fn();
  } catch (e) {
    if (!(e as Error).message.includes(contains)) throw new Error(`threw ${(e as Error).message}, want ${contains}`);
    return;
  }
  throw new Error("did not throw");
}

async function readFrame(conn: Deno.Conn): Promise<Record<string, unknown> | null> {
  const hdr = new Uint8Array(4);
  let n = 0;
  while (n < 4) {
    const r = await conn.read(hdr.subarray(n));
    if (r === null) return null;
    n += r;
  }
  const size = hdr[0] | (hdr[1] << 8) | (hdr[2] << 16) | (hdr[3] << 24);
  const body = new Uint8Array(size);
  n = 0;
  while (n < size) {
    const r = await conn.read(body.subarray(n));
    if (r === null) return null;
    n += r;
  }
  return JSON.parse(new TextDecoder().decode(body));
}

async function writeFrame(conn: Deno.Conn, obj: unknown): Promise<void> {
  const body = new TextEncoder().encode(JSON.stringify(obj));
  const frame = new Uint8Array(4 + body.length);
  new DataView(frame.buffer).setUint32(0, body.length, true);
  frame.set(body, 4);
  let n = 0;
  while (n < frame.length) n += await conn.write(frame.subarray(n));
}

// Loads a fresh copy of the shim against a fake daemon that reports the given
// resume reply, then closes the connection so the module can be inspected.
async function loadShim(resume: unknown, tag: string) {
  const dir = await Deno.makeTempDir();
  const sock = `${dir}/ipc.sock`;
  const listener = Deno.listen({ transport: "unix", path: sock });
  const served = (async () => {
    const conn = await listener.accept();
    await readFrame(conn);
    await writeFrame(conn, { proto: 1, caps: [] });
    while (true) {
      const req = await readFrame(conn);
      if (req === null) break;
      if (req.method === "input") await writeFrame(conn, { id: req.id, result: null });
      else if (req.method === "resume") await writeFrame(conn, { id: req.id, result: resume });
    }
  })();
  Deno.env.set("DICODE_SOCKET", sock);
  Deno.env.set("DICODE_TOKEN", "t");
  const shim = await import(`./shim.ts?${tag}`);
  return {
    shim,
    close: async () => {
      shim.__conn__.close();
      await served.catch(() => {});
      listener.close();
      await Deno.remove(dir, { recursive: true });
    },
  };
}

const main = () => "main";
const resumeFn = () => "resume";
const stepB = () => "b";

Deno.test("__selectHandler__ covers the dispatch branches", async () => {
  const { shim, close } = await loadShim(null, "select");
  try {
    const sel = shim.__selectHandler__;
    // first run → main, even when resume/steps are exported
    assertEquals(sel({ default: main, resume: resumeFn, steps: { b: stepB } }, false, "b"), main);
    // resume with a step marker → steps[step]
    assertEquals(sel({ default: main, steps: { b: stepB } }, true, "b"), stepB);
    // resume without a marker → exported resume
    assertEquals(sel({ default: main, resume: resumeFn }, true, undefined), resumeFn);
    // resume with nothing else exported → main
    assertEquals(sel({ default: main }, true, undefined), main);
    // marker names no exported step → fail loudly instead of re-running main
    assertThrows(() => sel({ default: main, steps: { b: stepB } }, true, "typo"), Error, 'resume step "typo"');
    assertThrows(() => sel({ default: main, steps: { b: "x" } }, true, "b"), Error, "not an exported step function");
    // no handler at all
    assertThrows(() => sel({}, false, undefined), Error, "no default export");
  } finally {
    await close();
  }
});

Deno.test("__unwrapState__ splits the envelope from the author's state", async () => {
  const { shim, close } = await loadShim(null, "unwrap");
  try {
    assertEquals(shim.__unwrapState__({ __step: "b", state: { x: 1 } }), { step: "b", state: { x: 1 } });
    assertEquals(shim.__unwrapState__({ __step: null, state: 3 }), { step: undefined, state: 3 });
    assertEquals(shim.__unwrapState__({ plain: true }), { step: undefined, state: { plain: true } });
    assertEquals(shim.__unwrapState__(undefined), { step: undefined, state: undefined });
  } finally {
    await close();
  }
});

Deno.test("resume reply populates the step marker, state and input", async () => {
  const { shim, close } = await loadShim(
    { resumed: true, state: { __step: "b", state: { x: 1 } }, input: { y: 2 } },
    "resumed",
  );
  try {
    assertEquals(shim.__resumed__, true);
    assertEquals(shim.__resumeStep__, "b");
    assertEquals(shim.state, { x: 1 });
    assertEquals(shim.input, { y: 2 });
  } finally {
    await close();
  }
});

Deno.test("suspend envelope matches the shared fixture", async () => {
  const cases = JSON.parse(await Deno.readTextFile(new URL("../../testdata/suspend_envelopes.json", import.meta.url)));
  const { shim, close } = await loadShim(null, "envelope");
  try {
    for (const c of cases) {
      assertEquals(shim.__suspendEnvelope__(c.to, c.state), c.envelope, c.name);
    }
  } finally {
    await close();
  }
});
