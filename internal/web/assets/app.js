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

for (const btn of document.querySelectorAll("button.act")) {
  btn.addEventListener("click", async () => {
    const note = document.getElementById("note");
    btn.disabled = true;
    if (note) {
      setText(note, "working...");
    }
    const res = await post(btn.dataset.post);
    btn.disabled = false;
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
