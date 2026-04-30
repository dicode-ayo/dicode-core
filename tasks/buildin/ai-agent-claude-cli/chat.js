(function () {
  'use strict';

  // Storage key is task-scoped so this preset's session_id doesn't collide
  // with the OpenAI-compatible buildin/ai-agent's session at /hooks/ai.
  var STORAGE_KEY = "dicode-ai-claude-session";
  var sessionId = localStorage.getItem(STORAGE_KEY) || "";

  var $messages = document.getElementById("messages");
  var $form     = document.getElementById("prompt-form");
  var $prompt   = document.getElementById("prompt");
  var $send     = document.getElementById("send");
  var $newChat  = document.getElementById("new-chat");
  var $status   = document.getElementById("status");
  var $setup    = document.getElementById("setup");
  var $setupDetail = document.getElementById("setup-detail");

  function setStatus(text) {
    $status.textContent = text || "";
  }

  function showSetup(detail) {
    $setup.hidden = false;
    if (detail) $setupDetail.textContent = detail;
  }

  function addBubble(role, text) {
    var el = document.createElement("div");
    el.className = "bubble " + role;
    appendTextWithLinks(el, text);
    $messages.appendChild(el);
    el.scrollIntoView({ block: "end", behavior: "smooth" });
    return el;
  }

  // textContent / createElement only — never innerHTML. Model output is
  // untrusted; URL detection still has to escape via document fragments.
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

  $newChat.addEventListener("click", function () {
    sessionId = "";
    localStorage.removeItem(STORAGE_KEY);
    while ($messages.firstChild) $messages.removeChild($messages.firstChild);
    setStatus("");
    $prompt.focus();
  });

  $form.addEventListener("submit", function (ev) {
    ev.preventDefault();
    send();
  });

  // Submit on Enter, newline on Shift+Enter — matches the OpenAI-compatible
  // chat preset for muscle-memory consistency.
  $prompt.addEventListener("keydown", function (ev) {
    if (ev.key === "Enter" && !ev.shiftKey) {
      ev.preventDefault();
      send();
    }
  });

  async function send() {
    var prompt = $prompt.value.trim();
    if (!prompt) return;

    addBubble("user", prompt);
    $prompt.value = "";
    $prompt.disabled = true;
    $send.disabled = true;
    setStatus("Thinking…");

    var thinking = addBubble("assistant pending", "…");

    try {
      var res = await fetch(window.location.pathname, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          prompt: prompt,
          session_id: sessionId,
        }),
      });

      var data;
      try {
        data = await res.json();
      } catch (_) {
        // Non-JSON response: treat as raw text.
        data = { ok: false, error: "non-JSON response from server" };
      }

      thinking.remove();

      if (!data || data.ok === false) {
        var msg = (data && data.error) || ("HTTP " + res.status);
        // Surface common setup errors as the dedicated setup card rather
        // than as a generic error bubble — the operator needs to act, not
        // the chatter.
        if (msg.indexOf("CLAUDE_CODE_OAUTH_TOKEN") !== -1) {
          showSetup("Server says: " + msg);
        } else if (msg.indexOf("claude binary not found") !== -1) {
          showSetup("Server says: " + msg);
        } else {
          addBubble("error", msg);
        }
        setStatus("");
        return;
      }

      addBubble("assistant", data.reply || "(empty reply)");
      if (data.session_id) {
        sessionId = data.session_id;
        localStorage.setItem(STORAGE_KEY, sessionId);
      }
      var modelLabel = data.model ? data.model : "claude";
      setStatus(modelLabel + (sessionId ? " · session " + sessionId.slice(0, 8) : ""));
    } catch (e) {
      thinking.remove();
      addBubble("error", "Network error: " + (e && e.message ? e.message : String(e)));
      setStatus("");
    } finally {
      $prompt.disabled = false;
      $send.disabled = false;
      $prompt.focus();
    }
  }

  $prompt.focus();
})();
