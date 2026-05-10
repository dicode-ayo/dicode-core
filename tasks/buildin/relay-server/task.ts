// buildin/relay-server — daemon task that runs dicode-relay in-process
// under the daemon's Deno runtime.
//
// OAuth client secrets and the status password are sourced from Doppler via
// the existing buildin/secret-providers/doppler task (declared in task.yaml's
// permissions.env). The only deployment-specific value supplied via task
// params is `base_url`, the public URL the relay is reachable at.
//
// The relay's loadConfig() reads ${VAR} placeholders out of process.env at
// startup, so this task body just surfaces base_url as BASE_URL and hands
// off to the relay's library entry point. No subprocess, no shell-out.

import { startServer } from "npm:dicode-relay@^0.1.6/start";
import type { DicodeSdk } from "../../sdk.ts";

export default async function main({ params }: DicodeSdk): Promise<void> {
  const baseUrl = await params.get("base_url");
  if (!baseUrl) {
    // params.base_url is marked required in task.yaml, so the daemon should
    // refuse to dispatch without it; this guard is defence in depth.
    throw new Error("base_url param is required");
  }

  // Relay's loadConfig() walks YAML strings and replaces ${VAR} from
  // process.env. Deno's node-compat exposes Deno.env as process.env.
  Deno.env.set("BASE_URL", baseUrl);

  const configPath = new URL("./relay.yaml", import.meta.url).pathname;
  const handle = await startServer({ configPath });

  // First buildin to use Deno.addSignalListener directly: the relay's
  // startServer returns a long-lived http.Server rather than an abortable
  // promise, so we have to bridge OS signals to handle.close() manually.
  // Future buildins that expose AbortSignal should prefer that pattern.
  //
  // No explicit Deno.exit(0): handle.close() tears down the http.Server,
  // which fires the `close` event below, which resolves main(). Exiting
  // explicitly bypasses the daemon engine's "task returned" accounting
  // (compare relay-client/task.ts) and short-circuits any in-flight
  // microtasks the relay's close() queued.
  const shutdown = async () => {
    await handle.close();
  };
  Deno.addSignalListener("SIGTERM", shutdown);
  Deno.addSignalListener("SIGINT", shutdown);

  // Keep the daemon task alive while the relay is running. RelayServer's
  // underlying http.Server emits `close` when listen() is undone (clean
  // shutdown or crash); resolving here lets the task exit cleanly so the
  // engine can apply its restart policy.
  await new Promise<void>((resolve) => {
    handle.httpServer.on("close", () => resolve());
  });
}
