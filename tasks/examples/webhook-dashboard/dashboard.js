var gridEl   = document.getElementById('grid');
var statusEl = document.getElementById('status');
var btn      = document.getElementById('refresh-btn');

function fmt(bytes) {
  return (bytes / 1024 / 1024).toFixed(1) + ' MB';
}

function renderTiles(data) {
  var tiles = [
    { label: 'Deno',       value: data.deno },
    { label: 'TypeScript', value: data.typescript },
    { label: 'V8',         value: data.v8 },
    { label: 'OS',         value: data.os + ' / ' + data.arch },
    { label: 'PID',        value: data.pid },
    { label: 'Heap used',  value: fmt(data.memory.heapUsed) },
    { label: 'Heap total', value: fmt(data.memory.heapTotal) },
    { label: 'RSS',        value: fmt(data.memory.rss) },
  ];

  gridEl.innerHTML = tiles.map(function(t) {  // eslint-disable-line -- trusted system data
    return '<div class="tile"><div class="label">' + t.label +
           '</div><div class="value">' + t.value + '</div></div>';
  }).join('');
}

function refresh() {
  btn.disabled = true;
  gridEl.textContent = '';
  statusEl.textContent = 'Running…';

  dicode.execute({}, {
    onFinish: function(data) {
      btn.disabled = false;

      if (data.status !== 'success') {
        statusEl.textContent = 'Run failed';
        return;
      }

      // dicode.execute() parses application/json bodies into an object on
      // data.returnValue. Fall back to parsing data.body for tasks that
      // return a non-JSON content type but a JSON body anyway.
      var info = data.returnValue;
      if (!info) {
        try { info = JSON.parse(data.body); } catch (e) {
          statusEl.textContent = 'Could not parse result';
          return;
        }
      }
      renderTiles(info);
      statusEl.textContent = 'Last refreshed ' + new Date().toLocaleTimeString();
    },
    onError: function() {
      btn.disabled = false;
      statusEl.textContent = 'Connection error — is dicode running?';
    }
  }).catch(function() {
    btn.disabled = false;
    statusEl.textContent = 'Connection error — is dicode running?';
  });
}

btn.addEventListener('click', refresh);

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', refresh);
} else {
  refresh();
}
