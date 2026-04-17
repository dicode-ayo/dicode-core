# Follow-up: relay identity rotation CLI

**Status:** work complete on branch `feat/oauth-relay-rotation`, deferred
from the main OAuth broker PR (`feat/oauth-relay-client`) so the main PR
stays focused on the broker flow itself.

**Open this as a GitHub issue and link the follow-up branch.**

## Background

The daemon's relay identity is a long-lived ECDSA P-256 keypair stored in
the local SQLite KV at `relay.private_key`. The derived UUID
(`hex(sha256(uncompressed_pubkey))`) becomes the stable prefix for every
`https://relay.dicode.app/u/<uuid>/hooks/...` URL the user has ever
shared, and secures both the WSS handshake and the signed
`/auth/:provider` broker requests.

Up to this PR there is no way to rotate that key. If a user suspects
compromise — e.g. laptop stolen, private key leaked by a misconfigured
backup — the only path is to delete the KV row manually, which silently
invalidates every shared webhook URL without operator awareness.

## What the follow-up branch contains

Branch: [`feat/oauth-relay-rotation`](../../../../tree/feat/oauth-relay-rotation)

- `pkg/relay/keys.go` — public `RotateIdentity(ctx, db)` function that
  delegates to the existing `generateAndStore` path (which already uses
  `INSERT OR REPLACE`).
- `pkg/relay/keys_test.go` — `TestRotateIdentity` verifying the UUID
  changes and the new key persists across reloads.
- `pkg/relay/oauth_pending.go` — new `Clear()` method on
  `PendingSessions`. Rotation flushes in-flight flows because any
  arriving delivery was encrypted to the now-retired public key.
- `pkg/ipc/control.go` — new `RelayIdentityRotator` callback type,
  new `SetRelayIdentityRotator` setter on `ControlServer`, new
  `RelayRotateResult` response struct, new `handleRelayRotate` handler,
  and a dispatch case for `cli.relay.rotate_identity`.
- `pkg/ipc/control_test.go` — `TestControl_RelayRotate_NotEnabled` and
  `TestControl_RelayRotate_InvokesRotator`.
- `pkg/daemon/daemon.go` — inside the `if cfg.Relay.Enabled` block,
  wires a closure that calls `relay.RotateIdentity` and
  `PendingSessions.Clear` on demand.
- `cmd/dicode/main.go` — new `relay rotate-identity` subcommand behind
  a `--yes` confirmation gate. Updates the `usage()` help text.

All tests on the branch pass.

## Known limitation

The running relay WSS client keeps using the old in-memory
`*relay.Identity` pointer until the daemon restarts. The callback
updates the database and invalidates pending OAuth flows, but it does
not atomically swap the in-memory identity the already-dialed WSS
connection holds.

The CLI surfaces this as a warning in `RelayRotateResult.Warning`
("Restart the daemon to activate the new identity on the WSS relay
connection"), which is honest but suboptimal. A proper fix requires:

1. A mutex on `relay.Client.identity` (currently an unprotected
   pointer field).
2. A new `Client.ReplaceIdentity(*Identity)` method that swaps the
   pointer under the mutex and cancels the current `runOnce` loop so
   the reconnect happens under the new identity.
3. The relay server's in-memory registry will see the old UUID go away
   (connection dropped) and the new UUID come up on the reconnect.
4. Any pending HTTP requests waiting on a `forward(oldUUID, ...)` call
   should be failed fast rather than left hanging.

That's a ~50-line change in `pkg/relay/client.go` plus a concurrency
test. It was left out of the follow-up branch to keep the rotation work
reviewable on its own.

## What needs to happen to merge

1. Open a GitHub issue pointing at this doc and the
   `feat/oauth-relay-rotation` branch.
2. Decide whether to ship the "documented caveat + restart required"
   version first (small) or block on the hot-swap fix (larger).
3. If shipping the caveat version: review the branch as-is, merge.
4. If blocking on hot-swap: add the `Client.ReplaceIdentity` plumbing
   on the same branch before review.

## Threat-model recap

The reviewer who flagged this gap (see the round-1 design review
discussion) specifically said rotation is *out of scope for the broker
PR but must exist*. The concern is that exposing any key-touching
primitive to task code — even the narrow `dicode.oauth.build_auth_url`
and `dicode.oauth.store_token` primitives that ship in the main PR —
makes the non-rotatable nature of the identity more load-bearing than
it was before. Adding rotation closes the blast radius: if a leak is
suspected, `dicode relay rotate-identity --yes` is the recovery path.
