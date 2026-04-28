// buildin/secret-providers/doppler — Doppler REST API secret provider (issue #119).
//
// Contract:
//   input params:
//     requests: JSON-encoded [{name: string, optional: boolean}, ...]
//                (the trigger engine wraps the array in {"requests": [...]}
//                via json.Marshal — we accept both shapes defensively)
//   env (declared in permissions.env):
//     DOPPLER_TOKEN: workspace service token (set via `dicode secrets set`)
//   output:
//     dicode.output({ <name>: <value>, ... }, { secret: true })
//
// We hit `GET https://api.doppler.com/v3/configs/config/secrets` with an
// auth header and pluck only the requested keys. Optional misses become
// absent in the output map; required misses surface as a thrown error
// which the trigger engine renders as required_secret_missing.

import type { DicodeSdk, Output as BaseOutput } from "../../../../sdk.ts";

interface SecretRequest { name: string; optional: boolean; }

interface DopplerSecretsResp {
  secrets: Record<string, { computed: string }>;
}

// The Bundle E SDK shim makes `output` callable in addition to the
// .html/.text/.image/.file methods, but tasks/sdk.ts hasn't been updated
// to reflect that yet. Locally extend the Output type so the provider
// entry-point typechecks against the runtime shape.
type ProviderOutput = BaseOutput & {
  (value: Record<string, string>, opts: { secret: true }): Promise<void>;
};

type ProviderSdk = Omit<DicodeSdk, "output"> & { output: ProviderOutput };

function parseRequests(raw: string | null): SecretRequest[] {
  const text = raw ?? "[]";
  const parsed: unknown = JSON.parse(text);
  // Engine wraps as {"requests": [...]}; accept that shape or a bare array.
  if (Array.isArray(parsed)) return parsed as SecretRequest[];
  if (parsed && typeof parsed === "object" && Array.isArray((parsed as { requests?: unknown }).requests)) {
    return (parsed as { requests: SecretRequest[] }).requests;
  }
  throw new Error(`doppler: invalid requests payload: ${text}`);
}

export default async function main({ params, output }: ProviderSdk) {
  const requests = parseRequests(await params.get("requests"));

  const token = Deno.env.get("DOPPLER_TOKEN");
  if (!token) {
    throw new Error("DOPPLER_TOKEN not set; run `dicode secrets set DOPPLER_TOKEN dp.st.xxx`");
  }

  const resp = await fetch("https://api.doppler.com/v3/configs/config/secrets", {
    headers: {
      "Accept": "application/json",
      "Authorization": "Bearer " + token,
    },
  });
  if (!resp.ok) {
    throw new Error(`Doppler API ${resp.status}: ${await resp.text()}`);
  }
  const body = (await resp.json()) as DopplerSecretsResp;

  const out: Record<string, string> = {};
  for (const r of requests) {
    const entry = body.secrets?.[r.name];
    if (entry && typeof entry.computed === "string") {
      out[r.name] = entry.computed;
    } else if (!r.optional) {
      throw new Error(`required secret ${r.name} not present in Doppler config`);
    }
  }

  await output(out, { secret: true });
}
