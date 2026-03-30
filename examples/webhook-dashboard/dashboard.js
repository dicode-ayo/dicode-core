var logEl    = document.getElementById('log-stream');
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

  gridEl.innerHTML = tiles.map(function(t) {
    return '<div class="tile"><div class="label">' + t.label +
           '</div><div class="value">' + t.value + '</div></div>';
  }).join('');
}

function refresh() {
  btn.disabled = true;
  logEl.textContent = '';
  logEl.classList.add('active');
  gridEl.innerHTML = '';
  statusEl.textContent = '';

  dicode.execute({}, {
    onLog: function(line) {
      logEl.textContent += line + '\n';
    },
    onFinish: function(data) {
      btn.disabled = false;
      logEl.classList.remove('active');
      logEl.textContent = '';

      if (data.status !== 'success') {
        statusEl.textContent = 'Run failed: ' + data.status;
        return;
      }

      var info;
      try { info = JSON.parse(data.returnValue); } catch (e) {
        statusEl.textContent = 'Could not parse result';
        return;
      }
      renderTiles(info);
      statusEl.textContent = 'Last refreshed ' + new Date().toLocaleTimeString();
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
