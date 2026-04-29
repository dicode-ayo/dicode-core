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

test("switch_dev_mode tool advertises branch, base, run_id args", async () => {
  input = { jsonrpc: "2.0", id: 1, method: "tools/list", params: {} };
  const result = await runTask();
  // result is the full JSON-RPC envelope: { jsonrpc, id, result: { tools: [...] } }
  const tools = (result as { result: { tools: Array<{ name: string; inputSchema: { properties: Record<string, unknown> } }> } }).result.tools;
  const tool = tools.find((t) => t.name === "switch_dev_mode");
  assert.ok(tool, "switch_dev_mode tool missing from tools/list");
  const props = tool!.inputSchema.properties;
  assert.ok(props["branch"], "branch property missing");
  assert.ok(props["base"],   "base property missing");
  assert.ok(props["run_id"], "run_id property missing");
});

test("switch_dev_mode dispatcher round-trips branch/base/run_id in hint body", async () => {
  input = {
    jsonrpc: "2.0",
    id: 2,
    method: "tools/call",
    params: {
      name: "switch_dev_mode",
      arguments: {
        source: "demo",
        enabled: true,
        branch: "fix/abc",
        base: "main",
        run_id: "r1",
      },
    },
  };
  const result = await runTask();
  const text = (result as { result: { content: Array<{ text: string }> } }).result.content[0].text;
  assert.ok(
    text.includes('"branch":"fix/abc"') || text.includes('"branch": "fix/abc"'),
    `branch missing in hint: ${text}`,
  );
  assert.ok(
    text.includes('"base":"main"') || text.includes('"base": "main"'),
    `base missing in hint: ${text}`,
  );
  assert.ok(
    text.includes('"run_id":"r1"') || text.includes('"run_id": "r1"'),
    `run_id missing in hint: ${text}`,
  );
});
