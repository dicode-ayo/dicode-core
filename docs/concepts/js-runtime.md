# JS Runtime (removed)

> **Removed**: the goja-based JS runtime (`pkg/runtime/js/`) documented on this page has been deleted from the codebase. Task execution is now **Deno** (TypeScript/JavaScript) and **Python** (uv). See [Deno Runtime](../deno-runtime.md) and [Python Runtime](../python-runtime.md) for the current SDKs — use `runtime: deno` or `runtime: python` in `task.yaml`.

This page is kept only so old links resolve; it no longer describes any runtime dicode ships.

## Globals reference

The goja globals this section used to document (`env`, `http`, `fs`, `server`, `dicode.trigger`/`isRunning`/`ask`) do not exist in the current SDKs. For the equivalents, see:

- [Deno Runtime — SDK globals](../deno-runtime.md#sdk-globals) for `params`, `kv`, `input`, `output`, `dicode`, `mcp`
- [Python Runtime](../python-runtime.md) for the Python SDK surface
- [Task → Orchestrator API](./orchestrator-api.md) for the `dicode.*` methods actually implemented (e.g. `dicode.run_task`)

### `output` — rich return values

Moved to [Deno Runtime — `output`](../deno-runtime.md#output).
