/*!
 * dicode.js — client SDK for webhook task UIs.
 *
 * Automatically injected by dicode when serving a task's index.html.
 * Exposes window.dicode with methods to run tasks and auto-enhance HTML forms.
 *
 * Complexity levels for task UI authors:
 *
 *   Level 1 — Zero JS: plain <form method="POST"> works out of the box.
 *     The server parses the form body and redirects to /runs/{id}/result.
 *
 *   Level 2 — Auto-enhanced: add data-dicode to any <form>.
 *     dicode.js intercepts submission, POSTs synchronously, and renders the
 *     task output into the data-output target element.
 *
 *   Level 3 — Full API: use dicode.execute(params, { onFinish }).
 *     Full control over rendering and error handling.
 */
(function () {
  'use strict';

  // Context injected by the server into <head>.
  var hookMeta = document.querySelector('meta[name="dicode-hook"]');
  var taskMeta = document.querySelector('meta[name="dicode-task"]');
  var hookPath = hookMeta ? hookMeta.content : window.location.pathname;
  var taskID   = taskMeta ? taskMeta.content : '';

  // ── ANSI → HTML ──────────────────────────────────────────────────────────

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

  function ansiToHtml(text) {
    return _convert ? _convert.toHtml(text) : escHtml(text);
  }

  var dicode = {
    task: { id: taskID, hookPath: hookPath },

    /**
     * @private
     */
    _tryRefresh: function () {
      return fetch('/api/auth/refresh', { method: 'POST' })
        .then(function (res) { return res.ok; })
        .catch(function () { return false; });
    },

    /**
     * @private
     */
    _handle401: function (retryFn) {
      var self = this;
      return self._tryRefresh().then(function (refreshed) {
        if (refreshed) return retryFn();
        location.href = '/?auth=required';
        return new Promise(function () {});
      });
    },

    /**
     * Execute the task synchronously with the given params.
     * POSTs to the webhook endpoint and waits for the result.
     * Returns a Promise resolving to:
     *   { runId, status, contentType, body, returnValue }
     *
     * @param {Object} [params]
     * @param {{ onFinish?: (data) => void, onError?: (e) => void }} [handlers]
     */
    execute: function (params, handlers) {
      var self = this;
      params = params || {};
      handlers = handlers || {};

      var doFetch = function () {
        return fetch(hookPath, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(params)
        });
      };

      return doFetch().then(function (res) {
        if (res.status === 401) {
          return self._handle401(doFetch);
        }
        return res;
      }).then(function (res) {
        var runId = res.headers.get('X-Run-Id') || '';
        var contentType = res.headers.get('Content-Type') || '';
        var status = res.status;
        return res.text().then(function (body) {
          var result = {
            runId: runId,
            status: status >= 200 && status < 300 ? 'success' : 'failure',
            contentType: contentType,
            body: body,
            returnValue: null
          };

          if (contentType.indexOf('application/json') !== -1) {
            try { result.returnValue = JSON.parse(body); }
            catch (e) { result.returnValue = null; }
          }

          if (handlers.onFinish) handlers.onFinish(result);
          return result;
        });
      }).catch(function (e) {
        if (handlers.onError) handlers.onError(e);
        throw e;
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
     * Convert ANSI escape sequences to HTML. Safe for innerHTML.
     * @param {string} text
     * @returns {string}
     */
    ansiToHtml: ansiToHtml
  };

  // ── Auto-enhanced forms ───────────────────────────────────────────────────
  // Any <form data-dicode> is intercepted on submit. The task runs synchronously
  // and the response is rendered into the data-output target element.
  // HTML output (from output.html()) is rendered as HTML; everything else as text.

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
          if (output) { output.textContent = 'Running…'; }

          dicode.execute(params, {
            onFinish: function (data) {
              if (btn) { btn.disabled = false; }
              if (!output) return;

              // Task output.html() → render as HTML (trusted task-author content).
              // Everything else → plain text.
              if (data.contentType && data.contentType.indexOf('text/html') !== -1) {
                output.innerHTML = data.body;  // eslint-disable-line -- trusted task output
              } else {
                output.textContent = data.body;
              }
            },
            onError: function () {
              if (btn) { btn.disabled = false; }
              if (output) { output.textContent = 'Connection error — is dicode running?'; }
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
