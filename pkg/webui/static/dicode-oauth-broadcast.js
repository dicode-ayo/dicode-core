/*!
 * dicode-oauth-broadcast.js — notifies peer tabs when an OAuth flow stores
 * secrets, so they can react without polling.
 *
 * Loaded by tasks/auth/_oauth/flow.ts (successHtml) as:
 *   <script src="/dicode-oauth-broadcast.js?keys=FOO,BAR" defer></script>
 *
 * Served as an external file rather than inline because the webui's CSP
 * (see pkg/webui/auth.go securityHeaders) blocks inline <script> — the
 * inline form would silently fail to fire.
 *
 * Broadcasts { type: "stored", keys: [...] } on a BroadcastChannel named
 * "dicode-secrets", and also via window.opener.postMessage when the tab
 * was opened from a peer (e.g. the chat UI's "Authorize" button).
 */
(function () {
  'use strict';

  var script = document.currentScript;
  if (!script) return;

  var src = script.src || '';
  var q = src.indexOf('?');
  if (q < 0) return;
  var params = new URLSearchParams(src.slice(q + 1));
  var keysRaw = params.get('keys') || '';
  if (!keysRaw) return;

  var keys = keysRaw.split(',').map(function (s) { return s.trim(); }).filter(Boolean);
  if (!keys.length) return;

  var payload = { type: 'stored', keys: keys };

  try {
    var ch = new BroadcastChannel('dicode-secrets');
    ch.postMessage(payload);
    ch.close();
  } catch (_) { /* BroadcastChannel unsupported */ }

  try {
    if (window.opener && window.opener !== window) {
      window.opener.postMessage(payload, '*');
    }
  } catch (_) { /* cross-origin or no opener */ }
})();
