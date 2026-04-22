# End-to-end tests

Two independent e2e suites live in this repo:

| Suite | Stack | Runs via |
|---|---|---|
| Browser-level flows (webhook delivery, UI, auth modes) | Playwright + TypeScript | `make test-e2e` (and friends) |
| Cross-service daemon ↔ relay integration | Go + [testcontainers-go](https://golang.testcontainers.org/) | `make test-e2e-relay` |

This document covers the **daemon ↔ relay** suite. See [`tests/e2e/README.md`](../../tests/e2e/README.md) for the Playwright suite.

## What it covers

Spawns a real `dicode-relay` container (from the published image) and drives it against an in-process `pkg/relay.Client` + `pkg/ipc.Server` to catch cross-service drift that unit tests on either side cannot. The matrix lives in [tests/e2e/relay/relay_test.go](../../tests/e2e/relay/relay_test.go).

## Coverage matrix (#137)

Every scenario from the [#137](https://github.com/dicode-ayo/dicode-core/issues/137) matrix is covered, but not all of them live in this testcontainers suite. Some are faster and stronger as in-package unit tests; one is guarded upstream by dicode-relay's own Vitest. The split is deliberate — testcontainers earns its cost when the invariant only shows up under real cross-implementation wire interaction, and doesn't when the invariant is purely local to the daemon.

| # | Scenario | Where it lives |
|---|---|---|
| 1 | Handshake + `protocol: 2` + HTTP forward round-trip | `tests/e2e/relay/relay_test.go: TestHappyPath_HandshakeAndForward` |
| 2 | OAuth happy path (`BuildAuthURL` → `/connect/mock` → ECIES delivery) | `tests/e2e/relay/relay_test.go: TestOAuthHappyPath_MockProvider` |
| 3 | Split-key regression — broker encrypts to SignKey, daemon must fail loudly | `pkg/relay/oauth_split_test.go: TestOAuth_UsesDecryptKey` (unit; faster and more targeted than a containerised forgery — exercises the #104 invariant directly) |
| 4 | Protocol version guard — old broker → daemon refuses with "upgrade" | `pkg/relay/broker_protocol_test.go: TestClient_RefusesConnection_WhenBrokerProtocolOld` (unit; no need to publish a legacy `dicodeayo/dicode-relay:v0.0.x` image tag) |
| 5 | Reconnect survives broker bounce | Intentionally **not shipped** — see the inline note in `tests/e2e/relay/tofu_test.go`. Testcontainers `Stop` + `Start` is too flaky against the daemon's exponential backoff inside a reasonable test timeout, and the invariant the scenario guarded (`PinMatch` branch doesn't overwrite) is a tautology today (zero DB writes on PinMatch). Reintroduce via a non-backoff-dependent harness if future work adds writes to PinMatch |
| 6 | Replay protection — broker rejects a duplicate hello nonce | dicode-relay's `tests/relay/handshake.test.ts` ("replayed nonce: NonceStore rejects duplicate nonces"). Pure broker-side state machine; no daemon-side invariant to guard in this repo |
| 7 | TOFU key rotation — pin, rotate broker key, refuse; `trust-broker --yes` accepts | `tests/e2e/relay/tofu_test.go: TestTOFU_RefusesAfterRotation` + `TestTOFU_TrustBrokerClearsPinAndRepins` (mutation-verified) |

Rule of thumb for future scenarios: start with a unit test; promote to testcontainers only if the same coverage can't be expressed daemon-side without rebuilding broker wire-format semantics in Go.

## Running locally

```sh
make test-e2e-relay
```

Requirements:

- **Docker** must be available on the host. The Makefile target fails fast with a clear message if the `docker` binary isn't on `$PATH`.
- The first run pulls `dicodeayo/dicode-relay:<tag>` from Docker Hub (under a minute on most connections); subsequent runs reuse the cached image.

## Pinning the relay image version

The `FROM` line in [`tests/e2e/relay/testdata/Dockerfile.relay`](../../tests/e2e/relay/testdata/Dockerfile.relay) is the single source of truth. testcontainers-go builds that Dockerfile on the fly (no `RUN` steps → cheap pull + tag) and runs the resulting image. Bump the tag there, the tests pick it up on the next run.

Dependabot's docker ecosystem watches the file and opens weekly bump PRs — see [`.github/dependabot.yml`](../../.github/dependabot.yml).

## Debugging a failing scenario

1. **Container exit code 1 on startup**: most commonly means the image was updated and now requires a new env var or config shape. `docker run --rm -e DICODE_E2E_MOCK_PROVIDER=1 dicodeayo/dicode-relay:<tag>` will print the underlying error in isolation.
2. **Handshake timeout**: check the `wait.ForLog("dicode-relay listening")` matcher in `relay_test.go` still matches the image's startup log line.
3. **Forwarding returns empty body**: previously caused by the `DICODE_E2E_MOCK_PROVIDER` router globally consuming `application/json` bodies (fixed in dicode-relay ≥ 0.1.2). If it returns, re-inspect the middleware mount order in the relay's `src/index.ts`.
4. **`invalid signature` on `/auth/:provider`**: hash-depth mismatch between daemon's `SignAuthPayload` and broker's `verifyECDSA`. See [#151](https://github.com/dicode-ayo/dicode-core/issues/151) / [#152](https://github.com/dicode-ayo/dicode-core/pull/152) / `TestSignAuthPayload_MatchesNodeVerify_Shape` for the guard.

Run a single scenario verbosely:

```sh
go test -tags e2e -timeout 180s -v -run TestHappyPath_HandshakeAndForward ./tests/e2e/relay/...
```

## Why not just mock?

Unit tests on both sides cover wire parsing, crypto primitives, and handler logic in isolation. They catch bugs **inside** each service. They do **not** catch wire drift, protocol version negotiation regressions, silent-failure classes from cross-implementation key reuse, or TOFU reset workflows. These are exactly the classes of bugs that make alpha users' `dicode relay` connection fail silently. The cost of one testcontainers run per CI pipeline is worth the signal — see [epic #136](https://github.com/dicode-ayo/dicode-core/issues/136) for the full rationale.
