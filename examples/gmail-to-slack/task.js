// Build today's date range for Gmail search query
const now = new Date();
const pad = n => String(n).padStart(2, "0");
const dateStr = `${now.getFullYear()}/${pad(now.getMonth() + 1)}/${pad(now.getDate())}`;
const tomorrow = new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1);
const tomorrowStr = `${tomorrow.getFullYear()}/${pad(tomorrow.getMonth() + 1)}/${pad(tomorrow.getDate())}`;

// Exchange refresh token for a short-lived access token
const tokenRes = await http.post("https://oauth2.googleapis.com/token", {
  body: {
    client_id: env.GMAIL_CLIENT_ID,
    client_secret: env.GMAIL_CLIENT_SECRET,
    refresh_token: env.GMAIL_REFRESH_TOKEN,
    grant_type: "refresh_token",
  },
});
if (tokenRes.status !== 200) {
  log.error("OAuth2 token exchange failed", { status: tokenRes.status });
  throw new Error(`OAuth2 error: ${tokenRes.status}`);
}
const accessToken = tokenRes.body.access_token;
log.info("Obtained Gmail access token");

// Fetch today's message list
const maxEmails = Math.min(parseInt(params.max_emails) || 20, 50);
const channel = params.slack_channel || "#general";
const query = encodeURIComponent(`after:${dateStr} before:${tomorrowStr}`);
const listUrl = `https://gmail.googleapis.com/gmail/v1/users/me/messages?q=${query}&maxResults=${maxEmails}`;

const listRes = await http.get(listUrl, {
  headers: { Authorization: `Bearer ${accessToken}` },
});
if (listRes.status !== 200) {
  log.error("Gmail list messages failed", { status: listRes.status });
  throw new Error(`Gmail API error: ${listRes.status}`);
}

const messages = listRes.body.messages || [];
log.info(`Found ${messages.length} email(s) today`);

if (messages.length === 0) {
  log.info("Inbox quiet today — skipping Slack notification");
  return { count: 0, posted: false };
}

// Fetch subject and sender for each message via metadata endpoint
const emails = [];
for (const msg of messages) {
  const detailUrl = `https://gmail.googleapis.com/gmail/v1/users/me/messages/${msg.id}?format=metadata&metadataHeaders=Subject&metadataHeaders=From`;
  const detailRes = await http.get(detailUrl, {
    headers: { Authorization: `Bearer ${accessToken}` },
  });
  if (detailRes.status !== 200) {
    log.warn("Failed to fetch message metadata", { id: msg.id, status: detailRes.status });
    continue;
  }
  const hdrs = detailRes.body.payload?.headers || [];
  const subject = hdrs.find(h => h.name === "Subject")?.value || "(no subject)";
  const from = hdrs.find(h => h.name === "From")?.value || "Unknown sender";
  emails.push({ subject, from });
}

// Format Slack message with simple date label (locale-safe)
const monthNames = ["January", "February", "March", "April", "May", "June",
  "July", "August", "September", "October", "November", "December"];
const dayNames = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];
const dateLabel = `${dayNames[now.getDay()]}, ${monthNames[now.getMonth()]} ${now.getDate()}, ${now.getFullYear()}`;

const lines = emails.map((e, i) => `${i + 1}. *${e.subject}*\n   _${e.from}_`);
const slackText = [
  `📬 *Gmail digest — ${dateLabel}*`,
  `${emails.length} email(s) received today`,
  "",
  lines.join("\n\n"),
].join("\n");

const slackRes = await http.post("https://slack.com/api/chat.postMessage", {
  headers: {
    Authorization: `Bearer ${env.SLACK_BOT_TOKEN}`,
    "Content-Type": "application/json",
  },
  body: { channel, text: slackText, mrkdwn: true },
});
if (!slackRes.body?.ok) {
  log.error("Slack post failed", { error: slackRes.body?.error });
  throw new Error(`Slack error: ${slackRes.body?.error}`);
}

log.info("Digest posted to Slack", { channel, count: emails.length });
return { count: emails.length, posted: true, channel };
