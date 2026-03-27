test("posts digest when emails exist today", async () => {
  // OAuth2 token exchange
  http.mock("POST", "https://oauth2.googleapis.com/token", {
    status: 200,
    body: { access_token: "ya29.test-access-token" },
  });

  // Gmail list — returns 2 messages
  http.mockOnce("GET", "https://gmail.googleapis.com/*", {
    status: 200,
    body: { messages: [{ id: "msg1" }, { id: "msg2" }] },
  });

  // Message detail — msg1
  http.mockOnce("GET", "https://gmail.googleapis.com/*", {
    status: 200,
    body: {
      payload: {
        headers: [
          { name: "Subject", value: "Weekly report" },
          { name: "From", value: "boss@company.com" },
        ],
      },
    },
  });

  // Message detail — msg2
  http.mockOnce("GET", "https://gmail.googleapis.com/*", {
    status: 200,
    body: {
      payload: {
        headers: [
          { name: "Subject", value: "Lunch tomorrow?" },
          { name: "From", value: "friend@example.com" },
        ],
      },
    },
  });

  // Slack post
  http.mock("POST", "https://slack.com/api/chat.postMessage", {
    status: 200,
    body: { ok: true },
  });

  env.set("GMAIL_CLIENT_ID", "client-id");
  env.set("GMAIL_CLIENT_SECRET", "client-secret");
  env.set("GMAIL_REFRESH_TOKEN", "refresh-token");
  env.set("SLACK_BOT_TOKEN", "xoxb-test-token");
  params.set("slack_channel", "#daily-digest");
  params.set("max_emails", "20");

  const result = await runTask();

  assert.equal(result.count, 2);
  assert.equal(result.posted, true);
  assert.equal(result.channel, "#daily-digest");
  assert.httpCalled("POST", "https://slack.com/api/chat.postMessage");
  assert.httpCalledWith("POST", "https://slack.com/api/chat.postMessage", {
    body: { channel: "#daily-digest", mrkdwn: true },
  });
});

test("skips Slack post when inbox is empty today", async () => {
  http.mock("POST", "https://oauth2.googleapis.com/token", {
    status: 200,
    body: { access_token: "ya29.test-access-token" },
  });

  http.mock("GET", "https://gmail.googleapis.com/*", {
    status: 200,
    body: { messages: [] },
  });

  env.set("GMAIL_CLIENT_ID", "client-id");
  env.set("GMAIL_CLIENT_SECRET", "client-secret");
  env.set("GMAIL_REFRESH_TOKEN", "refresh-token");
  env.set("SLACK_BOT_TOKEN", "xoxb-test-token");
  params.set("slack_channel", "#daily-digest");
  params.set("max_emails", "20");

  const result = await runTask();

  assert.equal(result.count, 0);
  assert.equal(result.posted, false);
  assert.httpNotCalled("POST", "https://slack.com/api/chat.postMessage");
});

test("throws when OAuth2 token exchange fails", async () => {
  http.mock("POST", "https://oauth2.googleapis.com/token", {
    status: 401,
    body: { error: "invalid_client" },
  });

  env.set("GMAIL_CLIENT_ID", "bad-id");
  env.set("GMAIL_CLIENT_SECRET", "bad-secret");
  env.set("GMAIL_REFRESH_TOKEN", "bad-token");
  env.set("SLACK_BOT_TOKEN", "xoxb-test-token");
  params.set("slack_channel", "#general");
  params.set("max_emails", "20");

  assert.throws(() => runTask());
});

test("throws when Slack rejects the message", async () => {
  http.mock("POST", "https://oauth2.googleapis.com/token", {
    status: 200,
    body: { access_token: "ya29.test-access-token" },
  });

  http.mockOnce("GET", "https://gmail.googleapis.com/*", {
    status: 200,
    body: { messages: [{ id: "msg1" }] },
  });

  http.mockOnce("GET", "https://gmail.googleapis.com/*", {
    status: 200,
    body: {
      payload: {
        headers: [
          { name: "Subject", value: "Hello" },
          { name: "From", value: "someone@example.com" },
        ],
      },
    },
  });

  http.mock("POST", "https://slack.com/api/chat.postMessage", {
    status: 200,
    body: { ok: false, error: "channel_not_found" },
  });

  env.set("GMAIL_CLIENT_ID", "client-id");
  env.set("GMAIL_CLIENT_SECRET", "client-secret");
  env.set("GMAIL_REFRESH_TOKEN", "refresh-token");
  env.set("SLACK_BOT_TOKEN", "xoxb-test-token");
  params.set("slack_channel", "#nonexistent");
  params.set("max_emails", "20");

  assert.throws(() => runTask());
});
