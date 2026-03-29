/*!
 * dicode.js — client SDK for webhook task UIs.
 *
 * Automatically injected by dicode when serving a task's index.html.
 * Exposes window.dicode with methods to run tasks, stream logs, and
 * auto-enhance HTML forms.
 *
 * Complexity levels for task UI authors:
 *
 *   Level 1 — Zero JS: plain <form method="POST"> works out of the box.
 *     The server parses the form body and redirects to /runs/{id}/result.
 *
 *   Level 2 — Auto-enhanced: add data-dicode to any <form>.
 *     dicode.js intercepts submission, streams logs into data-output target.
 *
 *   Level 3 — Full API: use dicode.execute(params, { onLog, onFinish }).
 *     Full control over rendering and error handling.
 */
(function () {
  'use strict';

  // Context injected by the server into <head>.
  var hookMeta = document.querySelector('meta[name="dicode-hook"]');
  var taskMeta = document.querySelector('meta[name="dicode-task"]');
  var hookPath = hookMeta ? hookMeta.content : window.location.pathname;
  var taskID   = taskMeta ? taskMeta.content : '';

  /** Open a WebSocket to the dicode hub. */
  function openWS() {
    var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    return new WebSocket(proto + '//' + location.host + '/ws');
  }

  // ── ANSI → HTML ──────────────────────────────────────────────────────────
  // Catppuccin Mocha palette mapped to standard ANSI foreground colour codes.
  var ANSI_FG = {
    30: '#585b70', 31: '#f38ba8', 32: '#a6e3a1', 33: '#f9e2af',
    34: '#89b4fa', 35: '#cba6f7', 36: '#89dceb', 37: '#cdd6f4',
    90: '#6c7086', 91: '#f38ba8', 92: '#a6e3a1', 93: '#f9e2af',
    94: '#89b4fa', 95: '#cba6f7', 96: '#89dceb', 97: '#cdd6f4'
  };

  function escHtml(s) {
    return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }

  /**
   * Convert a string containing ANSI SGR escape sequences to an HTML string.
   * Text content is HTML-escaped; styles become inline <span> elements.
   * Safe to set as innerHTML / insertAdjacentHTML.
   *
   * @param {string} text
   * @returns {string}
   */
  function ansiToHtml(text) {
    var parts  = text.split(/(\x1b\[[0-9;]*m)/);
    var out    = '';
    var bold   = false, italic = false, color = '';

    for (var i = 0; i < parts.length; i++) {
      var part = parts[i];
      if (part.charAt(0) === '\x1b' && part.charAt(part.length - 1) === 'm') {
        var seq   = part.slice(2, -1);
        var codes = seq === '' ? [0] : seq.split(';');
        for (var j = 0; j < codes.length; j++) {
          var c = parseInt(codes[j], 10) || 0;
          if      (c === 0)  { bold = false; italic = false; color = ''; }
          else if (c === 1)  { bold   = true;  }
          else if (c === 3)  { italic = true;  }
          else if (c === 22) { bold   = false; }
          else if (c === 23) { italic = false; }
          else if (c === 39) { color  = '';    }
          else if (ANSI_FG[c]) { color = ANSI_FG[c]; }
        }
      } else if (part !== '') {
        var escaped = escHtml(part);
        if (bold || italic || color) {
          var styles = [];
          if (bold)   styles.push('font-weight:bold');
          if (italic) styles.push('font-style:italic');
          if (color)  styles.push('color:' + color);
          out += '<span style="' + styles.join(';') + '">' + escaped + '</span>';
        } else {
          out += escaped;
        }
      }
    }
    return out;
  }

  var dicode = {
    /** Context for the current task UI page. */
    task: { id: taskID, hookPath: hookPath },

    /**
     * Run the task asynchronously with the given params.
     * Returns a Promise resolving to { runId: string }.
     *
     * @param {Object} [params] - Key/value params passed to the task.
     */
    run: function (params) {
      params = params || {};
      return fetch(hookPath + '?wait=false', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(params)
      }).then(function (res) {
        // Run ID is available both as a header and in the JSON body.
        var runId = res.headers.get('X-Run-Id');
        return res.json().then(function (body) {
          return { runId: runId || body.runId };
        });
      });
    },

    /**
     * Stream logs and completion events for a run via WebSocket.
     * Returns an unsubscribe function that closes the connection.
     *
     * @param {string} runId
     * @param {{ onLog?: (msg, data) => void, onFinish?: (data) => void, onError?: (e) => void }} handlers
     */
    stream: function (runId, handlers) {
      handlers = handlers || {};
      var ws = openWS();

      ws.onmessage = function (evt) {
        var msg;
        try { msg = JSON.parse(evt.data); } catch (e) { return; }
        var d = msg.data;
        if (!d || d.runID !== runId) return;

        if (msg.type === 'run:log' && handlers.onLog) {
          handlers.onLog(d.message, d);
        }
        if (msg.type === 'run:finished') {
          if (handlers.onFinish) handlers.onFinish(d);
          ws.close();
        }
      };

      ws.onerror = function (e) {
        if (handlers.onError) handlers.onError(e);
      };

      return function unsubscribe() { ws.close(); };
    },

    /**
     * Run the task and stream results in one call.
     * Returns a Promise resolving to the RunFinishedData payload.
     *
     * @param {Object} [params]
     * @param {{ onLog?: (msg, data) => void, onFinish?: (data) => void, onError?: (e) => void }} [handlers]
     */
    execute: function (params, handlers) {
      var self = this;
      handlers = handlers || {};
      return self.run(params).then(function (res) {
        return new Promise(function (resolve, reject) {
          self.stream(res.runId, {
            onLog: handlers.onLog,
            onFinish: function (data) {
              if (handlers.onFinish) handlers.onFinish(data);
              resolve(data);
            },
            onError: function (e) {
              if (handlers.onError) handlers.onError(e);
              reject(e);
            }
          });
        });
      });
    },

    /**
     * Fetch the stored result for a completed run from the REST API.
     * @param {string} runId
     */
    result: function (runId) {
      return fetch('/api/runs/' + runId).then(function (r) { return r.json(); });
    },

    /**
     * Convert a string containing ANSI escape sequences to an HTML string.
     * Safe to use with innerHTML / insertAdjacentHTML.
     * @param {string} text
     * @returns {string}
     */
    ansiToHtml: ansiToHtml
  };

  // ── Auto-enhanced forms ───────────────────────────────────────────────────
  // Any <form data-dicode> is intercepted on submit. Logs are streamed into
  // the element matching the data-output CSS selector (if provided).

  function enhanceForms() {
    var forms = document.querySelectorAll('form[data-dicode]');
    for (var i = 0; i < forms.length; i++) {
      (function (form) {
        var outputSel = form.getAttribute('data-output');
        var output    = outputSel ? document.querySelector(outputSel) : null;

        form.addEventListener('submit', function (e) {
          e.preventDefault();

          var fd     = new FormData(form);
          var params = {};
          fd.forEach(function (val, key) { params[key] = val; });

          var btn = form.querySelector('[type="submit"]') ||
                    form.querySelector('button:not([type])');
          if (btn) { btn.disabled = true; }
          if (output) { output.innerHTML = ''; }

          dicode.execute(params, {
            onLog: function (line) {
              if (output) { output.insertAdjacentHTML('beforeend', ansiToHtml(line) + '\n'); }
            },
            onFinish: function () {
              if (btn) { btn.disabled = false; }
            },
            onError: function () {
              if (btn) { btn.disabled = false; }
              if (output) { output.insertAdjacentHTML('beforeend', '\nConnection error.'); }
            }
          }).catch(function () {
            if (btn) { btn.disabled = false; }
          });
        });
      })(forms[i]);
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', enhanceForms);
  } else {
    enhanceForms();
  }

  window.dicode = dicode;
})();
