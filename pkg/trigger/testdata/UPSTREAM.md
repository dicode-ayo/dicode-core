# Upstream copies

`template/`, `write-local/`, `local-storage/` and `sdk.ts` are verbatim copies
from [dicode-buildin](https://github.com/dicode-ayo/dicode-buildin), where
those tasks are maintained. Everything else under `testdata/` is a fixture local to
this package.

The engine tests drive all three as real tasks rather than stubs: `template/`
is the renderer whose `${VAR}` and literal-passthrough behavior
`e2e_input_output_test.go` asserts, `write-local/` is the terminal stage
`e2e_relay_pipeline_test.go` exercises, and `local-storage/` is the real
InputStore backend `e2e_inputstore_largestate_test.go` offloads through. They
are copied rather than resolved over git so `go test` needs neither network nor
a second checkout.

Kept byte-identical to upstream, so diffing against a dicode-buildin clone is
the whole staleness check. `sdk.ts` rides along so `write-local`'s type-only
import resolves at the same relative depth it does upstream.
