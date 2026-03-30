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
  // Use ansi-to-html from esm.sh (same package as the main UI uses).
  // Dynamic import so this standalone script doesn't need to be a module.
  // Until the package loads, ansiToHtml falls back to HTML-escaping only.

  var _convert = null;
  import('https://esm.sh/ansi-to-html@0.7.2').then(function (m) {
    _convert = new m.default({
      fg: '#cdd6f4', bg: '#1e1e2e', newline: false, escapeXML: true,
      colors: {
        0:  '#585b70', 1:  '#f38ba8', 2:  '#a6e3a1', 3:  '#f9e2af',
        4:  '#89b4fa', 5:  '#cba6f7', 6:  '#89dceb', 7:  '#cdd6f4',
        8:  '#6c7086', 9:  '#f38ba8', 10: '#a6e3a1', 11: '#f9e2af',
        12: '#89b4fa', 13: '#cba6f7', 14: '#89dceb', 15: '#cdd6f4',
      },
    });
  });

  function escHtml(s) {
    return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }

  /**
   * Convert a string containing ANSI escape sequences to an HTML string.
   * Falls back to plain HTML-escaping until ansi-to-html finishes loading.
   * Safe for innerHTML / insertAdjacentHTML.
   *
   * @param {string} text
   * @returns {string}
   */
  function ansiToHtml(text) {
    return _convert ? _convert.toHtml(text) : escHtml(text);
  }

  var dicode = {
    /** Context for the current task UI page. */
    task: { id: taskID, hookPath: hookPath },

    /**
     * Attempt a silent session refresh via the device-token cookie.
     * Returns a Promise<boolean> — true if the refresh succeeded.
     * @private
     */
    _tryRefresh: function () {
      return fetch('/api/auth/refresh', { method: 'POST' })
        .then(function (res) { return res.ok; })
        .catch(function () { return false; });
    },

    /**
     * Handle a 401 response: try silent refresh, then redirect to login page.
     * Returns a Promise that resolves with the retried fetch Response when
     * refresh succeeds, or redirects the browser on failure.
     * @private
     */
    _handle401: function (retryFn) {
      var self = this;
      return self._tryRefresh().then(function (refreshed) {
        if (refreshed) {
          return retryFn();
        }
        location.href = '/?auth=required';
        // Return a never-resolving promise so callers don't proceed.
        return new Promise(function () {});
      });
    },

    /**
     * Run the task asynchronously with the given params.
     * Returns a Promise resolving to { runId: string }.
     * On 401, attempts a silent refresh; redirects to login on failure.
     *
     * @param {Object} [params] - Key/value params passed to the task.
     */
    run: function (params) {
      var self = this;
      params = params || {};
      var doFetch = function () {
        return fetch(hookPath + '?wait=false', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(params)
        });
      };
      return doFetch().then(function (res) {
        if (res.status === 401) {
          return self._handle401(doFetch).then(function (retried) {
            if (!retried) return new Promise(function () {});
            var runId = retried.headers.get('X-Run-Id');
            return retried.json().then(function (body) {
              return { runId: runId || body.runId };
            });
          });
        }
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
