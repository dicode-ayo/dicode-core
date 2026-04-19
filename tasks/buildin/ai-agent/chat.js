(function () {
  'use strict';

  var STORAGE_KEY = "dicode-ai-session";
  var sessionId = localStorage.getItem(STORAGE_KEY) || "";

  var $messages = document.getElementById("messages");
  var $form     = document.getElementById("prompt-form");
  var $prompt   = document.getElementById("prompt");
  var $send     = document.getElementById("send");
  var $newChat  = document.getElementById("new-chat");

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

  // Render a task-failure response into a bubble. Parses the structured
  // { error, logs, runId, status } body when present and cleans ANSI codes
  // from log lines. textContent only — no innerHTML.
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
    $log.textContent = stripAnsi(logs ? logs.join("\n") : (body || ""));
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
    var pending = addBubble("assistant", "…");
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
        renderError(pending, result.body || "unknown error");
        return;
      }

      if (!sessionId && payload.session_id) {
        sessionId = payload.session_id;
        localStorage.setItem(STORAGE_KEY, sessionId);
      }

      pending.textContent = payload.reply || "(no reply)";
    }).catch(function (e) {
      renderError(pending, "Connection error — is dicode running? (" + e.message + ")");
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
