/**
 * task.test.ts — unit tests for the MCP buildin task.
 *
 * Run with:
 *   deno test --allow-read tasks/buildin/mcp/task.test.ts
 * or:
 *   make test-tasks
 */
import { setupHarness } from "../../sdk-test.ts";
await setupHarness(import.meta.url);

type ToolsList = {
  result: { tools: Array<{ name: string; inputSchema: { properties: Record<string, unknown> } }> };
};
type ToolCall = { result: { content: Array<{ text: string }> }; error?: { message: string } };

function call(name: string, args: Record<string, unknown>) {
  input = { jsonrpc: "2.0", id: 1, method: "tools/call", params: { name, arguments: args } };
}

async function textOf(): Promise<string> {
  const result = (await runTask()) as ToolCall;
  assert.ok(!result.error, `unexpected JSON-RPC error: ${result.error?.message}`);
  return result.result.content[0].text;
}

test("switch_dev_mode tool advertises branch, base, run_id args", async () => {
  input = { jsonrpc: "2.0", id: 1, method: "tools/list", params: {} };
  const tools = ((await runTask()) as ToolsList).result.tools;
  const tool = tools.find((t) => t.name === "switch_dev_mode");
  assert.ok(tool, "switch_dev_mode tool missing from tools/list");
  const props = tool!.inputSchema.properties;
  assert.ok(props["branch"], "branch property missing");
  assert.ok(props["base"],   "base property missing");
  assert.ok(props["run_id"], "run_id property missing");
});

// local_path redirects the daemon's taskset resolution at any host path, so it
// is not the caller's to set (#740). Absent from the schema and dropped by the
// dispatcher — the two tests below pin both halves.
test("switch_dev_mode tool does not advertise local_path", async () => {
  input = { jsonrpc: "2.0", id: 1, method: "tools/list", params: {} };
  const tools = ((await runTask()) as ToolsList).result.tools;
  const tool = tools.find((t) => t.name === "switch_dev_mode");
  assert.ok(!tool!.inputSchema.properties["local_path"], "local_path is exposed as a tool argument");
});

test("switch_dev_mode dispatcher drops a local_path the caller sends anyway", async () => {
  call("switch_dev_mode", { source: "demo", enabled: true, branch: "fix/abc", local_path: "/etc" });
  await textOf();
  const sent = dicode._setDevModeCalls[0];
  assert.equal(sent.local_path, undefined);
});

test("switch_dev_mode dispatcher forwards branch/base/run_id to the SDK", async () => {
  call("switch_dev_mode", {
    source: "demo",
    enabled: true,
    branch: "fix/abc",
    base: "main",
    run_id: "r1",
  });
  const text = await textOf();
  assert.equal(dicode._setDevModeCalls.length, 1);
  const sent = dicode._setDevModeCalls[0];
  assert.equal(sent.name, "demo");
  assert.equal(sent.enabled, true);
  assert.equal(sent.branch, "fix/abc");
  assert.equal(sent.base, "main");
  assert.equal(sent.run_id, "r1");
  // The tool returns what the daemon returned, not a pointer to another call.
  assert.ok(text.includes('"ok"'), `set_dev_mode result missing from reply: ${text}`);
});

// An MCP client's arguments are not validated against the tool schema before
// they reach here, and a model asking to leave dev mode may well send the
// string "false". Reading that as true would enter dev mode instead.
test("switch_dev_mode reads a stringified boolean the way the caller meant it", async () => {
  call("switch_dev_mode", { source: "demo", enabled: "false" });
  await textOf();
  assert.equal(dicode._setDevModeCalls[0].enabled, false);
});

test("switch_dev_mode requires a source", async () => {
  call("switch_dev_mode", { enabled: true });
  const result = (await runTask()) as ToolCall;
  assert.ok(result.error, "expected an error for a missing source");
  assert.equal(dicode._setDevModeCalls.length, 0);
});

test("test_task runs the task's tests and returns the result", async () => {
  call("test_task", { id: "demo/thing" });
  const text = await textOf();
  assert.equal(dicode._testTaskCalls.length, 1);
  assert.equal(dicode._testTaskCalls[0], "demo/thing");
  // The reply carries the daemon's result verbatim, keyed as pkg/tasktest
  // spells it.
  assert.ok(text.includes('"taskID"'), `test result missing from reply: ${text}`);
  assert.ok(text.includes("demo/thing"), `test result missing from reply: ${text}`);
});

test("test_task requires an id", async () => {
  call("test_task", {});
  const result = (await runTask()) as ToolCall;
  assert.ok(result.error, "expected an error for a missing id");
  assert.equal(dicode._testTaskCalls.length, 0);
});

test("list_sources returns the daemon's source listing", async () => {
  call("list_sources", {});
  dicode._sources.push({ name: "demo", type: "taskset", dev_mode: false });
  const text = await textOf();
  assert.ok(text.includes("demo"), `source listing missing from reply: ${text}`);
});
