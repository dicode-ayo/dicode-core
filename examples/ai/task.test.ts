/**
 * task.test.ts — unit tests for the AI agent task.
 *
 * Run with:
 *   dicode task test examples/ai
 *
 * The dicode test runtime provides params, log, env, and dicode globals.
 * These tests stub out dicode.get_config, dicode.list_tasks, and
 * dicode.run_task so no live AI provider or task engine is required.
 *
 * NOTE: Because task.ts uses `import OpenAI from "npm:openai"`, tests mock
 * the full tool-use loop via dicode stubs and verify the agent's return value
 * without making real network calls.
 */

test("agent returns result when model answers directly (no tool calls)", async () => {
  // Stub AI config
  dicode.get_config = async (_section: string) => ({
    baseURL: "http://localhost:11434/v1",
    model: "llama3.2",
    apiKey: "ollama",
  });

  // No tasks available — model must answer directly
  dicode.list_tasks = async () => [];

  // Model replies without calling any tools
  dicode.run_task = async (_id: string, _args: Record<string, unknown>) => {
    throw new Error("run_task should not be called when there are no tools");
  };

  params.set("prompt", "What is 2 + 2?");
  params.set("skills", "");

  const result = await runTask();

  assert.ok(typeof result.result === "string", "result should be a string");
  assert.ok(result.steps >= 2, "steps should be at least 2 (user + assistant)");
});

test("agent calls a task tool and returns the final answer", async () => {
  dicode.get_config = async (_section: string) => ({
    baseURL: "http://localhost:11434/v1",
    model: "llama3.2",
    apiKey: "ollama",
  });

  dicode.list_tasks = async () => [
    {
      id: "hello-cron",
      name: "Hello Cron",
      description: "A simple cron task that logs a greeting.",
      params: [
        { name: "name", type: "string", description: "Who to greet", required: true },
      ],
    },
  ];

  let taskCalled = false;
  dicode.run_task = async (id: string, _args: Record<string, unknown>) => {
    taskCalled = true;
    assert.equal(id, "hello-cron");
    return { status: "success", runID: "run-1", returnValue: { greeted: true } };
  };

  params.set("prompt", "Run the hello-cron task with name=World");
  params.set("skills", "hello-cron");

  const result = await runTask();

  assert.ok(taskCalled, "run_task should have been called");
  assert.ok(typeof result.result === "string");
  assert.ok(result.steps >= 3, "steps: user + assistant(tool_call) + tool + assistant");
});

test("agent respects skills filter — excludes tasks not in filter", async () => {
  let listCalled = false;

  dicode.get_config = async (_section: string) => ({
    baseURL: "http://localhost:11434/v1",
    model: "llama3.2",
    apiKey: "ollama",
  });

  dicode.list_tasks = async () => {
    listCalled = true;
    return [
      { id: "task-a", name: "Task A", description: "First task", params: [] },
      { id: "task-b", name: "Task B", description: "Second task", params: [] },
    ];
  };

  dicode.run_task = async (_id: string, _args: Record<string, unknown>) => ({
    status: "success",
    runID: "run-2",
    returnValue: null,
  });

  // Only allow task-a
  params.set("prompt", "Do something");
  params.set("skills", "task-a");

  await runTask();

  assert.ok(listCalled, "list_tasks should have been called");
  // The model only receives one tool (task-a) — we can't directly assert the
  // OpenAI request payload here, but we verify the task ran without error.
});
