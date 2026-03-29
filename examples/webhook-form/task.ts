const text = String(await params.get("text") ?? "");
const op   = String(await params.get("op")   ?? "uppercase");

if (!text) throw new Error("text param is required");

await log.info(`Running "${op}" on ${text.length} character(s)…`);

let result: string;
switch (op) {
  case "uppercase":
    result = text.toUpperCase();
    break;
  case "lowercase":
    result = text.toLowerCase();
    break;
  case "reverse":
    result = [...text].reverse().join("");
    break;
  case "wordcount": {
    const words = text.trim().split(/\s+/).filter(Boolean);
    result = `${words.length} word(s), ${text.length} character(s)`;
    break;
  }
  default:
    throw new Error(`Unknown operation: ${op}`);
}

await log.info(`Result: ${result}`);

return output.html(`
<div style="font-family:system-ui,sans-serif;max-width:640px;padding:1.5rem">
  <h2 style="margin:0 0 .5rem">Result</h2>
  <p style="color:#888;font-size:.875rem">Operation: <strong>${op}</strong></p>
  <pre style="background:#1e1e2e;color:#cdd6f4;padding:1rem;border-radius:8px;
              white-space:pre-wrap;word-break:break-all">${result}</pre>
  <a href="/hooks/text-transform"
     style="color:#89b4fa;font-size:.875rem">← Transform another</a>
</div>
`);
