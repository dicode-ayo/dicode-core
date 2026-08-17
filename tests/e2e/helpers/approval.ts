import type { APIRequestContext } from '@playwright/test';

/**
 * settleApproved returns a task to "current on-disk content, approved" after a
 * test that mutated it — the state every spec sharing a fixture assumes it
 * starts from.
 *
 * Restoring the original bytes is not enough on its own. dicode.lock holds one
 * hash per task, so a test that clicked Approve recorded the mutated content's
 * hash, and the restore re-pends the task — but not instantly, because the
 * reconciler has to observe the write first. Polling for "not pending" and
 * returning on the first hit therefore reports success while the re-pend is
 * still in flight, stranding the fixture for every later test and spec file.
 *
 * So this requires the task to be observed armed across several consecutive
 * polls spanning more than one reconcile, approving whatever is pending in
 * between. It tolerates the caller never having approved anything, in which
 * case no re-pend arrives and the consecutive-clean check passes immediately.
 *
 * Throws on timeout: a fixture left pending turns one broken test into a
 * cascade of unrelated-looking failures elsewhere, and that is far harder to
 * diagnose than a loud failure here.
 */
export async function settleApproved(
  request: APIRequestContext,
  taskID: string,
  { timeoutMs = 90_000, quietPolls = 8, pollMs = 2_000 } = {},
): Promise<void> {
  const encoded = encodeURIComponent(taskID);
  const deadline = Date.now() + timeoutMs;
  let clean = 0;
  while (Date.now() < deadline) {
    const res = await request.get(`/api/tasks/${encoded}`);
    if (res.ok()) {
      const t = await res.json() as Record<string, unknown>;
      if (t.pending_approval === true) {
        clean = 0;
        await request.post(`/api/tasks/${encoded}/approve`);
      } else if (++clean >= quietPolls) {
        return;
      }
    } else {
      clean = 0;
    }
    await new Promise((r) => setTimeout(r, pollMs));
  }
  throw new Error(`${taskID} did not settle to approved within ${timeoutMs}ms`);
}
