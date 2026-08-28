// grain's UI frontend. Plain DOM + fetch, no build step and no framework
// -- see v2/README.md's ui/ section for why: this whole package ships as
// one Go binary, and a frontend with its own npm toolchain would be the
// one part of it that isn't just `go build`.
//
// Every value on screen is re-fetched from the API, never cached across
// actions (see refreshList/openTask below) -- docs/data-model.md's UI
// direction: "it shows freshness for anything" read live from the store,
// rather than presenting a stale value as current.
"use strict";

let config = null;
let stateFilter = "all";

// model.State's own vocabulary, in model.StateOf's precedence order.
// This used to be a second, label-shaped set that had drifted from it:
// "needs_approval" for what the store calls proposed, plus an
// "untracked" the store has no notion of and no "closed" that it does.
const STATE_ORDER = ["proposed", "queued", "running", "awaiting_reply", "completed", "closed"];
const STATE_LABELS = {
  proposed: "Proposed",
  queued: "Queued",
  running: "Running",
  awaiting_reply: "Awaiting reply",
  completed: "Completed",
  closed: "Closed",
};

async function api(path, opts) {
  const res = await fetch(path, Object.assign({ headers: { "Content-Type": "application/json" } }, opts));
  const isJSON = (res.headers.get("Content-Type") || "").includes("application/json");
  const body = isJSON ? await res.json() : null;
  if (!res.ok) {
    throw new Error((body && body.error) || `${res.status} ${res.statusText}`);
  }
  return body;
}

function showError(err) {
  const banner = document.getElementById("error-banner");
  banner.textContent = String(err.message || err);
  banner.classList.remove("hidden");
  setTimeout(() => banner.classList.add("hidden"), 5000);
}

function el(tag, attrs, children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === "text") node.textContent = v;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v);
  }
  for (const child of children || []) {
    node.appendChild(typeof child === "string" ? document.createTextNode(child) : child);
  }
  return node;
}

function capabilityName(id) {
  const c = (config.capabilities || []).find((c) => c.id === id);
  return c ? c.name : id;
}

// --- list view ----------------------------------------------------------

async function refreshList() {
  const tasks = await api("/api/tasks");
  renderFilters(tasks);
  renderList(tasks);
}

function renderFilters(tasks) {
  const counts = {};
  for (const t of tasks) counts[t.state] = (counts[t.state] || 0) + 1;
  const nav = document.getElementById("state-filter");
  nav.innerHTML = "";
  const makeButton = (id, label) => {
    const b = el("button", {
      class: stateFilter === id ? "active" : "",
      onclick: () => { stateFilter = id; renderList(tasks); renderFilters(tasks); },
      text: label,
    });
    nav.appendChild(b);
  };
  makeButton("all", `All (${tasks.length})`);
  for (const s of STATE_ORDER) {
    if (!counts[s]) continue;
    makeButton(s, `${STATE_LABELS[s]} (${counts[s]})`);
  }
}

function renderList(tasks) {
  const list = document.getElementById("task-list");
  const empty = document.getElementById("empty-state");
  list.innerHTML = "";
  const visible = stateFilter === "all" ? tasks : tasks.filter((t) => t.state === stateFilter);
  empty.classList.toggle("hidden", visible.length > 0);
  for (const t of visible) {
    const chips = [];
    if (t.repo) chips.push(el("span", { class: "chip", text: t.repo }));
    for (const id of t.capabilities) chips.push(el("span", { class: "chip", text: capabilityName(id) }));
    list.appendChild(el("li", { onclick: () => openTask(t.id) }, [
      el("span", { class: "task-number", text: t.id }),
      el("span", { class: "task-title", text: t.title }),
      el("span", { class: "chips" }, chips),
      el("span", { class: `badge badge-${t.state}`, text: STATE_LABELS[t.state] || t.state }),
    ]));
  }
}

// --- detail view ---------------------------------------------------------

async function openTask(id) {
  try {
    const detail = await api(`/api/tasks/${id}`);
    renderDetail(detail);
    document.getElementById("detail-overlay").classList.remove("hidden");
  } catch (err) {
    showError(err);
  }
}

function renderDetail(t) {
  const container = document.getElementById("detail-body");
  container.innerHTML = "";

  container.appendChild(el("div", { class: "detail-header" }, [
    el("h2", { text: `${t.id} ${t.title}` }),
    el("span", { class: `badge badge-${t.state}`, text: STATE_LABELS[t.state] || t.state }),
  ]));
  const freshness = el("div", { class: "freshness", text: "as of just now" });
  if (t.pullRequest) {
    freshness.appendChild(document.createTextNode(" · "));
    freshness.appendChild(el("span", { text: t.pullRequest }));
  }
  container.appendChild(freshness);

  // Real columns on the task now, not directive lines parsed out of a
  // body -- so they are rendered as fields rather than as the /repo,
  // /base, /auto-merge syntax they used to have to be written in.
  const declaredParts = [];
  if (t.repo) declaredParts.push(`repo ${t.repo}`);
  if (t.base) declaredParts.push(`base ${t.base}`);
  declaredParts.push(`auto-merge ${t.autoMerge}`);
  container.appendChild(el("div", { class: "declared", text: declaredParts.join("  ") }));

  container.appendChild(el("div", { class: "description", text: t.description || "(no description)" }));

  const actions = el("div", { class: "actions" });
  if (t.state === "proposed") {
    actions.appendChild(el("button", { class: "primary", onclick: () => act(() => api(`/api/tasks/${t.id}/approve`, { method: "POST" }), t.id) }, ["Approve"]));
  }
  if (t.state === "closed") {
    actions.appendChild(el("button", { class: "secondary", onclick: () => act(() => api(`/api/tasks/${t.id}/reopen`, { method: "POST" }), t.id) }, ["Reopen"]));
  } else {
    actions.appendChild(el("button", { class: "danger secondary", onclick: () => act(() => api(`/api/tasks/${t.id}/close`, { method: "POST" }), t.id) }, ["Close"]));
  }
  container.appendChild(actions);

  container.appendChild(renderCapabilityToggles(t));
  container.appendChild(renderComments(t));
}

function renderCapabilityToggles(t) {
  const fs = el("fieldset", {}, [el("legend", { text: "Capabilities" })]);
  for (const c of config.capabilities || []) {
    const checked = t.capabilities.includes(c.id);
    const input = el("input", { type: "checkbox" });
    input.checked = checked;
    input.addEventListener("change", () => act(() => api(`/api/tasks/${t.id}/capabilities`, {
      method: "POST",
      body: JSON.stringify({ id: c.id, attach: input.checked }),
    }), t.id));
    fs.appendChild(el("label", { class: "checkbox", title: c.description }, [input, c.name]));
  }
  return fs;
}

function renderComments(t) {
  const wrap = el("div", { class: "comments" }, [el("h3", { text: "Conversation" })]);
  for (const c of t.comments || []) {
    // onBehalfOf is set when grain relayed somebody else's words -- a
    // question from a dispatched run reads as grain speaking for an
    // agent, not as grain's own.
    const who = c.onBehalfOf ? `${c.author} on behalf of ${c.onBehalfOf}` : c.author;
    wrap.appendChild(el("div", { class: "comment" }, [
      el("div", { class: "meta", text: `${who} · ${c.authorKind}` }),
      el("div", { text: c.body }),
    ]));
  }
  const textarea = el("textarea", { rows: "2", placeholder: "Reply..." });
  const send = el("button", { class: "secondary", onclick: async () => {
    if (!textarea.value.trim()) return;
    await act(() => api(`/api/tasks/${t.id}/comments`, { method: "POST", body: JSON.stringify({ body: textarea.value }) }), t.id);
  } }, ["Comment"]);
  wrap.appendChild(el("div", { class: "comment-form" }, [textarea, send]));
  return wrap;
}

// act runs a mutation, then re-fetches the task (and the list behind it)
// so the screen reflects what GitHub now reports -- never the value the
// UI optimistically assumed it wrote, per the freshness rule above.
async function act(mutate, id) {
  try {
    await mutate();
    await openTask(id);
    await refreshList();
  } catch (err) {
    showError(err);
  }
}

// --- new task form --------------------------------------------------------

function populateCapabilityFieldset() {
  const fs = document.getElementById("new-task-capabilities");
  fs.innerHTML = "<legend>Capabilities</legend>";
  for (const c of config.capabilities || []) {
    const input = el("input", { type: "checkbox", name: "cap-" + c.id });
    fs.appendChild(el("label", { class: "checkbox", title: c.description }, [input, c.name]));
  }
}

async function submitNewTask(evt) {
  evt.preventDefault();
  const form = evt.target;
  const data = new FormData(form);
  const capabilities = (config.capabilities || [])
    .filter((c) => form.elements["cap-" + c.id] && form.elements["cap-" + c.id].checked)
    .map((c) => c.id);
  const payload = {
    title: data.get("title"),
    description: data.get("description") || "",
    repo: data.get("repo") || "",
    base: data.get("base") || "",
    autoMerge: form.elements.autoMerge.checked,
    capabilities,
    approved: form.elements.approved.checked,
  };
  try {
    await api("/api/tasks", { method: "POST", body: JSON.stringify(payload) });
    form.reset();
    closeOverlay("new-task");
    await refreshList();
  } catch (err) {
    showError(err);
  }
}

// --- wiring ---------------------------------------------------------------

function closeOverlay(name) {
  document.getElementById(`${name}-overlay`).classList.add("hidden");
}

function wireOverlays() {
  document.querySelectorAll("[data-close]").forEach((btn) => {
    btn.addEventListener("click", () => closeOverlay(btn.dataset.close));
  });
  document.querySelectorAll(".overlay").forEach((overlay) => {
    overlay.addEventListener("click", (evt) => {
      if (evt.target === overlay) overlay.classList.add("hidden");
    });
  });
}

async function main() {
  wireOverlays();
  document.getElementById("new-task-form").addEventListener("submit", submitNewTask);
  document.getElementById("new-task-button").addEventListener("click", () => {
    document.getElementById("new-task-overlay").classList.remove("hidden");
  });

  try {
    config = await api("/api/config");
    const target = config.defaultTarget;
    document.getElementById("repo-name").textContent =
      target ? `${target.Owner}/${target.Name}` : `as ${config.actor.ID}`;
    populateCapabilityFieldset();
    await refreshList();
  } catch (err) {
    showError(err);
  }
}

main();
