import type { DicodeSdk } from "../sdk.ts";
/**
 * WebUI Example task
 *
 * Handles three actions dispatched from the browser UI:
 *   status  — return runtime info (uptime, version, timestamp)
 *   ping    — return a pong with a timestamp
 *   echo    — echo the query param back with a log line
 */

export default async function main({ params }: DicodeSdk) {
  const action = String((await params.get("action")) ?? "ping").trim();
  const query  = String((await params.get("query"))  ?? "").trim();

  switch (action) {
    case "status": {
      console.log("Collecting status…");

      const version = Deno.env.get("DICODE_VERSION") ?? "unknown";
      const uptimeMs = performance.now();
      const uptimeSec = Math.round(uptimeMs / 1000);

      console.log(`Uptime: ${uptimeSec}s  |  Version: ${version}`);

      return {
        action: "status",
        version,
        uptime: uptimeSec,
        uptimeMs,
        timestamp: new Date().toISOString(),
        runtime: {
          deno: Deno.version.deno,
          typescript: Deno.version.typescript,
          v8: Deno.version.v8,
          os: Deno.build.os,
          arch: Deno.build.arch,
        },
      };
    }

    case "ping": {
      console.log("Pong!");
      return {
        action: "ping",
        pong: true,
        timestamp: new Date().toISOString(),
      };
    }

    case "echo": {
      if (!query) throw new Error("echo action requires a non-empty 'query' param");
      console.log(`Echo: ${query}`);
      return {
        action: "echo",
        query,
        timestamp: new Date().toISOString(),
      };
    }

    default:
      throw new Error(
        `Unknown action "${action}". Valid actions: status | ping | echo`,
      );
  }
}
