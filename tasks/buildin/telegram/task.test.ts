/**
 * task.test.ts — unit tests for the telegram notification task.
 *
 * Run with:
 *   deno test --allow-all --config=tasks/deno.json tasks/buildin/telegram/task.test.ts
 * or:
 *   make test-tasks
 *
 * Covers rendering for each notification source, the send path against a
 * mocked Bot API, and the retry loop.
 */
import { setupHarness } from "../../sdk-test.ts";
await setupHarness(import.meta.url);

import { collectFields, escapeHtml, MAX_TEXT, renderMessage } from "./render.ts";
import { backoffDelayMs, stripToken } from "./retry.ts";

const SEND = "https://api.telegram.org/bot*/sendMessage";

function render(params: Record<string, string>, input?: unknown) {
  return renderMessage(collectFields(params, input));
}

// ─── rendering ───────────────────────────────────────────────────────────

test("suspend hook: keeps the rendered title and body", () => {
  const { text, silent } = render({
    title: "dicode: an agent needs your reply",
    body: "Task buildin/ai-agent is paused for your input. Resume: http://localhost:8080/?run=r1",
    priority: "default",
    event: "suspended",
    run_id: "r1",
    task_id: "buildin/ai-agent",
    resume_url: "http://localhost:8080/?run=r1",
  });

  assert.ok(text.includes("<b>dicode: an agent needs your reply</b>"));
  assert.ok(text.includes("Task: <code>buildin/ai-agent</code>"));
  assert.ok(text.includes("Run: <code>r1</code>"));
  assert.equal(silent, false);
  // The body already carries the resume link — it is not repeated as a detail line.
  assert.equal(text.split("http://localhost:8080/?run=r1").length - 1, 1);
});

test("ended event: low priority is delivered silently", () => {
  const { text, silent } = render({
    title: "dicode: conversation ended",
    body: "Task buildin/ai-agent finished (success).",
    priority: "low",
    event: "ended",
    run_id: "r2",
    task_id: "buildin/ai-agent",
    status: "success",
  });

  assert.ok(text.includes("<b>dicode: conversation ended</b>"));
  assert.ok(text.includes("Status: success"));
  assert.equal(silent, true);
});

test("approval hook: composes a message from task_id/hash/approve_url alone", () => {
  const { text } = render({
    task_id: "buildin/ai-agent-claude-cli",
    hash: "abc123",
    approve_url: "http://localhost:8080/approve?t=tok",
  });

  assert.ok(text.includes("<b>dicode: task pending approval</b>"), text);
  assert.ok(text.includes("buildin/ai-agent-claude-cli"), text);
  assert.ok(text.includes("Content: <code>abc123</code>"), text);
  assert.ok(text.includes("Approve: http://localhost:8080/approve?t=tok"), text);
});

test("approval hook: an empty approve_url still yields a usable message", () => {
  const { text } = render({ task_id: "buildin/x", hash: "h1", approve_url: "" });
  assert.ok(text.includes("pending approval"), text);
  assert.equal(text.includes("Approve:"), false);
});

test("failure chain: reads the engine-stamped input, not params", () => {
  const { text } = render({}, {
    taskID: "examples/github-stars",
    runID: "run-77",
    status: "failure",
    output: { error: "boom" },
    _chain_depth: 1,
  });

  assert.ok(text.includes("<b>dicode: task failed</b>"), text);
  assert.ok(text.includes("Task: <code>examples/github-stars</code>"), text);
  assert.ok(text.includes("Run: <code>run-77</code>"), text);
  assert.ok(text.includes("Status: failure"), text);
  assert.ok(text.includes("boom"), text);
});

test("params win over input on the same field", () => {
  const f = collectFields({ task_id: "from/params" }, { taskID: "from/input", runID: "r9" });
  assert.equal(f.task_id, "from/params");
  assert.equal(f.run_id, "r9");
});

test("escapes the three HTML-reserved characters", () => {
  assert.equal(escapeHtml(`a & b < c > d`), "a &amp; b &lt; c &gt; d");

  const { text } = render({
    title: "<b>raw</b> & more",
    body: "value <script>x</script> & y",
    task_id: "ns/a&b<c>",
  });
  assert.ok(text.includes("&lt;b&gt;raw&lt;/b&gt; &amp; more"), text);
  assert.ok(text.includes("&lt;script&gt;x&lt;/script&gt; &amp; y"), text);
  assert.ok(text.includes("<code>ns/a&amp;b&lt;c&gt;</code>"), text);
  // Only the tags the renderer itself emits survive.
  assert.equal(text.includes("<script>"), false);
});

test("never exceeds the Telegram length cap", () => {
  // title and body are pre-cut by TITLE_MAX/BODY_MAX, so they cannot reach the
  // cap on their own. resume_url and task_id have no cut() in front of them —
  // they are what pushes a real message over.
  const { text } = render({
    title: "t".repeat(500),
    body: "b".repeat(9000),
    task_id: "ns/" + "t".repeat(3000),
    resume_url: "http://localhost:8080/?run=" + "r".repeat(3000),
  });
  assert.ok(text.length <= MAX_TEXT, `text was ${text.length}`);
  assert.ok(text.includes("<b>"), "the headline survives truncation");
});

test("length cap holds when a kept line lands flush against the boundary", () => {
  // The overflow window is two characters wide: it needs a line to fill the
  // budget exactly, so the following break still appends a separator and the
  // "…". Sweeping the whole window rather than one length, since the exact
  // offset moves whenever a detail line's format changes.
  for (let n = MAX_TEXT - 60; n <= MAX_TEXT - 20; n++) {
    const { text } = render({
      title: "t",
      task_id: "x".repeat(n),
      run_id: "y".repeat(100),
    });
    assert.ok(text.length <= MAX_TEXT, `task_id ${n}: text was ${text.length}`);
  }
});

test("no fields at all: still renders something sendable", () => {
  const { text } = render({});
  assert.equal(text, "<b>dicode notification</b>");
});

// ─── send path ───────────────────────────────────────────────────────────

test("posts HTML-parsed text to sendMessage", async () => {
  http.mock("POST", SEND, { status: 200, body: { ok: true, result: { message_id: 42 } } });
  env.set("TELEGRAM_BOT_TOKEN", "123:ABC");
  params.set("chat_id", "-100987");
  params.set("title", "hello");
  params.set("body", "world");

  const res = await runTask();
  assert.equal(res.ok, true);
  assert.equal(res.message_id, 42);

  const sent = http.lastRequestBody("POST", SEND);
  assert.equal(sent.chat_id, "-100987");
  assert.equal(sent.parse_mode, "HTML");
  assert.equal(sent.disable_notification, false);
  assert.ok(sent.text.includes("<b>hello</b>"));
  assert.httpCalled("POST", "https://api.telegram.org/bot123:ABC/sendMessage");
});

test("falls back to the TELEGRAM_CHAT_ID secret when chat_id is empty", async () => {
  http.mock("POST", SEND, { status: 200, body: { ok: true, result: { message_id: 1 } } });
  env.set("TELEGRAM_BOT_TOKEN", "123:ABC");
  env.set("TELEGRAM_CHAT_ID", "-100555");
  params.set("title", "hello");

  const res = await runTask();
  assert.equal(res.chat_id, "-100555");
});

test("failure-chain input reaches the send path", async () => {
  http.mock("POST", SEND, { status: 200, body: { ok: true, result: { message_id: 7 } } });
  env.set("TELEGRAM_BOT_TOKEN", "123:ABC");
  env.set("TELEGRAM_CHAT_ID", "-100555");
  globalThis.input = { taskID: "ns/failing", runID: "r3", status: "failure", output: null };

  const res = await runTask();
  assert.equal(res.task_id, "ns/failing");
  assert.ok(http.lastRequestBody("POST", SEND).text.includes("ns/failing"));
});

test("throws on a 200 that carries ok:false", async () => {
  http.mock("POST", SEND, {
    status: 200,
    body: { ok: false, error_code: 400, description: "Bad Request: chat not found" },
  });
  env.set("TELEGRAM_BOT_TOKEN", "123:ABC");
  params.set("chat_id", "-1");
  params.set("title", "hello");

  await assert.throws(() => runTask(), /chat not found/);
});

test("surfaces Telegram's own description on a non-2xx", async () => {
  http.mock("POST", SEND, { status: 401, body: { ok: false, description: "Unauthorized" } });
  env.set("TELEGRAM_BOT_TOKEN", "123:ABC");
  params.set("chat_id", "-1");

  await assert.throws(() => runTask(), /Unauthorized/);
});

test("a connection-level failure is reported without the bot token", async () => {
  // No mock for the send URL, so fetch fails with an error carrying the full
  // request URL — the path the token actually leaks from. A mocked
  // `description` never contained it, so it cannot exercise this.
  env.set("TELEGRAM_BOT_TOKEN", "123:SECRET");
  params.set("chat_id", "-1");
  params.set("title", "hello");

  let message = "";
  try {
    await runTask();
  } catch (e) {
    message = (e as Error).message;
  }
  assert.equal(message.includes("SECRET"), false, message);
  assert.ok(message.includes("<token>"), message);
});

test("throws with a set-the-secret hint when the bot token is missing", async () => {
  params.set("chat_id", "-1");
  await assert.throws(() => runTask(), /TELEGRAM_BOT_TOKEN/);
  assert.httpNotCalled("POST", SEND);
});

test("throws when no chat is configured", async () => {
  env.set("TELEGRAM_BOT_TOKEN", "123:ABC");
  await assert.throws(() => runTask(), /chat_id/);
  assert.httpNotCalled("POST", SEND);
});

// ─── retry policy ────────────────────────────────────────────────────────

test("retries a 429 and succeeds on the next attempt", async () => {
  http.mockOnce("POST", SEND, {
    status: 429,
    body: { ok: false, description: "Too Many Requests", parameters: { retry_after: 0 } },
  });
  http.mock("POST", SEND, { status: 200, body: { ok: true, result: { message_id: 9 } } });
  env.set("TELEGRAM_BOT_TOKEN", "123:ABC");
  params.set("chat_id", "-1");
  params.set("title", "hello");

  const res = await runTask();
  assert.equal(res.message_id, 9);
});

test("does not retry a 400 — the follow-up mock is never reached", async () => {
  http.mockOnce("POST", SEND, {
    status: 400,
    body: { ok: false, description: "Bad Request: chat not found" },
  });
  http.mock("POST", SEND, { status: 200, body: { ok: true, result: { message_id: 1 } } });
  env.set("TELEGRAM_BOT_TOKEN", "123:ABC");
  params.set("chat_id", "-1");
  params.set("title", "hello");

  await assert.throws(() => runTask(), /chat not found/);
});

test("backoff honors Telegram's retry_after over the curve, still capped", () => {
  assert.equal(backoffDelayMs(1), 500);
  assert.equal(backoffDelayMs(1, 3), 3000);
  assert.equal(backoffDelayMs(1, 600), 8000);
});

test("stripToken keeps the bot token out of error text", () => {
  const token = "123456:AAsecretvalue";
  const raw = `error sending request for url (https://api.telegram.org/bot${token}/sendMessage)`;
  const safe = stripToken(raw, token);
  assert.equal(safe.includes(token), false);
  assert.ok(safe.includes("<token>"), safe);
  assert.equal(stripToken("plain failure", ""), "plain failure");
});
