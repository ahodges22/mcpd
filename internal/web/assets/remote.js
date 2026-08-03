"use strict";

// The remote page builds its DOM through createElement and textContent only,
// for the same reason app.js does: nothing backend-derived may enter as markup.
function setText(el, text) {
  el.textContent = text;
}

function say(text) {
  const note = document.getElementById("note");
  if (!note) {
    return;
  }
  setText(note, text);
  note.hidden = text === "";
}

async function postJSON(path, payload) {
  const res = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload || {}),
  });
  let parsed = null;
  try {
    parsed = JSON.parse(await res.text());
  } catch {
    parsed = null;
  }
  return { ok: res.ok, body: parsed };
}

// safeAuthorizeURL admits only an http(s) URL, so a compromised response could
// not hand the browser a javascript: target.
function safeAuthorizeURL(raw) {
  try {
    const u = new URL(raw);
    if (u.protocol === "http:" || u.protocol === "https:") {
      return u.href;
    }
  } catch {}
  return "";
}

async function authorize(name) {
  say("Starting authorization for " + name + "...");
  const res = await postJSON("/api/backends/" + encodeURIComponent(name) + "/authorize");
  if (!res.body) {
    say("The authorize request failed.");
    return;
  }
  if (res.body.authorize_url) {
    const target = safeAuthorizeURL(res.body.authorize_url);
    if (target === "") {
      say("The provider returned an unusable authorization URL.");
      return;
    }
    window.location.assign(target);
    return;
  }
  say(res.body.message || res.body.error || res.body.status || "No authorization is pending.");
}

function renderBackends(list) {
  const ul = document.getElementById("remote-backends");
  ul.replaceChildren();
  if (list.length === 0) {
    const li = document.createElement("li");
    setText(li, "No OAuth-backed backends are declared.");
    ul.appendChild(li);
    return;
  }
  for (const b of list) {
    const li = document.createElement("li");
    const label = document.createElement("span");
    setText(label, b.name + " - " + (b.needs_auth ? "waiting for you to authorize it" : b.state));
    li.appendChild(label);
    const btn = document.createElement("button");
    btn.className = "act";
    setText(btn, "Authorize");
    btn.addEventListener("click", () => authorize(b.name));
    li.appendChild(btn);
    ul.appendChild(li);
  }
}

async function refresh() {
  try {
    const res = await fetch("/api/status");
    if (!res.ok) {
      return;
    }
    renderBackends(await res.json());
  } catch {}
}

document.getElementById("paste-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const input = document.getElementById("paste-url");
  const res = await postJSON("/api/callback", { url: input.value });
  if (res.ok) {
    say("Authorization delivered. The backend reconnects on its own.");
    input.value = "";
  } else {
    say((res.body && res.body.error) || "Delivery failed.");
  }
  refresh();
});

refresh();
setInterval(refresh, 2000);
