import { setupHarness } from "../../sdk-test.ts";
import { steps } from "./task.ts";
await setupHarness(import.meta.url);

// suspend isn't part of the default mock surface; capture the request and throw
// to mimic that it never returns, so the handler under test stops there.
function captureSuspend() {
  const calls: Record<string, unknown>[] = [];
  const suspend = (req: Record<string, unknown>) => {
    calls.push(req);
    throw new Error("__suspended__");
  };
  return { calls, suspend };
}

async function expectSuspend(fn: () => Promise<unknown>) {
  let threw = false;
  try { await fn(); } catch { threw = true; }
  assert.ok(threw, "handler was expected to suspend");
}

test("main scans the candidate and suspends to pickEnv", async () => {
  const { calls, suspend } = captureSuspend();
  (dicode as Record<string, unknown>).suspend = suspend;
  await expectSuspend(() => runTask());
  assert.equal(calls[0].to, "pickEnv");
  const schema = calls[0].schema as { properties: { env: { enum: string[] } } };
  assert.equal(schema.properties.env.enum.length, 3);
});

test("pickEnv: a lower env that passes checks goes straight to deploy", async () => {
  const { calls, suspend } = captureSuspend();
  await expectSuspend(() =>
    steps.pickEnv({ dicode: { suspend }, input: { env: "dev" }, state: { candidate: { version: "1.4.0" } } } as never)
  );
  assert.equal(calls[0].to, "runDeploy");
});

test("pickEnv: prod fails the coverage gate and branches to the override prompt", async () => {
  const { calls, suspend } = captureSuspend();
  await expectSuspend(() =>
    steps.pickEnv({ dicode: { suspend }, input: { env: "prod" }, state: { candidate: { version: "1.4.0" } } } as never)
  );
  assert.equal(calls[0].to, "confirmOverride");
});

test("confirmOverride: declining the override aborts without deploying", async () => {
  const result = await steps.confirmOverride({
    input: { override: false },
    state: { candidate: { version: "1.4.0" }, env: "prod" },
  } as never);
  assert.equal((result as { deployed: boolean }).deployed, false);
});

test("confirmOverride: overriding a lower env proceeds to the deploy note", async () => {
  const { calls, suspend } = captureSuspend();
  await expectSuspend(() =>
    steps.confirmOverride({
      dicode: { suspend },
      input: { override: true },
      state: { candidate: { version: "1.4.0" }, env: "staging" },
    } as never)
  );
  assert.equal(calls[0].to, "runDeploy");
});

test("confirmProd: declining the prod confirmation aborts without deploying", async () => {
  const result = await steps.confirmProd({
    input: { confirm: false },
    state: { candidate: { version: "1.4.0" }, env: "prod" },
  } as never);
  assert.equal((result as { deployed: boolean }).deployed, false);
});

test("confirmProd: confirming prod proceeds to the deploy note", async () => {
  const { calls, suspend } = captureSuspend();
  await expectSuspend(() =>
    steps.confirmProd({
      dicode: { suspend },
      input: { confirm: true },
      state: { candidate: { version: "1.4.0" }, env: "prod" },
    } as never)
  );
  assert.equal(calls[0].to, "runDeploy");
});

test("runDeploy: performs the async deploy and returns the result", async () => {
  const result = await steps.runDeploy({
    input: { note: "ship it" },
    state: { candidate: { version: "1.4.0" }, env: "dev" },
  } as never);
  assert.equal((result as { deployed: boolean }).deployed, true);
  assert.equal((result as { version: string }).version, "1.4.0");
  assert.ok((result as { url: string }).url.includes("dev"));
});
