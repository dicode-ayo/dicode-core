/**
 * Runs `body`, then always runs `cleanup`, even if `body` threw. A cleanup
 * failure is preserved rather than swallowed: it fails the caller unless
 * `body` itself already failed, in which case body's error (the more
 * informative one — the actual assertion/behavior failure) is what gets
 * thrown. A concurrent cleanup failure in that case is logged rather than
 * dropped entirely, so it isn't lost without a trace.
 *
 * Failure is tracked via explicit flags rather than truthiness of the
 * caught value, so a `throw undefined` / `throw null` / rejection-with-no-
 * reason still counts as a failure.
 */
export async function withGuaranteedCleanup<T>(
  body: () => Promise<T>,
  cleanup: () => Promise<void>,
): Promise<T> {
  let bodyFailed = false;
  let bodyError: unknown;
  let result: T | undefined;
  try {
    result = await body();
  } catch (err) {
    bodyFailed = true;
    bodyError = err;
  }

  let cleanupFailed = false;
  let cleanupError: unknown;
  try {
    await cleanup();
  } catch (err) {
    cleanupFailed = true;
    cleanupError = err;
  }

  if (bodyFailed) {
    if (cleanupFailed) {
      console.error('withGuaranteedCleanup: cleanup also failed:', cleanupError);
    }
    throw bodyError;
  }
  if (cleanupFailed) throw cleanupError;
  return result as T;
}
