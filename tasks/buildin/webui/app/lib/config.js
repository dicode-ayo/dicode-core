// Shared config cache. Fetches /api/config once, re-fetches on demand.
import { get } from './api.js';

let _cfg = null;
let _pending = null;

export async function getConfig() {
  if (_cfg) return _cfg;
  if (_pending) return _pending;
  _pending = get('/api/config').then(c => { _cfg = c; _pending = null; return c; });
  return _pending;
}

export function invalidateConfig() { _cfg = null; }

export async function relayHookBaseURL() {
  const cfg = await getConfig();
  return cfg?.relay_hook_base_url || '';
}

export function webhookURL(relayBase, path) {
  if (!relayBase) return path;
  // Relay base already ends with /hooks/ and webhook paths start with /hooks/,
  // so strip the /hooks prefix from the path to avoid /hooks/hooks/.
  const suffix = path.replace(/^\/hooks\//, '');
  return relayBase.replace(/\/+$/, '/') + suffix;
}
