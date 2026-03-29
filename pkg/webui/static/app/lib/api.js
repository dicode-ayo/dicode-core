export async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  if (!res.ok) throw new Error(await res.text());
  const ct = res.headers.get('Content-Type') || '';
  if (ct.includes('application/json')) return res.json();
  return res.text();
}
export const get   = (path)       => api('GET',    path);
export const post  = (path, body) => api('POST',   path, body);
export const patch = (path, body) => api('PATCH',  path, body);
export const del   = (path)       => api('DELETE', path);
