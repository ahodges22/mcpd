"use strict";

// setText is the only place a value enters the DOM. Tool results and error text
// come from third-party servers, and same-origin script could drive the very
// mutation routes the origin guard protects, so nothing is ever inserted as markup.
function setText(el, text) {
  el.textContent = text;
}

async function post(path, payload) {
  const res = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload || {}),
  });
  const text = await res.text();
  let parsed = null;
  try {
    parsed = JSON.parse(text);
  } catch (err) {
    parsed = null;
  }
  return { ok: res.ok, status: res.status, text: text, body: parsed };
}

function pretty(res) {
  return res.body ? JSON.stringify(res.body, null, 2) : res.text;
}

// isLoopback keeps the check below usable against a test provider on 127.0.0.1,
// which is the only case where a plain-HTTP authorization endpoint is legitimate.
function isLoopback(host) {
  const name = host.replace(/^\[/, "").replace(/\]$/, "");
  return name === "localhost" || name === "::1" || /^127\.\d+\.\d+\.\d+$/.test(name);
}

// safeAuthorizeURL returns raw only if navigating to it is safe. The authorization
// URL is provider-derived data entering a navigation context, which is the one place
// textContent does not protect: a javascript: URL from a hostile or compromised
// authorization server would run as same-origin script and could drive the very
// mutation routes the guard exists to protect.
function safeAuthorizeURL(raw) {
  let target;
  try {
    target = new URL(raw);
  } catch (err) {
    return null;
  }
  if (target.protocol === "https:" || (target.protocol === "http:" && isLoopback(target.hostname))) {
    return target.href;
  }
  return null;
}

for (const btn of document.querySelectorAll("button.act")) {
  btn.addEventListener("click", async () => {
    const note = document.getElementById("note");
    const reason = btn.dataset.confirm;
    // A misclick guard only, exactly as the inspector's is: the request is subject to
    // the origin, method and host guards whether or not this ran.
    if (reason && !window.confirm("This will " + reason + ". Continue?")) {
      return;
    }
    btn.disabled = true;
    if (note) {
      setText(note, "working...");
    }
    const res = await post(btn.dataset.post);
    btn.disabled = false;
    const offered = res.ok && res.body ? res.body.authorize_url : null;
    if (offered) {
      // Navigating this tab rather than opening one: window.open after an await can be
      // suppressed as a popup, and a rejected URL must be reported rather than dropped.
      const target = safeAuthorizeURL(offered);
      if (target === null) {
        if (note) {
          setText(note, "refusing to open the authorization URL: it is neither https nor loopback http");
        }
        return;
      }
      location.assign(target);
      return;
    }
    if (res.ok) {
      // A warning means the operation committed and something after it did not, so it
      // is shown rather than reloaded past.
      const warnings = res.body ? res.body.warnings : null;
      if (warnings && warnings.length > 0 && note) {
        setText(note, warnings.join("; "));
        return;
      }
      location.reload();
      return;
    }
    if (note) {
      setText(note, pretty(res));
    }
  });
}

// The add form. Nothing here is prefilled from an existing declaration: a declaration can
// carry an inline credential, so an edit form would have to send one back to the browser.
// Remove and re-add is the supported path instead.
const addSubmit = document.getElementById("add-submit");
if (addSubmit) {
  const value = (id) => document.getElementById(id).value.trim();
  const applyTransport = () => {
    const http = value("add-transport") === "http";
    for (const el of document.querySelectorAll(".stdio-only")) {
      el.hidden = http;
    }
    for (const el of document.querySelectorAll(".http-only")) {
      el.hidden = !http;
    }
  };
  document.getElementById("add-transport").addEventListener("change", applyTransport);
  applyTransport();

  addSubmit.addEventListener("click", async () => {
    const note = document.getElementById("note");
    const spec = {};
    if (value("add-transport") === "http") {
      spec.http_url = value("add-url");
      if (document.getElementById("add-oauth").checked) {
        spec.auth = "oauth";
      }
    } else {
      spec.command = value("add-command");
      const args = value("add-args");
      if (args) {
        spec.args = args.split(/\s+/);
      }
    }
    const timeout = parseInt(value("add-timeout"), 10);
    if (!isNaN(timeout)) {
      spec.timeout = timeout;
    }
    addSubmit.disabled = true;
    const res = await post("/api/backends", { name: value("add-name"), spec: spec });
    addSubmit.disabled = false;
    if (res.ok) {
      location.reload();
      return;
    }
    if (note) {
      setText(note, pretty(res));
    }
  });
}

for (const section of document.querySelectorAll("section.tool")) {
  const out = section.querySelector("pre.result");
  const btn = section.querySelector("button.invoke");
  btn.addEventListener("click", async () => {
    let args;
    try {
      args = JSON.parse(section.querySelector("textarea.args").value || "{}");
    } catch (err) {
      setText(out, "the arguments are not valid JSON: " + err.message);
      return;
    }
    const reason = section.dataset.confirm;
    // A misclick guard only: the request is subject to the origin and method guards
    // whether or not this ran, and reaching /api/invoke directly skips it entirely.
    if (reason && !window.confirm("This tool is " + reason + ". Invoke it?")) {
      return;
    }
    btn.disabled = true;
    setText(out, "invoking...");
    const res = await post("/api/invoke", { id: section.dataset.id, arguments: args });
    btn.disabled = false;
    setText(out, pretty(res));
  });
}
