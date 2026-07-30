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
