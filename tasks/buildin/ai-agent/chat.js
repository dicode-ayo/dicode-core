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
  //
  // `Object.create(null)` yields a prototype-less map so attacker-controlled
  // keys like "__proto__" or "constructor" can't dispatch to inherited
  // methods — addresses the CodeQL "Unvalidated dynamic method call" finding.
  var pendingSetups = Object.create(null); // secretName → retryFn
  var SETUP_TTL_MS = 15 * 60 * 1000;       // abandon stale registrations after 15 min

  // BroadcastChannel is inherently same-origin, so no origin check needed.
  try {
    var _ch = new BroadcastChannel("dicode-secrets");
    _ch.onmessage = function (ev) { handleSecretsStored(ev.data); };
  } catch (_) { /* BroadcastChannel unsupported — user can retry manually */ }
  // window.postMessage fallback for window.opener-based delivery. ALWAYS check
  // event.origin — without it, any cross-origin popup or iframe could fake a
  // completion and trigger an unwanted retry.
  window.addEventListener("message", function (ev) {
    if (ev.origin !== window.location.origin) return;
    handleSecretsStored(ev.data);
  });
  function handleSecretsStored(d) {
    if (!d || d.type !== "stored" || !Array.isArray(d.keys)) return;
    d.keys.forEach(function (k) {
      if (typeof k !== "string") return;
      // hasOwn check is belt-and-suspenders with Object.create(null), but
      // makes the intent explicit: only fire retries we registered ourselves.
      if (!Object.prototype.hasOwnProperty.call(pendingSetups, k)) return;
      var entry = pendingSetups[k];
      delete pendingSetups[k];
      if (entry && typeof entry.fn === "function") {
        if (entry.ttlTimer) clearTimeout(entry.ttlTimer);
        entry.fn();
      }
    });
  }

  function clearPendingSetups() {
    Object.keys(pendingSetups).forEach(function (k) {
      var entry = pendingSetups[k];
      if (entry && entry.ttlTimer) clearTimeout(entry.ttlTimer);
      delete pendingSetups[k];
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

  // Known-provider display names — naive title-casing yields "Openrouter" or
  // "Github" which look wrong. Fall through to the naive form for unknown
  // providers so new tasks still render without requiring a code change here.
  var PROVIDER_DISPLAY_NAMES = {
    openrouter: "OpenRouter",
    github:     "GitHub",
    gitlab:     "GitLab",
    google:     "Google",
    slack:      "Slack",
    spotify:    "Spotify",
    linear:     "Linear",
    discord:    "Discord",
    notion:     "Notion",
    airtable:   "Airtable",
    confluence: "Atlassian",
    salesforce: "Salesforce",
    stripe:     "Stripe",
    azure:      "Azure",
    office365:  "Office 365",
    looker:     "Looker",
  };

  // Inspect a failure body for the engine's if_missing marker. If present,
  // also pull the authorize URL out of the replayed prereq logs. Returns
  // { taskId, providerName, secret, authUrl } or null.
  //
  // NOTE: the error prefix matched below is a stable contract with the
  // engine — see pkg/trigger/engine.go resolveIfMissing. Both sides must
  // move together if the format ever changes.
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

    var slug = taskId.replace(/^auth\//, "").replace(/-oauth$/, "");
    var providerName = PROVIDER_DISPLAY_NAMES[slug] ||
      (slug.charAt(0).toUpperCase() + slug.slice(1));

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
    // A TTL guards against abandoned flows leaking closures over detached
    // DOM nodes; if the user never completes auth we drop the registration.
    var prev = pendingSetups[info.secret];
    if (prev && prev.ttlTimer) clearTimeout(prev.ttlTimer);
    pendingSetups[info.secret] = {
      fn: function () {
        $status.textContent = "\u2713 Authorized \u2014 retrying\u2026";
        $status.className = "setup-status ok";
        setTimeout(retryFn, 300);
      },
      ttlTimer: setTimeout(function () {
        delete pendingSetups[info.secret];
      }, SETUP_TTL_MS),
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

    // Prefer the logs array (has full context, including replayed prereq
    // lines). Fall back to the error string, then to the raw body for
    // non-JSON responses (connection errors, malformed replies, etc.).
    var text = null;
    try {
      var data = JSON.parse(body);
      if (data && typeof data === "object") {
        if (Array.isArray(data.logs) && data.logs.length) {
          text = data.logs.join("\n");
        } else if (typeof data.error === "string") {
          text = data.error;
        }
      }
    } catch (_) { /* not JSON */ }
    if (text == null) text = body || "unknown error";

    var $log = document.createElement("pre");
    $log.className = "err-log";
    appendTextWithLinks($log, stripAnsi(text));
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
    clearPendingSetups();
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
