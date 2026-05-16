interface Req { name: string; optional: boolean }
interface Resp { secrets: Record<string, { computed: string }> }
export default async function main({ params, output }: any) {
  const reqs: Req[] = JSON.parse((await params.get("requests")) ?? "[]");
  const url = Deno.env.get("UPSTREAM_URL");
  if (!url) throw new Error("UPSTREAM_URL not set");
  const resp = await fetch(url, { headers: { "Authorization": "Bearer test" } });
  if (!resp.ok) throw new Error("upstream " + resp.status);
  const body = (await resp.json()) as Resp;
  const out: Record<string, string> = {};
  for (const r of reqs) {
    const v = body.secrets[r.name]?.computed;
    if (typeof v === "string") out[r.name] = v;
    else if (!r.optional) throw new Error("required secret " + r.name + " missing");
  }
  await output(out, { secret: true });
}
