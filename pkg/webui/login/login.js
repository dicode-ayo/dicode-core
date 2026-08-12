(function () {
  var p = new URLSearchParams(location.search);
  var next = p.get('next') || '';
  var err = p.get('err');

  // Populate hidden next field. Guard against open-redirect attempts
  // (server validates again at POST time; this is client-side defence-in-depth).
  if (next && /^\/[^/\\]/.test(next)) {
    document.getElementById('next-input').value = next;
  }

  // Show error banner when the server redirected back with ?err=1.
  if (err) {
    var el = document.getElementById('dc-err');
    el.textContent = 'Incorrect password';
    el.removeAttribute('hidden');
  }

  // Fetch contextual title (task name when signing in to reach a specific task).
  var ctx = '/api/login/context';
  if (next) { ctx += '?next=' + encodeURIComponent(next); }
  fetch(ctx)
    .then(function (r) { return r.ok ? r.json() : null; })
    .then(function (d) {
      if (d && d.title) {
        document.title = d.title;
        document.getElementById('login-title').textContent = d.title;
      }
      // No passphrase actually gates this login (server.auth: false in
      // practice) — the server accepts any value here, so don't make the
      // field look like a real credential check.
      if (d && d.passphrase_required === false) {
        var pwd = document.querySelector('input[name=password]');
        pwd.removeAttribute('required');
        var note = document.getElementById('dc-note');
        note.textContent = 'No password is configured for this dicode instance. Click Sign in to continue.';
        note.removeAttribute('hidden');
      }
    })
    .catch(function () {});
}());
