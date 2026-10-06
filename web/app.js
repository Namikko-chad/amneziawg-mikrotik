"use strict";

const $ = (s) => document.querySelector(s);

async function api(path, body, method) {
  const opts = body === undefined ? { method: method || "GET" } : {
    method: method || "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  };
  const res = await fetch("/api/" + path, opts);
  let data = {};
  try { data = await res.json(); } catch { /* empty body */ }
  if (!res.ok) throw new Error(data.error || res.status + " " + res.statusText);
  return data;
}

let toastTimer;
function toast(msg, isError) {
  const t = $("#toast");
  t.textContent = msg;
  t.className = isError ? "error" : "";
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.hidden = true), isError ? 8000 : 3000);
}

function fmtBytes(n) {
  const u = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return n.toFixed(i ? 1 : 0) + " " + u[i];
}

function fmtAgo(ts) {
  if (!ts) return "never";
  const s = Math.max(0, Math.round(Date.now() / 1000 - ts));
  if (s < 60) return s + "s ago";
  if (s < 3600) return Math.floor(s / 60) + "m ago";
  return Math.floor(s / 3600) + "h ago";
}

// [text, class] describing the tunnel state.
function tunnelState(st) {
  const peer = (st.peers || [])[0];
  const fresh = peer && peer.last_handshake && Date.now() / 1000 - peer.last_handshake < 180;
  if (!st.configured) return ["no config", ""];
  if (!st.running) return ["disconnected", "bad"];
  if (fresh) return ["connected", "ok"];
  return ["no handshake", "warn"];
}

function renderStatus(st) {
  const peer = (st.peers || [])[0];
  const [text, cls] = tunnelState(st);
  const badge = $("#badge");
  badge.textContent = text;
  badge.className = "badge " + cls;
  lastStatus = st;
  renderServerState();

  $("#st-endpoint").textContent = (peer && peer.endpoint !== "(none)" && peer.endpoint) || st.endpoint || "—";
  $("#st-address").textContent = st.address || "—";
  $("#st-handshake").textContent = peer ? fmtAgo(peer.last_handshake) : "—";
  $("#st-traffic").textContent = peer ? "↓ " + fmtBytes(peer.rx_bytes) + "  ↑ " + fmtBytes(peer.tx_bytes) : "—";
  $("#btn-up").disabled = !st.configured;
  $("#btn-down").disabled = !st.running;
}

async function refresh() {
  try {
    renderStatus(await api("status"));
  } catch (e) {
    $("#badge").textContent = "backend unavailable";
    $("#badge").className = "badge bad";
  }
  if ($("#logs").open) refreshLogs();
}

// --- Servers list ---------------------------------------------------------

let servers = { active: "", profiles: [] };
let lastStatus = null;
let openId = null; // server whose settings panel is expanded

function hostOf(endpoint) {
  if (!endpoint) return "";
  const m = endpoint.match(/^\[(.+)\]:\d+$/) || endpoint.match(/^([^:]+):\d+$/);
  return m ? m[1] : endpoint;
}

async function refreshServers() {
  try {
    servers = await api("profiles");
  } catch (e) { toast(e.message, true); return; }
  renderServers();
}

function renderServers() {
  const list = $("#servers");
  list.replaceChildren();
  $("#servers-empty").hidden = servers.profiles.length > 0;
  for (const p of servers.profiles) {
    const li = $("#server-tpl").content.firstElementChild.cloneNode(true);
    const active = p.id === servers.active;
    li.dataset.id = p.id;
    li.classList.toggle("active", active);
    li.querySelector(".pick").setAttribute("aria-checked", active);
    li.querySelector(".server-name").textContent = p.name;
    li.querySelector(".server-sub").textContent = hostOf(p.endpoint) || p.address || "";
    li.querySelector(".pick").title = [p.endpoint, p.address, p.source && "added via " + p.source].filter(Boolean).join("\n");
    li.querySelector(".rename").name.value = p.name;

    li.querySelector(".pick").addEventListener("click", (e) => activate(e.currentTarget, p));
    li.querySelector(".gear").addEventListener("click", () => {
      openId = openId === p.id ? null : p.id;
      syncPanels();
    });
    li.querySelector(".rename").addEventListener("submit", (e) => {
      e.preventDefault();
      const name = e.currentTarget.name.value.trim();
      run(e.currentTarget.querySelector("button"), async () => {
        await api("profiles/" + p.id, { name }, "PATCH");
        await refreshServers();
      }, "Renamed");
    });
    li.querySelector(".server-conf").addEventListener("toggle", async (e) => {
      if (!e.target.open) return;
      const pre = e.target.querySelector("pre");
      try { pre.textContent = (await api("profiles/" + p.id + "/config")).config || "—"; }
      catch (err) { pre.textContent = err.message; }
    });
    li.querySelector(".delete").addEventListener("click", (e) => {
      const msg = active
        ? `Delete "${p.name}"? It is the active server: the tunnel will be stopped.`
        : `Delete "${p.name}"?`;
      if (!confirm(msg)) return;
      run(e.currentTarget, async () => {
        const st = await api("profiles/" + p.id, undefined, "DELETE");
        if (openId === p.id) openId = null;
        await refreshServers();
        return st;
      }, "Server deleted");
    });
    list.append(li);
  }
  syncPanels();
  renderServerState();
}

function syncPanels() {
  document.querySelectorAll("#servers .server").forEach((li) => {
    const open = li.dataset.id === openId;
    li.querySelector(".server-panel").hidden = !open;
    li.querySelector(".gear").setAttribute("aria-expanded", open);
  });
}

// Shows the tunnel state next to the active server.
function renderServerState() {
  document.querySelectorAll("#servers .server").forEach((li) => {
    const el = li.querySelector(".server-state");
    const active = li.dataset.id === servers.active;
    el.hidden = !active || !lastStatus;
    if (el.hidden) return;
    const [text, cls] = tunnelState(lastStatus);
    el.textContent = text;
    el.className = "server-state " + cls;
  });
}

function activate(btn, p) {
  if (p.id === servers.active) return;
  const reconnect = lastStatus && lastStatus.enabled;
  run(btn, async () => {
    try {
      return await api("profiles/" + p.id + "/activate", {});
    } finally {
      await refreshServers();
    }
  }, reconnect ? `Switched to ${p.name}` : `${p.name} selected`);
}

async function refreshLogs() {
  try {
    const { lines } = await api("logs");
    const v = $("#log-view");
    const atBottom = v.scrollTop + v.clientHeight >= v.scrollHeight - 4;
    v.textContent = (lines || []).join("\n");
    if (atBottom) v.scrollTop = v.scrollHeight;
  } catch { /* ignore */ }
}

// Disables the triggering button while the request runs.
async function run(btn, fn, okMsg) {
  btn.disabled = true;
  const label = btn.innerHTML;
  if (!btn.classList.contains("pick")) btn.textContent = "…";
  try {
    const st = await fn();
    if (st) renderStatus(st);
    toast(okMsg);
  } catch (e) {
    toast(e.message, true);
  } finally {
    btn.disabled = false;
    if (btn.isConnected) btn.innerHTML = label;
    refresh();
  }
}

// Waits for an add request, then clears the form and reloads the list. The list is reloaded
// even on failure: a config that saved but failed to start is still a new server.
async function addServer(form, req) {
  try {
    const st = await req;
    form.reset();
    return st;
  } finally {
    refreshServers();
  }
}

function formData(form) {
  const o = {};
  for (const [k, v] of new FormData(form)) if (typeof v === "string") o[k] = v.trim();
  return o;
}

// Tabs
document.querySelectorAll(".tabs button").forEach((tab) => {
  tab.addEventListener("click", () => {
    document.querySelectorAll(".tabs button").forEach((t) => t.setAttribute("aria-selected", t === tab));
    document.querySelectorAll(".tab").forEach((p) => (p.hidden = p.id !== "tab-" + tab.dataset.tab));
  });
});

$("#btn-up").addEventListener("click", (e) => run(e.currentTarget, () => api("up", {}), "Tunnel is up"));
$("#btn-down").addEventListener("click", (e) => run(e.currentTarget, () => api("down", {}), "Tunnel stopped"));

$("#tab-key").addEventListener("submit", (e) => {
  e.preventDefault();
  const f = e.currentTarget;
  run(f.querySelector("button[type=submit]"), () => addServer(f, api("config/vpnkey", formData(f))), "Server added");
});

$("#tab-conf").addEventListener("submit", (e) => {
  e.preventDefault();
  const f = e.currentTarget;
  const body = { config: f.config.value, name: f.name.value.trim() };
  run(f.querySelector("button[type=submit]"), () => addServer(f, api("config/text", body)), "Server added");
});

$("#conf-file").addEventListener("change", async (e) => {
  const file = e.target.files[0];
  if (file) $("#tab-conf").config.value = await file.text();
  e.target.value = "";
});

$("#tab-ssh").addEventListener("submit", (e) => {
  e.preventDefault();
  const f = e.currentTarget;
  const body = formData(f);
  body.port = parseInt(body.port, 10) || 22;
  if (!body.password && !body.private_key) { toast("Enter a password or a private key", true); return; }
  run(f.querySelector("button[type=submit]"), () => addServer(f, api("config/ssh", body)), "Client created, tunnel is up");
});

$("#logs").addEventListener("toggle", (e) => e.target.open && refreshLogs());

refreshServers();
refresh();
setInterval(refresh, 5000);
