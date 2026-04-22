# End-to-end tests

Two independent e2e suites live in this repo:

| Suite | Stack | Runs via |
|---|---|---|
| Browser-level flows (webhook delivery, UI, auth modes) | Playwright + TypeScript | `make test-e2e` (and friends) |
| Cross-service daemon ↔ relay integration | Go + [testcontainers-go](https://golang.testcontainers.org/) | `make test-e2e-relay` |

This document covers the **daemon ↔ relay** suite. See [`tests/e2e/README.md`](../../tests/e2e/README.md) for the Playwright suite.

## What it covers

Spawns a real `dicode-relay` container (from the published image) and drives it against an in-process `pkg/relay.Client` + `pkg/ipc.Server` to catch cross-service drift that unit tests on either side cannot. The matrix lives in [tests/e2e/relay/relay_test.go](../../tests/e2e/relay/relay_test.go).

Phase A (shipped): handshake + webhook round-trip, OAuth happy path via the mock provider.

Phase B (tracked in [#137](https://github.com/dicode-ayo/dicode-core/issues/137)): split-key regression guard, protocol version guard, reconnect survivability, replay protection, TOFU key rotation.

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
