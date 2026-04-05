import type { DicodeSdk } from "../../sdk.ts";
export default async function main({}: DicodeSdk) {
  console.log("Collecting system info…");

  const mem = Deno.memoryUsage();

  const info = {
    deno:     Deno.version.deno,
    typescript: Deno.version.typescript,
    v8:       Deno.version.v8,
    os:       Deno.build.os,
    arch:     Deno.build.arch,
    memory: {
      rss:        mem.rss,
      heapTotal:  mem.heapTotal,
      heapUsed:   mem.heapUsed,
      external:   mem.external,
    },
    pid: Deno.pid,
    uptime: performance.now(),
  };

  console.log(`OS: ${info.os} / ${info.arch}`);
  console.log(`Deno: ${info.deno}`);
  console.log(`Heap used: ${Math.round(mem.heapUsed / 1024 / 1024)} MB`);

  return info;
}
