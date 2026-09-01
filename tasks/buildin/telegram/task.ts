// Delivers a dicode notification to a Telegram chat via the Bot API.
//
// Message composition for every notification shape lives in render.ts; this
// file is the send and its retry policy. See README.md for the field contract.

import type { DicodeSdk } from "../../sdk.ts";
import { collectFields, renderMessage } from "./render.ts";
import { MAX_ATTEMPTS, backoffDelayMs, isRetryableStatus, stripToken } from "./retry.ts";

const API_BASE = "https://api.telegram.org";

/** Per-attempt budget. Without it a single hung connection consumes the whole
 *  task timeout and the remaining attempts never run. */
const ATTEMPT_TIMEOUT_MS = 8000;

interface SendResponse {
  ok?: boolean;
  description?: string;
  result?: { message_id?: number };
  parameters?: { retry_after?: number };
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export default async function main({ params, input }: DicodeSdk) {
  const all = await params.all();
  const fields = collectFields(all, input);
  const { text, silent } = renderMessage(fields);

  const token = (Deno.env.get("TELEGRAM_BOT_TOKEN") ?? "").trim();
  if (!token) {
    throw new Error(
      "TELEGRAM_BOT_TOKEN is not set — create a bot with @BotFather, then " +
      "`dicode secrets set TELEGRAM_BOT_TOKEN <token>`",
    );
  }

  const chatID = (all["chat_id"] ?? "").trim() || (Deno.env.get("TELEGRAM_CHAT_ID") ?? "").trim();
  if (!chatID) {
    throw new Error(
      "no target chat — pass chat_id=<id> or `dicode secrets set TELEGRAM_CHAT_ID <id>`",
    );
  }

  const body = JSON.stringify({
    chat_id: chatID,
    text,
    parse_mode: "HTML",
    disable_web_page_preview: true,
    disable_notification: silent,
  });

  let payload: SendResponse | null = null;
  let lastError = "";
  let sent = false;

  for (let attempt = 1; attempt <= MAX_ATTEMPTS; attempt++) {
    let res: Response;
    try {
      res = await fetch(`${API_BASE}/bot${token}/sendMessage`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body,
        signal: AbortSignal.timeout(ATTEMPT_TIMEOUT_MS),
      });
    } catch (e) {
      lastError = stripToken(e instanceof Error ? e.message : String(e), token);
      if (attempt === MAX_ATTEMPTS) break;
      await sleep(backoffDelayMs(attempt));
      continue;
    }

    // Telegram answers 200 with {"ok":false,"description":...} for several
    // errors, so the body decides, not the status.
    payload = await res.json().catch(() => null) as SendResponse | null;
    if (res.ok && payload?.ok) {
      sent = true;
      break;
    }

    lastError = stripToken(
      payload?.description?.trim() ||
        `HTTP ${res.status} with no usable Telegram response body`,
      token,
    );
    if (!isRetryableStatus(res.status) || attempt === MAX_ATTEMPTS) break;
    console.log(`telegram send attempt ${attempt} failed, retrying: ${lastError}`);
    await sleep(backoffDelayMs(attempt, payload?.parameters?.retry_after));
  }

  if (!sent) {
    throw new Error(`telegram sendMessage failed: ${lastError}`);
  }

  return {
    ok: true,
    chat_id: chatID,
    message_id: payload?.result?.message_id ?? null,
    silent,
    task_id: fields.task_id,
    event: fields.event,
  };
}
