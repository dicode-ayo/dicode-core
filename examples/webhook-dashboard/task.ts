import type { DicodeSdk } from "../sdk.ts";
export default async function main({ log }: DicodeSdk) {
  await log.info("Collecting system info…");

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

  await log.info(`OS: ${info.os} / ${info.arch}`);
  await log.info(`Deno: ${info.deno}`);
  await log.info(`Heap used: ${Math.round(mem.heapUsed / 1024 / 1024)} MB`);

  return info;
}
