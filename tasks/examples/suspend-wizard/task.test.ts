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

test("main asks for a project name and suspends to chooseFramework", async () => {
  const { calls, suspend } = captureSuspend();
  (dicode as Record<string, unknown>).suspend = suspend;

  let threw = false;
  try {
    await runTask();
  } catch {
    threw = true;
  }
  assert.ok(threw);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].to, "chooseFramework");

  const schema = calls[0].schema as { type: string; required: string[] };
  assert.equal(schema.type, "object");
  assert.equal(schema.required[0], "project_name");
});

test("chooseFramework carries the project name forward and suspends to confirm", async () => {
  const { calls, suspend } = captureSuspend();
  let threw = false;
  try {
    await steps.chooseFramework({
      dicode: { suspend },
      input: { project_name: "acme" },
    } as never);
  } catch {
    threw = true;
  }
  assert.ok(threw);
  assert.equal(calls[0].to, "confirm");
  assert.equal((calls[0].state as { project: string }).project, "acme");
});

test("summarize returns the collected summary", async () => {
  const result = await steps.summarize({
    input: { confirmed: true },
    state: { project: "acme", framework: "deno" },
  } as never);
  assert.equal((result as { project: string }).project, "acme");
  assert.equal((result as { framework: string }).framework, "deno");
  assert.equal((result as { confirmed: boolean }).confirmed, true);
});
