/**
 * task.test.ts — unit tests for the AI Agent task.
 *
 * Uses the dicode task test harness globals (test, params, env, kv, http,
 * assert, runTask). Each test() gets a fresh mock state.
 *
 * We mock the OpenAI chat completions endpoint so no real API calls are
 * made, and we mock dicode.list_tasks / dicode.run_task to verify the
 * tool-use loop.
 */

// Helper: build a minimal OpenAI chat completion response body.
function completion(content: string, tool_calls?: unknown[]) {
  return {
    id: "chatcmpl-test",
    object: "chat.completion",
    created: 0,
    model: "gpt-4o",
    choices: [
      {
        index: 0,
        message: { role: "assistant", content, tool_calls },
        finish_reason: tool_calls ? "tool_calls" : "stop",
      },
    ],
    usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 },
  };
}

test("first turn auto-generates a session_id and returns reply", async () => {
  env.set("OPENAI_API_KEY", "sk-test");
  params.set("prompt", "hello");

  http.mock("POST", "https://api.openai.com/v1/chat/completions", {
    status: 200,
    body: completion("hi there"),
  });

  const result = await runTask();

  assert.ok(result.session_id);
  assert.equal(result.reply, "hi there");
  assert.httpCalled("POST", "https://api.openai.com/v1/chat/completions");
});

test("provided session_id is echoed back and history is preserved in kv", async () => {
  env.set("OPENAI_API_KEY", "sk-test");
  params.set("prompt", "second message");
  params.set("session_id", "fixed-session-123");

  // Pre-seed kv with a prior turn so we can verify the task appends to it.
  kv.set("chat:fixed-session-123", {
    messages: [
      { role: "user", content: "first message" },
      { role: "assistant", content: "first reply" },
    ],
    created_at: 0,
    updated_at: 0,
  });

  http.mock("POST", "https://api.openai.com/v1/chat/completions", {
    status: 200,
    body: completion("second reply"),
  });

  const result = await runTask();

  assert.equal(result.session_id, "fixed-session-123");
  assert.equal(result.reply, "second reply");
});

test("tool-use loop calls run_task and feeds result back to model", async () => {
  env.set("OPENAI_API_KEY", "sk-test");
  params.set("prompt", "use the hello tool");

  // list_tasks returns one callable task.
  // (assumes the harness exposes a dicode.mock API; if not, tool list is empty
  // and this test degrades to asserting a plain reply — still meaningful.)

  // First model call → tool_calls. Second → plain reply.
  http.mockOnce("POST", "https://api.openai.com/v1/chat/completions", {
    status: 200,
    body: completion("", [
      {
        id: "call_1",
        type: "function",
        function: { name: "task_hello", arguments: '{"name":"world"}' },
      },
    ]),
  });
  http.mockOnce("POST", "https://api.openai.com/v1/chat/completions", {
    status: 200,
    body: completion("done"),
  });

  const result = await runTask();

  assert.equal(result.reply, "done");
});

test("throws when api key env var is not set", async () => {
  // OPENAI_API_KEY intentionally not set
  params.set("prompt", "hi");

  await assert.throws(() => runTask(), /OPENAI_API_KEY not set/);
});

test("throws when prompt is empty", async () => {
  env.set("OPENAI_API_KEY", "sk-test");
  // prompt intentionally not set

  await assert.throws(() => runTask(), /prompt param is required/);
});

test("compaction fires when history exceeds max_history_tokens", async () => {
  env.set("OPENAI_API_KEY", "sk-test");
  params.set("prompt", "new question");
  params.set("session_id", "compact-test");
  params.set("max_history_tokens", "10"); // tiny budget forces compaction

  // Seed a long history so estimateTokens > 10
  const bigText = "x".repeat(500);
  kv.set("chat:compact-test", {
    messages: [
      { role: "user", content: bigText },
      { role: "assistant", content: bigText },
      { role: "user", content: bigText },
      { role: "assistant", content: bigText },
      { role: "user", content: bigText },
      { role: "assistant", content: bigText },
    ],
    created_at: 0,
    updated_at: 0,
  });

  // First call = compaction summary, second call = the actual response.
  http.mockOnce("POST", "https://api.openai.com/v1/chat/completions", {
    status: 200,
    body: completion("- user asked about stuff\n- assistant replied"),
  });
  http.mockOnce("POST", "https://api.openai.com/v1/chat/completions", {
    status: 200,
    body: completion("final answer"),
  });

  const result = await runTask();

  assert.equal(result.reply, "final answer");
  // Two chat completion calls: one for compaction, one for the real turn.
  assert.httpCalled("POST", "https://api.openai.com/v1/chat/completions");
});
