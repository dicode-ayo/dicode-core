const PLACEHOLDER_RE = /\$\{([A-Za-z_][A-Za-z0-9_]*)\}/g;
export default async function main({ params }) {
  const tpl = await params.get("template");
  const outpath = await params.get("outpath");
  if (tpl === null) throw new Error("missing template param");
  if (outpath === null) throw new Error("missing outpath param");
  const rendered = tpl.replace(PLACEHOLDER_RE, (_m, name) => {
    const v = Deno.env.get(name);
    if (v === undefined) throw new Error("unresolved placeholder: ${" + name + "}");
    return v;
  });
  await Deno.writeTextFile(outpath, rendered);
  return rendered;
}
