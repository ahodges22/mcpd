"use strict";

// setText is the only place a value enters the DOM. Tool results and error text
// come from third-party servers, and same-origin script could drive the very
// mutation routes the origin guard protects, so nothing is ever inserted as markup.
function setText(el, text) {
  el.textContent = text;
}

// say puts a message in the one place the page reports them, and reveals it. Passing an
// empty string hides it again, so a stale failure never outlives the action that caused it.
function say(text) {
  const note = document.getElementById("note");
  if (!note) {
    return;
  }
  setText(note, text);
  note.hidden = text === "";
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

// reason prefers the daemon's own error text to a dump of the whole envelope, because the
// envelope's other fields tell the reader nothing they asked about.
function reason(res) {
  if (res.body && typeof res.body.error === "string") {
    return res.body.error;
  }
  return res.body ? JSON.stringify(res.body, null, 2) : res.text;
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

// The bus segments are sized from a server-computed weight rather than an inline style
// attribute, which the page's own content-security policy forbids.
for (const seg of document.querySelectorAll(".seg[data-grow]")) {
  seg.style.flexGrow = seg.dataset.grow;
}

for (const btn of document.querySelectorAll("button.act")) {
  btn.addEventListener("click", async () => {
    const confirmReason = btn.dataset.confirm;
    // A misclick guard only, exactly as the inspector's is: the request is subject to
    // the origin, method and host guards whether or not this ran.
    if (confirmReason && !window.confirm("This will " + confirmReason + ". Continue?")) {
      return;
    }
    const was = btn.textContent;
    btn.disabled = true;
    setText(btn, "Working");
    say("");
    const res = await post(btn.dataset.post);
    btn.disabled = false;
    setText(btn, was);
    const offered = res.ok && res.body ? res.body.authorize_url : null;
    if (offered) {
      // Navigating this tab rather than opening one: window.open after an await can be
      // suppressed as a popup, and a rejected URL must be reported rather than dropped.
      const target = safeAuthorizeURL(offered);
      if (target === null) {
        say("Refusing to open that authorization URL: it is neither https nor loopback http.");
        return;
      }
      location.assign(target);
      return;
    }
    if (res.ok) {
      // A warning means the operation committed and something after it did not, so it
      // is shown rather than reloaded past.
      const warnings = res.body ? res.body.warnings : null;
      if (warnings && warnings.length > 0) {
        say(warnings.join("\n"));
        return;
      }
      location.reload();
      return;
    }
    say(reason(res));
  });
}

// A disclosure that keeps the add form out of the way until it is wanted, without
// hiding that it exists.
for (const btn of document.querySelectorAll("button[data-toggle]")) {
  const panel = document.getElementById(btn.dataset.toggle);
  if (!panel) {
    continue;
  }
  btn.addEventListener("click", () => {
    panel.hidden = !panel.hidden;
    btn.setAttribute("aria-expanded", String(!panel.hidden));
    if (!panel.hidden) {
      const first = panel.querySelector("input");
      if (first) {
        first.focus();
      }
    }
  });
}

// The add form. Nothing here is prefilled from an existing declaration: a declaration can
// carry an inline credential, so an edit form would have to send one back to the browser.
// Remove and re-add is the supported path instead.
const addSubmit = document.getElementById("add-submit");
if (addSubmit) {
  const value = (id) => document.getElementById(id).value.trim();
  const transport = () => document.querySelector('input[name="add-transport"]:checked').value;
  const applyTransport = () => {
    const http = transport() === "http";
    for (const el of document.querySelectorAll(".stdio-only")) {
      el.hidden = http;
    }
    for (const el of document.querySelectorAll(".http-only")) {
      el.hidden = !http;
    }
  };
  for (const radio of document.querySelectorAll('input[name="add-transport"]')) {
    radio.addEventListener("change", applyTransport);
  }
  applyTransport();

  addSubmit.addEventListener("click", async () => {
    const spec = {};
    if (transport() === "http") {
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
    say("");
    const res = await post("/api/backends", { name: value("add-name"), spec: spec });
    addSubmit.disabled = false;
    if (res.ok) {
      location.reload();
      return;
    }
    say(reason(res));
  });
}

// The inspector's filter. A backend can serve several hundred tools, so finding one by
// scrolling is not finding it.
const filter = document.getElementById("tool-filter");
if (filter) {
  const tools = document.querySelectorAll("section.tool");
  const noMatch = document.getElementById("no-match");
  filter.addEventListener("input", () => {
    const needle = filter.value.trim().toLowerCase();
    let shown = 0;
    for (const tool of tools) {
      const hit = needle === "" || tool.dataset.find.toLowerCase().includes(needle);
      tool.hidden = !hit;
      if (hit) {
        shown++;
      }
    }
    if (noMatch) {
      noMatch.hidden = shown > 0;
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
      setText(out, "Those arguments are not valid JSON: " + err.message);
      return;
    }
    const reasonToConfirm = section.dataset.confirm;
    // A misclick guard only: the request is subject to the origin and method guards
    // whether or not this ran, and reaching /api/invoke directly skips it entirely.
    if (reasonToConfirm && !window.confirm("This tool is " + reasonToConfirm + ". Run it?")) {
      return;
    }
    btn.disabled = true;
    setText(out, "Running...");
    const res = await post("/api/invoke", { id: section.dataset.id, arguments: args });
    btn.disabled = false;
    setText(out, pretty(res));
  });
}
