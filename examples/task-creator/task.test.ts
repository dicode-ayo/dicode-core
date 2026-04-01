/**
 * task.test.ts — unit tests for the task-creator example.
 *
 * Run with:
 *   dicode task test examples/task-creator
 *
 * The tests mock the OpenAI client via npm:openai so no real AI call is made.
 * Each test() block runs in its own isolated runtime; globals are reset
 * automatically between tests.
 */

test("throws when description param is missing", async () => {
  params.set("task_id", "my-task");
  // description intentionally not set

  await assert.throws(
    () => runTask(),
    /description param is required/,
  );
});

test("throws when task_id param is missing", async () => {
  params.set("description", "send a daily digest email");
  // task_id intentionally not set

  await assert.throws(
    () => runTask(),
    /task_id param is required/,
  );
});

test("throws when task_id is not a valid slug", async () => {
  params.set("description", "do something");
  params.set("task_id", "My Task!");

  await assert.throws(
    () => runTask(),
    /task_id must be a lowercase slug/,
  );
});
