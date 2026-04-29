// Issue #119 end-to-end demo: consumer task that resolves env vars
// through the buildin doppler provider. Run manually after setting
// DOPPLER_TOKEN in the local secrets store and stocking your Doppler
// config with PG_URL (and optionally REDIS_URL).
//
// We never log raw values — only the env-var name plus the length of
// the resolved string. That keeps the run log safe to share even if
// the redactor weren't doing its job (which it is).

import type { DicodeSdk } from "../../sdk.ts";

interface Report {
  resolved: { name: string; length: number; preview: string }[];
  missing: string[];
}

function preview(value: string): string {
  // First two characters + ellipsis. Enough to confirm "looks plausible"
  // without leaking. Empty values render as "(empty)".
  if (!value) return "(empty)";
  return value.length <= 2 ? "**" : value.slice(0, 2) + "…";
}

export default async function main({ output }: DicodeSdk): Promise<Report> {
  const targets = ["PG_URL", "REDIS_URL"];
  const report: Report = { resolved: [], missing: [] };

  for (const name of targets) {
    const value = Deno.env.get(name);
    if (value === undefined || value === "") {
      console.log(`${name} not resolved (optional miss or absent)`);
      report.missing.push(name);
      continue;
    }
    console.log(`${name} resolved: length=${value.length} preview=${preview(value)}`);
    report.resolved.push({ name, length: value.length, preview: preview(value) });
  }

  await output.text(
    `Doppler demo resolved ${report.resolved.length}/${targets.length} secrets. ` +
      `Resolved: [${report.resolved.map((r) => r.name).join(", ")}]. ` +
      `Missing: [${report.missing.join(", ")}].`,
  );

  return report;
}
