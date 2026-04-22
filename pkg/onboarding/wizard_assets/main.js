(async () => {
  const presets = await fetch("/setup/presets").then((r) => r.json());
  const defaults = await fetch("/setup/defaults").then((r) => r.json());

  const host = document.getElementById("tasksets");
  for (const p of presets) {
    const label = document.createElement("label");
    label.className = "preset";

    const checkbox = document.createElement("input");
    checkbox.type = "checkbox";
    checkbox.id = "ts-" + p.name;
    checkbox.name = "tasksets";
    checkbox.value = p.name;
    checkbox.checked = !!p.default_on;

    const text = document.createElement("span");
    const strong = document.createElement("strong");
    strong.textContent = p.label;
    const small = document.createElement("small");
    small.textContent = p.desc;
    text.append(strong, small);

    label.append(checkbox, text);
    host.appendChild(label);
  }

  document.getElementById("local").value = defaults.local_tasks_dir || "";
  document.getElementById("ddir").value = defaults.data_dir || "";
  document.getElementById("port").value = String(defaults.port || 8080);

  document.getElementById("f").addEventListener("submit", async (ev) => {
    ev.preventDefault();
    const form = ev.currentTarget;
    const enabled = {};
    form.querySelectorAll('input[name="tasksets"]:checked').forEach((c) => {
      enabled[c.value] = true;
    });
    for (const p of presets) {
      if (!(p.name in enabled)) enabled[p.name] = false;
    }

    const payload = {
      tasksets: enabled,
      local_tasks_dir: form.elements.local_tasks_dir.value.trim(),
      data_dir: form.elements.data_dir.value.trim(),
      port: Number(form.elements.port.value) || 8080,
    };

    const pin = form.elements.pin.value.trim();
    const pinErr = document.getElementById("pinErr");
    pinErr.style.display = "none";

    const res = await fetch("/setup/apply", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-Setup-Pin": pin,
      },
      body: JSON.stringify(payload),
    });
    if (res.status === 403) {
      pinErr.textContent = "Incorrect PIN. Check the daemon's terminal and try again.";
      pinErr.style.display = "block";
      return;
    }
    if (res.status === 423) {
      pinErr.textContent = "Session locked — too many wrong PINs. Restart the dicode daemon to get a new PIN.";
      pinErr.style.display = "block";
      form.elements.pin.disabled = true;
      return;
    }
    if (!res.ok) {
      alert("setup failed: " + res.status + " " + (await res.text()));
      return;
    }
    const { passphrase } = await res.json();

    form.hidden = true;
    const done = document.getElementById("done");
    done.hidden = false;
    document.getElementById("pass").textContent = passphrase;
    // Build the dashboard URL from an explicitly-clamped integer port so
    // it can never be a script URL or carry meta-characters.
    const safePort = Math.max(1, Math.min(65535, Math.trunc(Number(payload.port)) || 8080));
    const dashUrl = "http://localhost:" + safePort;
    const dash = document.getElementById("dashUrl");
    dash.textContent = dashUrl;
    dash.href = dashUrl;
  });
})();
