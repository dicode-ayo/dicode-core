// GitHub push webhook handler.
//
// Setup (GitHub side):
//   Repository → Settings → Webhooks → Add webhook
//     Payload URL:   https://your-dicode-host/hooks/github-push
//     Content type:  application/json
//     Secret:        <same value you stored as GITHUB_WEBHOOK_SECRET>
//     Events:        Just the push event
//
// Setup (dicode side):
//   Store the shared secret once:
//     dicode secret set GITHUB_WEBHOOK_SECRET <your-secret>
//
// dicode verifies the X-Hub-Signature-256 header before this script runs.
// If the signature is missing or wrong the request is rejected with 403 and
// the script is never executed.

// Input is the parsed JSON body GitHub sends for a push event.
const event = params.input as {
  ref: string;
  before: string;
  after: string;
  repository: { full_name: string; html_url: string };
  pusher: { name: string };
  commits: Array<{
    id: string;
    message: string;
    author: { name: string };
    added: string[];
    removed: string[];
    modified: string[];
  }>;
};

if (!event?.repository) {
  throw new Error("Unexpected payload — missing repository field");
}

const branch = event.ref.replace("refs/heads/", "");
const repo   = event.repository.full_name;
const pusher = event.pusher?.name ?? "unknown";
const commits = event.commits ?? [];

await log.info(
  `Push to ${repo} on branch ${branch} by ${pusher} — ${commits.length} commit(s)`,
);

// Collect all touched files across commits.
const added    = new Set<string>();
const removed  = new Set<string>();
const modified = new Set<string>();

for (const c of commits) {
  c.added?.forEach(f => added.add(f));
  c.removed?.forEach(f => removed.add(f));
  c.modified?.forEach(f => modified.add(f));
  await log.info(`  ${c.id.slice(0, 7)}  ${c.message.split("\n")[0]}`);
}

// ── Persist a run counter per repo/branch for trend display ──────────────────
const kvKey = `github-push:${repo}:${branch}:count`;
const prev  = (await kv.get(kvKey) as number | null) ?? 0;
const count = prev + 1;
await kv.set(kvKey, count);

// ── Build a compact HTML summary ─────────────────────────────────────────────
function fileList(files: Set<string>, colour: string, label: string) {
  if (files.size === 0) return "";
  const items = [...files].slice(0, 10).map(
    f => `<li style="font-family:monospace;font-size:.8rem">${f}</li>`,
  ).join("");
  const more = files.size > 10
    ? `<li style="color:#888">…and ${files.size - 10} more</li>` : "";
  return `
    <div style="margin-top:.75rem">
      <span style="color:${colour};font-weight:600">${label} (${files.size})</span>
      <ul style="margin:.25rem 0 0 1rem;padding:0">${items}${more}</ul>
    </div>`;
}

const commitRows = commits.slice(0, 5).map(c => `
  <tr>
    <td style="font-family:monospace;font-size:.8rem;padding:.3rem .6rem">
      <a href="${event.repository.html_url}/commit/${c.id}" target="_blank"
         style="color:#0d6efd">${c.id.slice(0, 7)}</a>
    </td>
    <td style="padding:.3rem .6rem;font-size:.88rem">${c.message.split("\n")[0]}</td>
    <td style="padding:.3rem .6rem;font-size:.82rem;color:#555">${c.author.name}</td>
  </tr>`).join("");

const moreCommits = commits.length > 5
  ? `<tr><td colspan="3" style="padding:.3rem .6rem;color:#888;font-size:.82rem">
       …and ${commits.length - 5} more commits</td></tr>` : "";

return output.html(`
<div style="font-family:system-ui,sans-serif;max-width:600px;padding:1.5rem">

  <div style="display:flex;align-items:baseline;gap:.75rem;margin-bottom:1.25rem">
    <h2 style="margin:0">
      <a href="${event.repository.html_url}/tree/${branch}"
         target="_blank" style="text-decoration:none;color:inherit">
        ${repo}
      </a>
    </h2>
    <span style="
      background:#dbeafe;color:#1d4ed8;
      padding:.15rem .55rem;border-radius:999px;font-size:.8rem;font-weight:600
    ">${branch}</span>
    <span style="color:#888;font-size:.82rem">push #${count}</span>
  </div>

  <p style="margin:0 0 1rem;color:#555;font-size:.9rem">
    Pushed by <strong>${pusher}</strong> ·
    ${commits.length} commit${commits.length !== 1 ? "s" : ""}
  </p>

  <table style="width:100%;border-collapse:collapse;background:#f8f9fa;border-radius:6px;overflow:hidden">
    <thead>
      <tr style="background:#e9ecef">
        <th style="padding:.35rem .6rem;text-align:left;font-size:.8rem">SHA</th>
        <th style="padding:.35rem .6rem;text-align:left;font-size:.8rem">Message</th>
        <th style="padding:.35rem .6rem;text-align:left;font-size:.8rem">Author</th>
      </tr>
    </thead>
    <tbody>${commitRows}${moreCommits}</tbody>
  </table>

  ${fileList(added,    "#2da44e", "Added")}
  ${fileList(modified, "#d97706", "Modified")}
  ${fileList(removed,  "#cf222e", "Removed")}

</div>
`);
