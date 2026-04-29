// buildin/secret-providers/doppler — Doppler REST API secret provider (issue #119).
//
// Contract:
//   input params:
//     requests: JSON-encoded [{name: string, optional: boolean}, ...]
//                (a bare JSON array — the trigger engine encodes the request
//                list directly at params["requests"])
//   env (declared in permissions.env):
//     DOPPLER_TOKEN: workspace service token (set via `dicode secrets set`)
//   output:
//     dicode.output({ <name>: <value>, ... }, { secret: true })
//
// We hit `GET https://api.doppler.com/v3/configs/config/secrets` with an
// auth header and pluck only the requested keys. Optional misses become
// absent in the output map; required misses surface as a thrown error
// which the trigger engine renders as required_secret_missing.

import type { DicodeSdk } from "../../../sdk.ts";

interface SecretRequest { name: string; optional: boolean; }

interface DopplerSecretsResp {
  secrets: Record<string, { computed: string }>;
}

export default async function main({ params, output }: DicodeSdk) {
  const reqsJSON = (await params.get("requests")) ?? "[]";
  const requests: SecretRequest[] = JSON.parse(reqsJSON);

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
