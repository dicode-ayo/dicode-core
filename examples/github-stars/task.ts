const repo = await params.get("repo");
const kvKey = `github-stars:${repo}`;

await log.info(`Fetching star count for ${repo}…`);

const res = await fetch(`https://api.github.com/repos/${repo}`, {
  headers: { "Accept": "application/vnd.github+json" },
});

if (!res.ok) {
  throw new Error(`GitHub API error: ${res.status} ${res.statusText}`);
}

const data = await res.json();
const stars: number = data.stargazers_count;
const forks: number = data.forks_count;
const description: string = data.description ?? "";

const prev = await kv.get(kvKey) as { stars: number } | null;
await kv.set(kvKey, { stars });

const delta = prev ? stars - prev.stars : null;
const trend =
  delta === null ? "first run"
  : delta > 0    ? `+${delta} since last run`
  : delta < 0    ? `${delta} since last run`
  :                "no change since last run";

await log.info(`${repo}: ${stars.toLocaleString()} ⭐  (${trend})`);

const trendBadge =
  delta === null ? `<span style="color:#888">first run</span>`
  : delta > 0    ? `<span style="color:#2da44e">▲ +${delta}</span>`
  : delta < 0    ? `<span style="color:#cf222e">▼ ${delta}</span>`
  :                `<span style="color:#888">— no change</span>`;

return output.html(`
<div style="font-family:system-ui,sans-serif;max-width:480px;padding:1.5rem">
  <h2 style="margin:0 0 .25rem">
    <a href="https://github.com/${repo}" target="_blank" style="text-decoration:none;color:inherit">${repo}</a>
  </h2>
  <p style="margin:0 0 1.5rem;color:#555;font-size:.9rem">${description}</p>
  <div style="display:flex;gap:2rem">
    <div>
      <div style="font-size:2rem;font-weight:700">${stars.toLocaleString()}</div>
      <div style="color:#888;font-size:.85rem">⭐ stars &nbsp; ${trendBadge}</div>
    </div>
    <div>
      <div style="font-size:2rem;font-weight:700">${forks.toLocaleString()}</div>
      <div style="color:#888;font-size:.85rem">🍴 forks</div>
    </div>
  </div>
</div>
`);
