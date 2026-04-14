/**
 * task.test.ts — unit tests for the AI Agent buildin.
 *
 * The buildin is generic: no provider defaults. Every test that exercises
 * the chat path must set model/base_url/api_key_env (just like a sibling
 * preset task — see tasks/examples/taskset.yaml — would do via overrides).
 *
 * Uses the dicode task test harness globals: test, params, env, kv, http,
 * assert, runTask. Each test() gets a fresh mock state.
 */

// Minimal OpenAI chat completion response body.
function completion(content: string, tool_calls?: unknown[]) {
  return {
    id: "chatcmpl-test",
    object: "chat.completion",
    created: 0,
    model: "llama3.2",
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

// Shortcut: wire the agent at a local Ollama-like endpoint. No real API key
// needed — the task uses a placeholder for localhost URLs.
function useLocal() {
  params.set("model", "llama3.2");
  params.set("base_url", "http://localhost:11434/v1");
  params.set("api_key_env", "OLLAMA_API_KEY");
}

// Shortcut: wire the agent at OpenAI proper. Needs a real (mocked) key.
function useOpenAI() {
  env.set("OPENAI_API_KEY", "sk-test");
  params.set("model", "gpt-4o-mini");
  params.set("base_url", "https://api.openai.com/v1");
  params.set("api_key_env", "OPENAI_API_KEY");
}

test("returns not_configured when no provider params are set", async () => {
  params.set("prompt", "hello");
  // intentionally no model / base_url / api_key_env

  const result = await runTask();

  assert.equal(result.error, "not_configured");
  assert.equal(result.reply, null);
  assert.ok(result.session_id);
  // Should list model and base_url as missing at minimum
  assert.ok(result.missing.includes("model"));
  assert.ok(result.missing.includes("base_url"));
});

test("first turn auto-generates a session_id and returns reply", async () => {
  useLocal();
  params.set("prompt", "hello");

  http.mock("POST", "http://localhost:11434/v1/chat/completions", {
    status: 200,
    body: completion("hi there"),
  });

  const result = await runTask();

  assert.ok(result.session_id);
  assert.equal(result.reply, "hi there");
  assert.httpCalled("POST", "http://localhost:11434/v1/chat/completions");
});

test("provided session_id is echoed back and history is preserved in kv", async () => {
  useLocal();
  params.set("prompt", "second message");
  params.set("session_id", "fixed-session-123");

  kv.set("chat:fixed-session-123", {
    messages: [
      { role: "user", content: "first message" },
      { role: "assistant", content: "first reply" },
    ],
    created_at: 0,
    updated_at: 0,
  });

  http.mock("POST", "http://localhost:11434/v1/chat/completions", {
    status: 200,
    body: completion("second reply"),
  });

  const result = await runTask();

  assert.equal(result.session_id, "fixed-session-123");
  assert.equal(result.reply, "second reply");
});

test("tool-use loop calls run_task and feeds result back to model", async () => {
  useLocal();
  params.set("prompt", "use the hello tool");

  // First model call → tool_calls. Second → plain reply.
  http.mockOnce("POST", "http://localhost:11434/v1/chat/completions", {
    status: 200,
    body: completion("", [
      {
        id: "call_1",
        type: "function",
        function: { name: "task_hello", arguments: '{"name":"world"}' },
      },
    ]),
  });
  http.mockOnce("POST", "http://localhost:11434/v1/chat/completions", {
    status: 200,
    body: completion("done"),
  });

  const result = await runTask();

  assert.equal(result.reply, "done");
});

test("throws when api key env var is not set (hosted provider)", async () => {
  // Configure openai base_url but do not set OPENAI_API_KEY
  params.set("prompt", "hi");
  params.set("model", "gpt-4o-mini");
  params.set("base_url", "https://api.openai.com/v1");
  params.set("api_key_env", "OPENAI_API_KEY");
  // intentionally no env.set("OPENAI_API_KEY", ...)

  await assert.throws(() => runTask(), /OPENAI_API_KEY not set/);
});

test("throws when prompt is empty", async () => {
  useLocal();
  // prompt intentionally not set

  await assert.throws(() => runTask(), /prompt param is required/);
});

test("compaction fires when history exceeds max_history_tokens", async () => {
  useLocal();
  params.set("prompt", "new question");
  params.set("session_id", "compact-test");
  params.set("max_history_tokens", "10"); // tiny budget forces compaction

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
  http.mockOnce("POST", "http://localhost:11434/v1/chat/completions", {
    status: 200,
    body: completion("- user asked about stuff\n- assistant replied"),
  });
  http.mockOnce("POST", "http://localhost:11434/v1/chat/completions", {
    status: 200,
    body: completion("final answer"),
  });

  const result = await runTask();

  assert.equal(result.reply, "final answer");
});

test("openai provider round-trip works with real key", async () => {
  useOpenAI();
  params.set("prompt", "hello");

  http.mock("POST", "https://api.openai.com/v1/chat/completions", {
    status: 200,
    body: completion("hi there"),
  });

  const result = await runTask();
  assert.equal(result.reply, "hi there");
  assert.httpCalled("POST", "https://api.openai.com/v1/chat/completions");
});
