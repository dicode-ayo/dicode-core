/**
 * task.test.ts — unit tests for the AI Agent buildin.
 *
 * The buildin is generic: no provider defaults. Every test that exercises
 * the chat path must set model/base_url/api_key_env (just like a sibling
 * preset task — see tasks/examples/taskset.yaml — would do via overrides).
 *
 * Uses the dicode task test harness globals: test, params, env, kv, http,
 * assert, runTask. Each test() gets a fresh mock state.
 *
 * Run with:
 *   make test-tasks
 */
import { setupHarness } from "../../sdk-test.ts";
import { isValidSessionId } from "../ai-agent-core/chat.ts";
await setupHarness(import.meta.url);

// Loaded AFTER setupHarness patches globalThis.fetch — a static import would
// evaluate task.ts's `npm:openai` dependency before the patch, so the client
// the task builds would bind the real fetch and skip the http mocks.
const { steps } = await import("./task.ts") as {
  steps: { turn: (ctx: unknown) => Promise<unknown> };
};

// dicode.suspend never returns in production (the process exits). Tests must
// stop execution the same way: install a suspend that records the request and
// throws a sentinel, then inspect the recorded calls.
class SuspendSignal extends Error {}
function recordSuspend(): Array<Record<string, unknown>> {
  const calls: Array<Record<string, unknown>> = [];
  (dicode as Record<string, unknown>).suspend = (req: Record<string, unknown>) => {
    calls.push(req);
    throw new SuspendSignal();
  };
  return calls;
}

// Drive one chat turn through steps.turn with the harness mocks, mirroring how
// the runner dispatches a resume.
// deno-lint-ignore no-explicit-any
function runTurnStep(input: unknown, state: unknown): Promise<any> {
  return steps.turn({
    params,
    kv,
    input,
    state,
    dicode,
    output: {},
    mcp: {},
  }) as Promise<Record<string, unknown>>;
}

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

// Shortcut: wire the agent at a local Ollama-like endpoint. The task still
// enforces that api_key_env is a real env var, so we stub a placeholder.
function useLocal() {
  params.set("model", "llama3.2");
  params.set("base_url", "http://localhost:11434/v1");
  params.set("api_key_env", "OLLAMA_API_KEY");
  env.set("OLLAMA_API_KEY", "stub-for-local");
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

// A UUID-shaped fixture: session_id now goes through isValidSessionId before
// it's used as the chat: group label / echoed back, so a valid-path test needs
// a UUID-shaped value. The rejection path is covered separately below.
const VALID_SESSION_ID = "44444444-4444-4444-4444-444444444444";

test("tags the run with the chat session group so the WebUI can collapse turns", async () => {
  // #112: every chat turn calls dicode.set_group(`chat:<sessionId>`) so all
  // turns of one conversation collapse into one expandable row in the run
  // list, while a new session_id produces a new group row.
  useLocal();
  params.set("prompt", "hello");
  params.set("session_id", VALID_SESSION_ID);

  http.mock("POST", "http://localhost:11434/v1/chat/completions", {
    status: 200,
    body: completion("hi"),
  });

  await runTask();

  const calls = (dicode as Record<string, unknown>)._setGroupCalls as string[];
  assert.equal(calls, [`chat:${VALID_SESSION_ID}`]);
});

test("provided session_id is echoed back on the one-shot path", async () => {
  useLocal();
  params.set("prompt", "second message");
  params.set("session_id", VALID_SESSION_ID);

  http.mock("POST", "http://localhost:11434/v1/chat/completions", {
    status: 200,
    body: completion("second reply"),
  });

  const result = await runTask();

  assert.equal(result.session_id, VALID_SESSION_ID);
  assert.equal(result.reply, "second reply");
});

test("rejects a malformed session_id param; generates a fresh UUID instead of echoing it", async () => {
  useLocal();
  params.set("prompt", "hello");
  // A crafted caller trying to smuggle something odd into the run-group label
  // / KV-style handle instead of a dicode-minted UUID.
  params.set("session_id", "../../etc/passwd\nSET x=1");

  http.mock("POST", "http://localhost:11434/v1/chat/completions", {
    status: 200,
    body: completion("hi"),
  });

  const result = await runTask();

  assert.ok(isValidSessionId(result.session_id as string), `expected a fresh UUID, got ${result.session_id}`);

  const calls = (dicode as Record<string, unknown>)._setGroupCalls as string[];
  assert.equal(calls.length, 1);
  assert.ok(!calls[0].includes("passwd"), `malformed session_id leaked into set_group label: ${calls[0]}`);
  assert.equal(calls[0], `chat:${result.session_id}`);
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

test("blank prompt on a fresh run opens the chat loop (suspends to turn)", async () => {
  // prompt intentionally not set → chat-start, not the one-shot path.
  const calls = recordSuspend();
  let signalled = false;
  try {
    await runTask();
  } catch (e) {
    if (e instanceof SuspendSignal) signalled = true;
    else throw e;
  }

  assert.equal(signalled, true);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].to, "turn");
  // `message` is intentionally NOT required so a blank line ends the chat.
  assert.equal((calls[0].schema as Record<string, unknown>).required, undefined);
  // Conversation state rides in the suspend blob, seeded empty.
  assert.equal((calls[0].state as Record<string, unknown>).messages, []);
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

test("self-id filter excludes only the exact task_id, not prefix matches", async () => {
  // Self-recursion prevention must compare task ids for EXACT equality, not
  // prefix or substring. Previously used a regex on "/ai-agent(-|$)/" which
  // would wrongly exclude things like "team/ai-agent-helper". With
  // dicode.task_id the filter is a simple !== check — this test guards
  // against regressing back to prefix matching.
  useLocal();
  params.set("prompt", "hi");

  dicode.task_id = "buildin/ai-agent";
  dicode.list_tasks = async () => [
    { id: "buildin/ai-agent" },       // self — must be excluded
    { id: "buildin/ai-agent-helper" }, // looks like self, must NOT be excluded
    { id: "team/ai-agent" },           // matches basename, must NOT be excluded
    { id: "other/something" },
  ];

  http.mock("POST", "http://localhost:11434/v1/chat/completions", {
    status: 200,
    body: completion("ok"),
  });

  await runTask();

  // Capture the tools array the agent sent to the model on the last call.
  const sent = http.lastRequestBody("POST", "http://localhost:11434/v1/chat/completions");
  const toolNames: string[] = (sent.tools ?? []).map(
    (t: { function: { name: string } }) => t.function.name,
  );

  // Tool name mangling (pkg taskIdToToolName) replaces `/` with `_` and
  // leaves `-` alone: task_buildin_ai-agent-helper, not …_helper.
  assert.ok(!toolNames.includes("task_buildin_ai-agent"), "self must be excluded");
  assert.ok(toolNames.includes("task_buildin_ai-agent-helper"), "look-alike sibling must NOT be excluded");
  assert.ok(toolNames.includes("task_team_ai-agent"), "name collision in a different namespace must NOT be excluded");
  assert.ok(toolNames.includes("task_other_something"), "unrelated task must remain");
});

test("temperature reaches the model: the tool loop is not left at the provider's chat default", async () => {
  // At a provider's chat default a model emits its tool call as prose in
  // `content`, which the loop cannot see — the turn then settles as a final
  // answer having called nothing (#756). The value has to be on the request:
  // a provider-side model default (e.g. an ollama Modelfile) does not apply
  // to requests arriving over the API.
  useLocal();
  params.set("prompt", "hello");
  params.set("temperature", "0");

  http.mock("POST", "http://localhost:11434/v1/chat/completions", {
    status: 200,
    body: completion("hi"),
  });

  await runTask();

  const sent = http.lastRequestBody("POST", "http://localhost:11434/v1/chat/completions");
  assert.equal(sent.temperature, 0);
});

test("a temperature outside 0-2 is refused rather than silently clamped", async () => {
  useLocal();
  params.set("prompt", "hello");
  params.set("temperature", "7");

  let threw = false;
  try {
    await runTask();
  } catch (e) {
    threw = true;
    assert.ok(
      String(e).includes("temperature"),
      `error should name the offending param, got: ${e}`,
    );
  }
  assert.ok(threw, "an out-of-range temperature must fail the run, not reach the provider");
});

test("refuses to run when dicode.task_id is empty", async () => {
  // A handshake regression that wipes task_id must not silently disable the
  // self-recursion guard above. The task throws a descriptive error so
  // operators see the misconfiguration immediately.
  useLocal();
  params.set("prompt", "hi");
  dicode.task_id = "";

  await assert.throws(() => runTask(), /dicode\.task_id is empty/);
});

// --- chat loop -------------------------------------------------------------

test("chat turn runs one OpenAI turn and suspends back with the reply", async () => {
  useLocal();
  const calls = recordSuspend();

  http.mock("POST", "http://localhost:11434/v1/chat/completions", {
    status: 200,
    body: completion("pong"),
  });

  let signalled = false;
  try {
    await runTurnStep({ message: "ping" }, { messages: [] });
  } catch (e) {
    if (e instanceof SuspendSignal) signalled = true;
    else throw e;
  }

  assert.equal(signalled, true);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].to, "turn");
  // The reply becomes the next prompt's banner.
  assert.equal((calls[0].schema as Record<string, unknown>).description, "pong");
  // Cumulative conversation is carried forward in the suspend state (not KV).
  const msgs = (calls[0].state as { messages: Array<{ role: string; content: string }> }).messages;
  assert.equal(msgs[0].role, "user");
  assert.equal(msgs[0].content, "ping");
  assert.equal(msgs[msgs.length - 1].role, "assistant");
  assert.equal(msgs[msgs.length - 1].content, "pong");
});

test("chat turn threads prior messages from the carried state", async () => {
  useLocal();
  recordSuspend();

  http.mock("POST", "http://localhost:11434/v1/chat/completions", {
    status: 200,
    body: completion("second reply"),
  });

  try {
    await runTurnStep(
      { message: "second message" },
      { messages: [{ role: "user", content: "first message" }, { role: "assistant", content: "first reply" }] },
    );
  } catch (e) {
    if (!(e instanceof SuspendSignal)) throw e;
  }

  // The prior turns from state are replayed to the model on this turn.
  const sent = http.lastRequestBody("POST", "http://localhost:11434/v1/chat/completions");
  const contents: string[] = (sent.messages ?? []).map((m: { content: string }) => m.content);
  assert.ok(contents.includes("first message"), "prior user turn must be replayed");
  assert.ok(contents.includes("first reply"), "prior assistant turn must be replayed");
  assert.ok(contents.includes("second message"), "new user turn must be present");
});

test("chat turn: a blank message ends the chat (returns, no OpenAI call)", async () => {
  useLocal();
  const calls = recordSuspend();

  const result = await runTurnStep(
    { message: "   " },
    { messages: [{ role: "user", content: "hi" }, { role: "assistant", content: "hello" }] },
  );

  assert.equal(result.ok, true);
  assert.equal(result.reply, "(chat ended)");
  assert.equal(calls.length, 0); // never suspended onward
  assert.httpNotCalled("POST", "http://localhost:11434/v1/chat/completions");
});

test("chat turn surfaces not_configured when no provider is set", async () => {
  // No useLocal(): model/base_url unset. The turn returns a failure the envelope
  // hands back verbatim instead of suspending onward.
  const calls = recordSuspend();

  const result = await runTurnStep({ message: "hi" }, { messages: [] });

  assert.equal(result.ok, false);
  assert.equal(result.error, "not_configured");
  assert.ok(result.missing.includes("model"));
  assert.equal(calls.length, 0);
});

// ─── capability-gated built-in tools (#735) ──────────────────────────────

// Collect the tool names the agent offered the model on the last request.
function offeredTools(): string[] {
  const sent = http.lastRequestBody("POST", "http://localhost:11434/v1/chat/completions");
  return (sent.tools ?? []).map((t: { function: { name: string } }) => t.function.name);
}

// Drive one turn whose first model response calls `name` with `args`, and whose
// second is a plain reply. Returns the tool result the agent fed back.
async function runBuiltinCall(name: string, args: Record<string, unknown>) {
  http.mockOnce("POST", "http://localhost:11434/v1/chat/completions", {
    status: 200,
    body: completion("", [
      { id: "call_1", type: "function", function: { name, arguments: JSON.stringify(args) } },
    ]),
  });
  http.mockOnce("POST", "http://localhost:11434/v1/chat/completions", {
    status: 200,
    body: completion("done"),
  });
  const result = await runTask();
  const sent = http.lastRequestBody("POST", "http://localhost:11434/v1/chat/completions");
  const toolMsg = (sent.messages ?? []).find(
    (m: { role: string; tool_call_id?: string }) => m.role === "tool" && m.tool_call_id === "call_1",
  );
  return { reply: result.reply, toolResult: JSON.parse(toolMsg.content) };
}

test("a run with no granted caps is offered no built-in tools", async () => {
  useLocal();
  params.set("prompt", "hi");
  dicode.caps = [];
  dicode.list_tasks = async () => [{ id: "other/something" }];

  http.mock("POST", "http://localhost:11434/v1/chat/completions", {
    status: 200,
    body: completion("ok"),
  });
  await runTask();

  const names = offeredTools();
  assert.ok(names.includes("task_other_something"), "task tools must still be offered");
  assert.ok(
    !names.some((n) => n.startsWith("dicode_")),
    `no built-in may be offered without a cap, got ${JSON.stringify(names)}`,
  );
});

test("each built-in is offered only when its own cap was granted", async () => {
  useLocal();
  params.set("prompt", "hi");
  dicode.caps = ["tasks.test", "sources.set_dev_mode"];

  http.mock("POST", "http://localhost:11434/v1/chat/completions", {
    status: 200,
    body: completion("ok"),
  });
  await runTask();

  const names = offeredTools();
  assert.ok(names.includes("dicode_test_task"), "tasks.test was granted");
  assert.ok(names.includes("dicode_set_dev_mode"), "sources.set_dev_mode was granted");
  assert.ok(!names.includes("dicode_list_sources"), "sources.list was not granted");
  assert.ok(!names.includes("dicode_git_commit_push"), "git.commit_push was not granted");
  assert.ok(!names.includes("dicode_replay_run"), "runs.replay was not granted");
});

test("dicode_list_sources reaches the SDK when sources.list is granted", async () => {
  useLocal();
  params.set("prompt", "which sources are there");
  dicode.caps = ["sources.list"];
  const listed = [{ name: "scratch", type: "taskset", dev_mode: false }];
  dicode.sources = { list: async () => listed };

  const { toolResult } = await runBuiltinCall("dicode_list_sources", {});

  assert.equal(toolResult, listed);
});

test("a built-in call reaches the SDK and its result feeds back to the model", async () => {
  useLocal();
  params.set("prompt", "test the task");
  dicode.caps = ["tasks.test"];
  const tested: string[] = [];
  dicode.tasks = {
    test: async (taskID: string) => {
      tested.push(taskID);
      return { passed: 2, failed: 0 };
    },
  };
  // run_task must not be reached: built-ins bypass task dispatch entirely.
  dicode.run_task = async () => {
    throw new Error("run_task must not be called for a built-in");
  };

  const { reply, toolResult } = await runBuiltinCall("dicode_test_task", { task_id: "scratch/demo" });

  assert.equal(reply, "done");
  assert.equal(tested, ["scratch/demo"]);
  assert.equal(toolResult, { passed: 2, failed: 0 });
});

test("set_dev_mode pins the clone to this run, ignoring any model-supplied id", async () => {
  // run_id names the clone directory. Letting the model choose it would let one
  // session reach another's clone, so the tool has no run_id argument at all.
  useLocal();
  params.set("prompt", "enter dev mode");
  dicode.caps = ["sources.set_dev_mode"];
  dicode.run_id = "run-abc";
  // deno-lint-ignore no-explicit-any
  const calls: any[] = [];
  dicode.sources = {
    // deno-lint-ignore no-explicit-any
    set_dev_mode: async (name: string, opts: any) => {
      calls.push({ name, opts });
      return { ok: true, clone_path: "/data/dev-clones/scratch/run-abc" };
    },
  };

  const { toolResult } = await runBuiltinCall("dicode_set_dev_mode", {
    source: "scratch",
    enabled: true,
    branch: "fix/thing",
    run_id: "run-somebody-else",
  });

  assert.equal(calls.length, 1);
  assert.equal(calls[0].name, "scratch");
  assert.equal(calls[0].opts.run_id, "run-abc");
  assert.equal(calls[0].opts.branch, "fix/thing");
  assert.equal(toolResult.clone_path, "/data/dev-clones/scratch/run-abc");
});

test("commit_push defaults the commit author instead of asking the model to invent one", async () => {
  useLocal();
  params.set("prompt", "push it");
  dicode.caps = ["git.commit_push"];
  dicode.task_id = "buildin/auto-fix";
  // deno-lint-ignore no-explicit-any
  const calls: any[] = [];
  dicode.git = {
    // deno-lint-ignore no-explicit-any
    commit_push: async (sourceID: string, opts: any) => {
      calls.push({ sourceID, opts });
      return { commit: "abc1234" };
    },
  };

  await runBuiltinCall("dicode_git_commit_push", {
    source_id: "scratch",
    message: "fix it",
    branch: "fix/thing",
  });

  assert.equal(calls[0].opts.author_name, "dicode buildin/auto-fix");
  assert.equal(calls[0].opts.author_email, "noreply@dicode.local");
});

test("commit_push takes its branch prefix from config, not from the model", async () => {
  // A prefix the model supplies alongside the branch it bounds constrains
  // nothing, so the prefix is a task param and never a tool argument.
  useLocal();
  params.set("prompt", "push it");
  params.set("git_branch_prefix", "fix/");
  dicode.caps = ["git.commit_push"];
  // deno-lint-ignore no-explicit-any
  const calls: any[] = [];
  dicode.git = {
    // deno-lint-ignore no-explicit-any
    commit_push: async (_id: string, opts: any) => {
      calls.push(opts);
      return { commit: "abc1234" };
    },
  };

  await runBuiltinCall("dicode_git_commit_push", {
    source_id: "scratch",
    message: "sneak",
    branch: "release/1.0",
    branch_prefix: "release/",
  });

  const sent = http.lastRequestBody("POST", "http://localhost:11434/v1/chat/completions");
  const tool = (sent.tools ?? []).find(
    (t: { function: { name: string } }) => t.function.name === "dicode_git_commit_push",
  );
  assert.equal(tool.function.parameters.properties.branch_prefix, undefined);
  assert.equal(calls[0].branch_prefix, "fix/");
});

test("a failing built-in returns an error result rather than killing the turn", async () => {
  useLocal();
  params.set("prompt", "test the task");
  dicode.caps = ["tasks.test"];
  dicode.tasks = {
    test: () => Promise.reject(new Error("task pending approval")),
  };

  const { reply, toolResult } = await runBuiltinCall("dicode_test_task", { task_id: "scratch/demo" });

  assert.equal(reply, "done");
  assert.equal(toolResult.error, "task pending approval");
});

test("set_dev_mode withholds local_path: the model cannot redirect taskset resolution", async () => {
  // local_path points the daemon's taskset resolution at an arbitrary path on
  // the host. Reachable from a tool argument, it would let a caller choose what
  // the daemon loads as tasks, so the tool offers clone mode only.
  useLocal();
  params.set("prompt", "enter dev mode");
  dicode.caps = ["sources.set_dev_mode"];
  // deno-lint-ignore no-explicit-any
  const calls: any[] = [];
  dicode.sources = {
    // deno-lint-ignore no-explicit-any
    set_dev_mode: async (name: string, opts: any) => {
      calls.push(opts);
      return { ok: true };
    },
  };

  const sentSchema = () => {
    const sent = http.lastRequestBody("POST", "http://localhost:11434/v1/chat/completions");
    const tool = (sent.tools ?? []).find(
      (t: { function: { name: string } }) => t.function.name === "dicode_set_dev_mode",
    );
    return tool.function.parameters.properties;
  };

  await runBuiltinCall("dicode_set_dev_mode", {
    source: "scratch",
    enabled: true,
    local_path: "/etc",
  });

  assert.equal(sentSchema().local_path, undefined);
  assert.equal(calls[0].local_path, undefined);
});

test("commit_push withholds allow_main: the model cannot waive branch protection", async () => {
  useLocal();
  params.set("prompt", "push it");
  dicode.caps = ["git.commit_push"];
  // deno-lint-ignore no-explicit-any
  const calls: any[] = [];
  dicode.git = {
    // deno-lint-ignore no-explicit-any
    commit_push: async (_id: string, opts: any) => {
      calls.push(opts);
      return { commit: "abc1234" };
    },
  };

  await runBuiltinCall("dicode_git_commit_push", {
    source_id: "scratch",
    message: "sneak",
    branch: "main",
    allow_main: true,
  });

  const sent = http.lastRequestBody("POST", "http://localhost:11434/v1/chat/completions");
  const tool = (sent.tools ?? []).find(
    (t: { function: { name: string } }) => t.function.name === "dicode_git_commit_push",
  );
  assert.equal(tool.function.parameters.properties.allow_main, undefined);
  assert.equal(calls[0].allow_main, undefined);
});
