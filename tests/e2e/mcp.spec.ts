/**
 * mcp.spec.ts
 *
 * End-to-end tests for the buildin/mcp task that exposes dicode as an MCP
 * (Model Context Protocol) server. Confirms the JSON-RPC 2.0 surface served
 * at /mcp (the legacy URL, forwarded into the buildin/mcp webhook task) works
 * for the four flows MCP clients exercise: initialize, tools/list, tools/call,
 * and error paths.
 */

import { test, expect } from '@playwright/test';

const MCP_URL = '/mcp';

interface JsonRpcResponse<T = unknown> {
  jsonrpc: '2.0';
  id: unknown;
  result?: T;
  error?: { code: number; message: string };
}

interface ToolsListResult {
  tools: Array<{
    name: string;
    description: string;
    inputSchema: { type: string; properties?: Record<string, unknown>; required?: string[] };
  }>;
}

interface ToolsCallResult {
  content: Array<{ type: 'text'; text: string }>;
}

test.describe('MCP — server-info probe (GET /mcp)', () => {
  test('returns server-info JSON', async ({ request }) => {
    const res = await request.get(MCP_URL);
    expect(res.ok()).toBe(true);
    const body = (await res.json()) as { name: string; version: string; protocol: string };
    expect(body.name).toBe('dicode');
    expect(body.protocol).toBe('mcp/2024-11-05');
  });
});

test.describe('MCP — JSON-RPC dispatch', () => {
  test('initialize returns capabilities + protocolVersion', async ({ request }) => {
    const res = await request.post(MCP_URL, {
      headers: { 'Content-Type': 'application/json' },
      data: { jsonrpc: '2.0', id: 1, method: 'initialize' },
    });
    expect(res.ok()).toBe(true);
    const body = (await res.json()) as JsonRpcResponse<{
      protocolVersion: string;
      capabilities: { tools: Record<string, unknown> };
      serverInfo: { name: string; version: string };
    }>;
    expect(body.jsonrpc).toBe('2.0');
    expect(body.id).toBe(1);
    expect(body.result?.protocolVersion).toBe('2024-11-05');
    expect(body.result?.capabilities.tools).toBeDefined();
    expect(body.result?.serverInfo.name).toBe('dicode');
  });

  test('tools/list returns the six expected tool definitions', async ({ request }) => {
    const res = await request.post(MCP_URL, {
      headers: { 'Content-Type': 'application/json' },
      data: { jsonrpc: '2.0', id: 2, method: 'tools/list' },
    });
    expect(res.ok()).toBe(true);
    const body = (await res.json()) as JsonRpcResponse<ToolsListResult>;
    expect(body.result).toBeDefined();
    const names = body.result!.tools.map((t) => t.name).sort();
    expect(names).toEqual([
      'get_task',
      'list_sources',
      'list_tasks',
      'run_task',
      'switch_dev_mode',
      'test_task',
    ]);
    // Every tool must declare an inputSchema with type:object — MCP clients
    // reject tools missing this.
    for (const tool of body.result!.tools) {
      expect(tool.inputSchema.type).toBe('object');
    }
  });

  test('tools/call list_tasks returns dicode task list', async ({ request }) => {
    const res = await request.post(MCP_URL, {
      headers: { 'Content-Type': 'application/json' },
      data: {
        jsonrpc: '2.0',
        id: 3,
        method: 'tools/call',
        params: { name: 'list_tasks' },
      },
    });
    expect(res.ok()).toBe(true);
    const body = (await res.json()) as JsonRpcResponse<ToolsCallResult>;
    expect(body.result?.content[0].type).toBe('text');
    const parsed = JSON.parse(body.result!.content[0].text) as Array<{ id: string }>;
    const ids = parsed.map((t) => t.id);
    // The mcp task itself plus the e2e-tests fixtures should both be visible.
    expect(ids).toContain('e2e-tests/mcp');
    expect(ids).toContain('e2e-tests/hello-webhook');
  });

  test('tools/call get_task returns the spec for a known task', async ({ request }) => {
    const res = await request.post(MCP_URL, {
      headers: { 'Content-Type': 'application/json' },
      data: {
        jsonrpc: '2.0',
        id: 4,
        method: 'tools/call',
        params: { name: 'get_task', arguments: { id: 'e2e-tests/hello-webhook' } },
      },
    });
    expect(res.ok()).toBe(true);
    const body = (await res.json()) as JsonRpcResponse<ToolsCallResult>;
    const parsed = JSON.parse(body.result!.content[0].text) as { id: string; name: string };
    expect(parsed.id).toBe('e2e-tests/hello-webhook');
    expect(parsed.name).toBeTruthy();
  });

  test('tools/call get_task with unknown id returns a JSON-RPC error', async ({ request }) => {
    const res = await request.post(MCP_URL, {
      headers: { 'Content-Type': 'application/json' },
      data: {
        jsonrpc: '2.0',
        id: 5,
        method: 'tools/call',
        params: { name: 'get_task', arguments: { id: 'nonexistent/task' } },
      },
    });
    expect(res.ok()).toBe(true);
    const body = (await res.json()) as JsonRpcResponse;
    expect(body.error).toBeDefined();
    expect(body.error!.code).toBe(-32603);
    expect(body.error!.message).toContain('not found');
  });

  test('tools/call list_sources returns a hint string (not a hard error)', async ({ request }) => {
    const res = await request.post(MCP_URL, {
      headers: { 'Content-Type': 'application/json' },
      data: {
        jsonrpc: '2.0',
        id: 6,
        method: 'tools/call',
        params: { name: 'list_sources' },
      },
    });
    expect(res.ok()).toBe(true);
    const body = (await res.json()) as JsonRpcResponse<ToolsCallResult>;
    // The buildin task intentionally returns a hint pointing at /api/sources
    // — MCP clients have the dicode API key already and can call it directly.
    expect(body.result?.content[0].text).toContain('/api/sources');
  });

  test('unknown method returns -32601', async ({ request }) => {
    const res = await request.post(MCP_URL, {
      headers: { 'Content-Type': 'application/json' },
      data: { jsonrpc: '2.0', id: 7, method: 'bogus/method' },
    });
    expect(res.ok()).toBe(true);
    const body = (await res.json()) as JsonRpcResponse;
    expect(body.error?.code).toBe(-32601);
  });

  test('tools/call with unknown tool name returns -32603', async ({ request }) => {
    const res = await request.post(MCP_URL, {
      headers: { 'Content-Type': 'application/json' },
      data: {
        jsonrpc: '2.0',
        id: 8,
        method: 'tools/call',
        params: { name: 'no_such_tool' },
      },
    });
    expect(res.ok()).toBe(true);
    const body = (await res.json()) as JsonRpcResponse;
    expect(body.error?.code).toBe(-32603);
    expect(body.error?.message).toContain('unknown tool');
  });

  test('empty body returns parse error -32700 with id:null', async ({ request }) => {
    const res = await request.post(MCP_URL, {
      headers: { 'Content-Type': 'application/json' },
      data: '',
    });
    expect(res.ok()).toBe(true);
    const body = (await res.json()) as JsonRpcResponse;
    expect(body.error?.code).toBe(-32700);
    expect(body.id).toBeNull();
  });
});
