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

// busy pauses the poll while an authorize is in flight, so the list rebuild
// cannot wipe the button's working state from under the user.
let busy = false;

async function authorize(name, btn) {
  say("Starting authorization for " + name + "...");
  const was = btn.textContent;
  busy = true;
  btn.disabled = true;
  setText(btn, "Working");
  const res = await postJSON("/api/backends/" + encodeURIComponent(name) + "/authorize");
  busy = false;
  btn.disabled = false;
  setText(btn, was);
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
    li.className = "pair-row";
    const name = document.createElement("span");
    name.className = "pair-state";
    setText(name, "No OAuth-backed backends are declared.");
    li.appendChild(name);
    ul.appendChild(li);
    return;
  }
  for (const b of list) {
    const li = document.createElement("li");
    li.className = "pair-row";
    const lamp = document.createElement("span");
    lamp.className = "lamp";
    lamp.dataset.tone = b.tone;
    li.appendChild(lamp);
    const name = document.createElement("span");
    name.className = "pair-name mono";
    setText(name, b.name);
    li.appendChild(name);
    const state = document.createElement("span");
    state.className = "pair-state";
    setText(state, b.label);
    li.appendChild(state);
    const btn = document.createElement("button");
    btn.className = "act";
    setText(btn, "Authorize");
    // A serving backend has nothing to authorize; the button stays visible
    // but inert so the row still reads as complete. Every other state keeps
    // it live, because authorize-after-reconnect is how a stale one is kicked.
    if (b.state === "up") {
      btn.disabled = true;
      btn.title = "Already authorized and serving";
    } else {
      btn.addEventListener("click", () => authorize(b.name, btn));
    }
    li.appendChild(btn);
    ul.appendChild(li);
  }
}

async function refresh() {
  if (busy) {
    return;
  }
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
