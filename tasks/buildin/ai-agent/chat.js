(function () {
  'use strict';

  var STORAGE_KEY = "dicode-ai-session";
  var sessionId = localStorage.getItem(STORAGE_KEY) || "";

  var $messages = document.getElementById("messages");
  var $form     = document.getElementById("prompt-form");
  var $prompt   = document.getElementById("prompt");
  var $send     = document.getElementById("send");
  var $newChat  = document.getElementById("new-chat");

  // ── Cross-tab signalling for OAuth completion ────────────────────────────
  // The OAuth success HTML broadcasts on "dicode-secrets" when a secret is
  // stored. Tabs with a pending setup card register their retry callback
  // under the expected secret name; the handler below fires it.
  var pendingSetups = {}; // secretName → retryFn
  try {
    var _ch = new BroadcastChannel("dicode-secrets");
    _ch.onmessage = function (ev) { handleSecretsStored(ev.data); };
  } catch (_) { /* BroadcastChannel unsupported — user can retry manually */ }
  // Fallback for browsers/configs where window.opener is set on the OAuth tab.
  window.addEventListener("message", function (ev) { handleSecretsStored(ev.data); });
  function handleSecretsStored(d) {
    if (!d || d.type !== "stored" || !Array.isArray(d.keys)) return;
    d.keys.forEach(function (k) {
      var fn = pendingSetups[k];
      if (fn) {
        delete pendingSetups[k];
        fn();
      }
    });
  }

  function addBubble(role, text) {
    var el = document.createElement("div");
    el.className = "bubble " + role;
    // textContent only — never innerHTML. Model output is untrusted.
    el.textContent = text;
    $messages.appendChild(el);
    el.scrollIntoView({ block: "end", behavior: "smooth" });
    return el;
  }

  // Strip ANSI SGR escape sequences (\u001b[...m) from terminal output.
  var ANSI_RE = /\u001b\[[\d;]*m/g;
  function stripAnsi(s) { return String(s).replace(ANSI_RE, ""); }

  // Splits on URLs using a capturing group so `split` returns alternating
  // [text, url, text, url, …] chunks. Strict trailing-punct exclusion keeps
  // ")" or "." out of the match. Caller uses textContent / createElement
  // — no innerHTML anywhere, so no XSS surface on the URL itself.
  var URL_SPLIT_RE = /(https?:\/\/[^\s<>"'()]+)/g;
  function appendTextWithLinks(parent, text) {
    var parts = String(text).split(URL_SPLIT_RE);
    for (var i = 0; i < parts.length; i++) {
      var chunk = parts[i];
      if (i % 2 === 1) {
        var a = document.createElement("a");
        a.href = chunk;
        a.target = "_blank";
        a.rel = "noopener noreferrer";
        a.textContent = chunk;
        parent.appendChild(a);
      } else if (chunk) {
        parent.appendChild(document.createTextNode(chunk));
      }
    }
  }

  // Inspect a failure body for the engine's if_missing marker. If present,
  // also pull the authorize URL out of the replayed prereq logs. Returns
  // { taskId, providerName, secret, authUrl } or null.
  function detectSetup(body) {
    var data;
    try { data = JSON.parse(body); } catch (_) { return null; }
    if (!data || typeof data !== "object") return null;

    var errStr = typeof data.error === "string" ? data.error : "";
    // Engine format: if_missing: secret "X" requires setup via task "Y": …
    var m = errStr.match(/if_missing: secret "([^"]+)" requires setup via task "([^"]+)"/);
    if (!m) return null;
    var secret = m[1];
    var taskId = m[2];

    var logBlob = Array.isArray(data.logs) ? data.logs.join("\n") : "";
    var urlMatch = stripAnsi(logBlob).match(/https?:\/\/[^\s]+/);
    if (!urlMatch) return null;

    // Title-case a provider display name from the task id.
    //   auth/openrouter-oauth → OpenRouter
    //   auth/google-oauth     → Google
    var slug = taskId.replace(/^auth\//, "").replace(/-oauth$/, "");
    var providerName = slug.charAt(0).toUpperCase() + slug.slice(1);

    return {
      taskId: taskId,
      secret: secret,
      providerName: providerName,
      authUrl: urlMatch[0],
    };
  }

  function renderSetupCard(el, info, retryFn) {
    el.className = "bubble setup";
    el.textContent = "";

    var $title = document.createElement("div");
    $title.className = "setup-title";
    $title.textContent = "Set up " + info.providerName;
    el.appendChild($title);

    var $desc = document.createElement("div");
    $desc.className = "setup-desc";
    $desc.textContent =
      "Authorize " + info.providerName +
      " in your browser to continue \u2014 we'll pick up where you left off once it's done.";
    el.appendChild($desc);

    var $btn = document.createElement("a");
    $btn.className = "setup-btn";
    $btn.href = info.authUrl;
    $btn.target = "_blank";
    $btn.rel = "opener noreferrer";
    $btn.textContent = "Authorize " + info.providerName + " \u2192";
    el.appendChild($btn);

    var $status = document.createElement("div");
    $status.className = "setup-status";
    el.appendChild($status);

    // Register the retry so the BroadcastChannel handler can fire it the
    // moment the secret is stored. Last registration for this secret wins —
    // if the user kicks off multiple setup attempts we only retry once.
    pendingSetups[info.secret] = function () {
      $status.textContent = "\u2713 Authorized \u2014 retrying\u2026";
      $status.className = "setup-status ok";
      setTimeout(retryFn, 300);
    };

    $btn.addEventListener("click", function () {
      $status.textContent =
        "Waiting for authorization\u2026 complete the flow in the new tab.";
      $status.className = "setup-status waiting";
    });

    el.scrollIntoView({ block: "end", behavior: "smooth" });
  }

  function renderError(el, body) {
    el.className = "bubble error";
    el.textContent = "";

    var title = "Task failed";
    var logs = null;
    try {
      var data = JSON.parse(body);
      if (data && typeof data === "object") {
        if (data.error) title = String(data.error);
        if (Array.isArray(data.logs) && data.logs.length) logs = data.logs;
      }
    } catch (_) { /* not JSON — use raw body as log */ }

    var $title = document.createElement("div");
    $title.className = "err-title";
    $title.textContent = title;
    el.appendChild($title);

    var $log = document.createElement("pre");
    $log.className = "err-log";
    appendTextWithLinks($log, stripAnsi(logs ? logs.join("\n") : (body || "")));
    el.appendChild($log);

    el.scrollIntoView({ block: "end", behavior: "smooth" });
  }

  function clearMessages() {
    while ($messages.firstChild) {
      $messages.removeChild($messages.firstChild);
    }
  }

  function send(text) {
    addBubble("user", text);
    var pending = addBubble("assistant", "\u2026");
    $send.disabled = true;

    var callParams = { prompt: text };
    if (sessionId) callParams.session_id = sessionId;

    dicode.execute(callParams).then(function (result) {
      var payload = result.returnValue;
      // Defensive: if dicode.js hasn't been fixed yet, returnValue is the
      // raw JSON string; parse it here as a fallback.
      if (typeof payload === "string") {
        try { payload = JSON.parse(payload); } catch (_) { payload = null; }
      }

      if (result.status !== "success" || !payload) {
        var body = result.body || "unknown error";
        var setup = detectSetup(body);
        if (setup) {
          renderSetupCard(pending, setup, function () {
            pending.className = "bubble assistant";
            pending.textContent = "\u2026";
            send(text);
          });
        } else {
          renderError(pending, body);
        }
        return;
      }

      if (!sessionId && payload.session_id) {
        sessionId = payload.session_id;
        localStorage.setItem(STORAGE_KEY, sessionId);
      }

      pending.textContent = payload.reply || "(no reply)";
    }).catch(function (e) {
      renderError(pending, "Connection error \u2014 is dicode running? (" + e.message + ")");
    }).then(function () {
      $send.disabled = false;
      $prompt.value = "";
      $prompt.focus();
    });
  }

  $form.addEventListener("submit", function (e) {
    e.preventDefault();
    var text = $prompt.value.trim();
    if (!text) return;
    send(text);
  });

  $newChat.addEventListener("click", function () {
    sessionId = "";
    localStorage.removeItem(STORAGE_KEY);
    clearMessages();
    $prompt.focus();
  });

  // Ctrl/Cmd+Enter to send
  $prompt.addEventListener("keydown", function (e) {
    if ((e.ctrlKey || e.metaKey) && e.key === "Enter") {
      e.preventDefault();
      $form.requestSubmit();
    }
  });

  $prompt.focus();
})();
