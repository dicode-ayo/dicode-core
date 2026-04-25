# proto/

Source of truth for the relay WebSocket protocol schema. Both sides generate
their wire types from this directory:

- **dicode-core** (Go) → [`pkg/relay/pb/`](../pkg/relay/pb/) via `protoc-gen-go`
- **dicode-relay** (Node) → `src/relay/pb/` via `@bufbuild/protoc-gen-es`,
  vendored from this file by `curl` (see
  [dicode-relay's `proto/README.md`](https://github.com/dicode-ayo/dicode-relay/blob/main/proto/README.md))

JSON over WebSocket text frames. Go's `protojson` and
`@bufbuild/protobuf`'s `fromJson`/`toJson` produce matching output, with
`UseProtoNames: true` on the Go side keeping field names snake_case.

## Regenerate

```sh
make proto-tools   # one-time: installs buf + protoc-gen-go into $GOBIN
make proto         # lints proto/ then writes pkg/relay/pb/*.pb.go
```

`make proto` is idempotent — running it on a clean tree must produce no diff.
If it does, commit the regenerated output.

## Bumping the protocol version

`Welcome.protocol` is the wire version. Daemons reject brokers advertising
`< 3`. Bump it when a wire-incompatible change lands, and update the floor
check in [`pkg/relay/protocol.go`](../pkg/relay/protocol.go) at the same
time. Coordinate with dicode-relay so its broker advertises the new value.

## Keeping dicode-relay in sync

dicode-relay vendors `relay.proto` via `curl` — there is no submodule, so
drift is not automatic. After merging a change here:

1. Open a follow-up PR in dicode-relay that re-runs the resync curl + `npm
   run proto`, then bumps the upstream commit reference in its
   `proto/README.md`.
2. Until the planned drift-detection CI lands (TODO in dicode-relay), this
   step is manual and easy to forget.
