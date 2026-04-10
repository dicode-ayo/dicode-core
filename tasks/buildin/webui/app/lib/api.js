// Central API client. Intercepts 401 responses and triggers the auth overlay.

let _authOverlay = null; // set by app.js once the overlay element exists

export function setAuthOverlay(overlay) {
  _authOverlay = overlay;
}

// api() is the single fetch wrapper used everywhere. On 401 it first attempts
// a silent device-token refresh; if that also fails it shows the auth overlay
// and queues the original request to retry after the user logs in.
export async function api(method, path, body) {
  const res = await _fetch(method, path, body);
  if (res.status === 401) {
    // Try silent refresh via trusted-device cookie.
    const refreshed = await _tryRefresh();
    if (refreshed) {
      const retry = await _fetch(method, path, body);
      if (retry.ok) return _parse(retry);
    }
    // Show the overlay and wait for the user to authenticate.
    await _awaitLogin();
    const retry = await _fetch(method, path, body);
    if (!retry.ok) throw new Error(await retry.text());
    return _parse(retry);
  }
  if (!res.ok) throw new Error(await res.text());
  return _parse(res);
}

async function _fetch(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  return fetch(path, opts);
}

async function _parse(res) {
  const ct = res.headers.get('Content-Type') || '';
  if (ct.includes('application/json')) return res.json();
  return res.text();
}

async function _tryRefresh() {
  try {
    const res = await fetch('/api/auth/refresh', { method: 'POST' });
    return res.ok;
  } catch {
    return false;
  }
}

// _awaitLogin shows the auth overlay and returns a promise that resolves once
// the user has successfully authenticated.
function _awaitLogin() {
  return new Promise(resolve => {
    if (_authOverlay) {
      _authOverlay.show(resolve);
    } else {
      // Fallback: hard reload to the login page if overlay isn't mounted yet.
      location.href = '/?auth=required';
    }
  });
}

export const get   = (path)       => api('GET',    path);
export const post  = (path, body) => api('POST',   path, body);
export const patch = (path, body) => api('PATCH',  path, body);
export const del   = (path)       => api('DELETE', path);
