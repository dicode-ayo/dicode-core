// retry.ts — handling for a failed Telegram send: whether to try again, when,
// and what is safe to say about it.
//
// Pure: no fetch, no timers. A notification that dies on a transient network
// blip is lost silently, since the daemon logs a failed notify at Debug.

export const MAX_ATTEMPTS = 3;

const BASE_DELAY_MS = 500;
const MAX_DELAY_MS = 8000;

/** Retryable: rate limits and server-side faults. A 4xx other than 429 is a
 *  rejected message — a bad chat_id or malformed HTML — and retrying it only
 *  repeats the same rejection. */
export function isRetryableStatus(status: number): boolean {
  return status === 429 || status >= 500;
}

/** Delay before the attempt following `attempt` (1-based), or null when there
 *  is no delay worth waiting. Telegram's 429 body carries the server's own wait
 *  hint; retrying earlier than it asked spends an attempt on a certain second
 *  429, so a hint longer than the task can absorb means stop rather than
 *  retry early. */
export function backoffDelayMs(attempt: number, retryAfterSec?: number): number | null {
  if (retryAfterSec && retryAfterSec > 0) {
    const wait = retryAfterSec * 1000;
    return wait > MAX_DELAY_MS ? null : wait;
  }
  return Math.min(BASE_DELAY_MS * 2 ** (attempt - 1), MAX_DELAY_MS);
}

/** The request URL embeds the bot token and fetch puts the URL into its own
 *  error messages. The runtime's stream redactor covers stdout and stderr, but
 *  the thrown message travels further than the log does — into the run's error
 *  and on to any chaining caller. */
export function stripToken(message: string, token: string): string {
  return token ? message.split(token).join("<token>") : message;
}
