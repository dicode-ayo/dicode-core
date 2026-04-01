/**
 * task.test.ts — unit tests for the failure-monitor task.
 *
 * Run with:
 *   dicode task test examples/failure-monitor
 *
 * Each test() block runs in its own isolated runtime. Globals
 * (input, dicode, http, …) are reset between tests automatically.
 */

test("diagnoses failure and returns diagnosis", async () => {
  input.set({ taskID: "my-task", runID: "run-abc", status: "failure", output: null })

  dicode.mock("get_runs", [
    {
      ID: "run-abc",
      Logs: [
        { Level: "error", Message: "connection refused: db unreachable" },
        { Level: "info",  Message: "task started" },
      ],
    },
  ])
  dicode.mock("get_config", {
    baseURL: "https://api.openai.com/v1",
    model: "gpt-4o-mini",
    apiKey: "sk-test",
  })

  http.mock("POST", "https://api.openai.com/v1/chat/completions", {
    status: 200,
    body: {
      choices: [{ message: { content: "The database is unreachable. Check that the DB service is running and the connection string is correct." } }],
    },
  })

  const result = await runTask()

  assert.equal(result.taskID, "my-task")
  assert.equal(result.runID, "run-abc")
  assert.ok(result.diagnosis)
  assert.httpCalled("POST", "https://api.openai.com/v1/chat/completions")
})

test("returns error when taskID is missing", async () => {
  input.set({ taskID: "", runID: "run-xyz", status: "failure" })

  const result = await runTask()

  assert.equal(result.error, "missing input")
})

test("returns error when runID is missing", async () => {
  input.set({ taskID: "my-task", runID: "", status: "failure" })

  const result = await runTask()

  assert.equal(result.error, "missing input")
})

test("handles run with no logs gracefully", async () => {
  input.set({ taskID: "silent-task", runID: "run-empty", status: "failure" })

  dicode.mock("get_runs", [
    { ID: "run-empty", Logs: [] },
  ])
  dicode.mock("get_config", {
    baseURL: "https://api.openai.com/v1",
    model: "gpt-4o-mini",
    apiKey: "sk-test",
  })

  http.mock("POST", "https://api.openai.com/v1/chat/completions", {
    status: 200,
    body: {
      choices: [{ message: { content: "No logs available — check the task configuration." } }],
    },
  })

  const result = await runTask()

  assert.equal(result.taskID, "silent-task")
  assert.ok(result.diagnosis)
  assert.httpCalledWith("POST", "https://api.openai.com/v1/chat/completions", {
    body: { messages: [{ role: "user", content: /no logs available/i }] },
  })
})

test("falls back to first run when runID does not match", async () => {
  input.set({ taskID: "other-task", runID: "run-missing", status: "failure" })

  dicode.mock("get_runs", [
    {
      ID: "run-latest",
      Logs: [{ Level: "error", Message: "timeout exceeded" }],
    },
  ])
  dicode.mock("get_config", {
    baseURL: "https://api.openai.com/v1",
    model: "gpt-4o-mini",
    apiKey: "sk-test",
  })

  http.mock("POST", "https://api.openai.com/v1/chat/completions", {
    status: 200,
    body: {
      choices: [{ message: { content: "The task exceeded its time limit." } }],
    },
  })

  const result = await runTask()

  assert.ok(result.diagnosis)
  assert.httpCalled("POST", "https://api.openai.com/v1/chat/completions")
})
