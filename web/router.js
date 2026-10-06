"use strict";

// Router card: edits the routing plan and applies it to the router over the RouterOS REST API.
// The router login lives only in sessionStorage (this tab) and is sent with each request.
(() => {
  const el = (s) => document.querySelector(s);
  const form = el("#rt-plan");
  const dlg = el("#rt-login");
  const loginForm = el("#rt-login-form");
  const AUTH_KEY = "awg-router-auth";

  let defaultURL = "";
  let info = null;
  let pending = null; // action to retry after logging in

  function notify(msg, isError) {
    if (typeof toast === "function") toast(msg, isError);
    else if (isError) alert(msg);
  }

  function getAuth() {
    try { return JSON.parse(sessionStorage.getItem(AUTH_KEY)); } catch { return null; }
  }
  function setAuth(a) {
    try {
      if (a) sessionStorage.setItem(AUTH_KEY, JSON.stringify(a));
      else sessionStorage.removeItem(AUTH_KEY);
    } catch { /* storage unavailable: the login is asked again next time */ }
    memAuth = a;
  }
  let memAuth = getAuth();

  async function call(path, body) {
    const res = await fetch("/api/" + path, body === undefined ? {} : {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    let data = {};
    try { data = await res.json(); } catch { /* empty body */ }
    if (!res.ok) {
      const err = new Error(data.error || res.status + " " + res.statusText);
      err.status = res.status;
      throw err;
    }
    return data;
  }

  // Runs fn(auth). Without a login, or when the router rejects it, asks for one and retries.
  async function withAuth(fn) {
    if (!memAuth) { askLogin(fn); return; }
    try {
      return await fn(memAuth);
    } catch (e) {
      if (e.status === 401) {
        setAuth(null);
        renderInfo();
        askLogin(fn, e.message);
        return;
      }
      throw e;
    }
  }

  function askLogin(then, msg) {
    pending = then;
    const a = memAuth || {};
    loginForm.url.value = a.url || defaultURL;
    loginForm.user.value = a.user || loginForm.user.value || "awg";
    loginForm.password.value = "";
    loginForm.insecure.checked = !!a.insecure;
    if (msg) notify(msg, true);
    dlg.showModal();
    (loginForm.user.value ? loginForm.password : loginForm.user).focus();
  }

  loginForm.addEventListener("submit", async (e) => {
    e.preventDefault();
    const auth = {
      url: loginForm.url.value.trim(),
      user: loginForm.user.value.trim(),
      password: loginForm.password.value,
      insecure: loginForm.insecure.checked,
    };
    const btn = loginForm.querySelector("button[type=submit]");
    btn.disabled = true;
    try {
      info = await call("router/info", { auth });
    } catch (err) {
      notify(err.message, true);
      return;
    } finally {
      btn.disabled = false;
    }
    setAuth(auth);
    dlg.close();
    renderInfo();
    const next = pending;
    pending = null;
    if (next) runAction(next);
  });
  el("#rt-login-cancel").addEventListener("click", () => { pending = null; dlg.close(); });

  async function runAction(fn) {
    try { await withAuth(fn); } catch (e) { notify(e.message, true); }
  }

  el("#rt-connect").addEventListener("click", () => {
    if (memAuth) {
      setAuth(null);
      info = null;
      renderInfo();
      return;
    }
    askLogin(null);
  });

  function renderInfo() {
    const badge = el("#rt-badge");
    const warn = el("#rt-warn");
    el("#rt-connect").textContent = memAuth ? "Disconnect" : "Connect";
    warn.hidden = true;
    if (!memAuth || !info) {
      badge.textContent = "not connected";
      badge.className = "badge";
      el("#rt-info").textContent = "Connect to the router to route LAN traffic through the tunnel. The login is kept only in this browser tab.";
      el("#rt-nets").hidden = true;
      return;
    }
    const route = info.route_active == null ? "no tunnel route" : info.route_active ? "tunnel route active" : "tunnel route inactive";
    badge.textContent = info.route_active == null ? "connected" : route;
    badge.className = "badge " + (info.route_active ? "ok" : info.route_active == null ? "" : "warn");
    el("#rt-info").textContent = [info.identity, info.version && "RouterOS " + info.version, info.board,
      "container " + info.container + " in " + info.container_net].filter(Boolean).join(" · ");
    if (plan.mode === "off" && info.managed > 0) {
      warn.textContent = `The router has ${info.managed} awg routing objects (probably set up by hand). Applying a plan replaces them.`;
      warn.hidden = false;
    }
    const nets = el("#rt-nets");
    nets.replaceChildren();
    for (const n of info.networks || []) {
      const b = document.createElement("button");
      b.type = "button";
      b.className = "chip";
      b.textContent = n.network + " (" + n.interface + ")";
      b.title = "Add to LAN subnets";
      b.addEventListener("click", () => {
        const lines = splitList(form.lan.value);
        if (!lines.includes(n.network)) form.lan.value = lines.concat(n.network).join("\n");
      });
      nets.append(b);
    }
    nets.hidden = nets.children.length === 0;
  }

  // --- plan form -----------------------------------------------------------

  let plan = { mode: "off" };

  const splitList = (s) => s.split(/[\s,;]+/).map((x) => x.trim()).filter(Boolean);
  const showList = (a) => (a || []).map((x) => x.replace(/\/32$/, "")).join("\n");

  function fillForm(p) {
    plan = p;
    form.querySelectorAll("input[name=mode]").forEach((r) => (r.checked = r.value === (p.mode || "off")));
    for (const k of ["lan", "exclude", "exclude_dst", "devices", "sites"]) form[k].value = showList(p[k]);
    form.kill_switch.checked = !!p.kill_switch;
    form.paused.checked = !!p.paused;
    form.dns_on.checked = (p.dns || []).length > 0;
    form.dns.value = (p.dns || []).map((x) => x.replace(/\/32$/, "")).join(", ") || "1.1.1.1, 1.0.0.1";
    syncForm();
  }

  function readForm() {
    const p = { mode: form.querySelector("input[name=mode]:checked")?.value || "off" };
    for (const k of ["lan", "exclude", "exclude_dst", "devices", "sites"]) p[k] = splitList(form[k].value);
    p.kill_switch = form.kill_switch.checked;
    p.paused = form.paused.checked;
    p.dns = form.dns_on.checked ? splitList(form.dns.value) : [];
    return p;
  }

  function syncForm() {
    const mode = form.querySelector("input[name=mode]:checked")?.value || "off";
    form.querySelectorAll("[data-modes]").forEach((n) => (n.hidden = !n.dataset.modes.split(" ").includes(mode)));
    form.querySelector("[data-dns]").hidden = !form.dns_on.checked;
    if (mode !== "off" && !form.lan.value.trim() && info) {
      // Suggest the networks on the default LAN bridge.
      const lan = (info.networks || []).filter((n) => n.interface === "bridge").map((n) => n.network);
      form.lan.value = lan.join("\n");
    }
  }
  form.addEventListener("change", syncForm);

  form.addEventListener("submit", (e) => {
    e.preventDefault();
    const p = readForm();
    if (p.mode === "off" && !confirm("Remove all awg routing rules from the router?")) return;
    const btn = form.querySelector("button[type=submit]");
    runAction(async (auth) => {
      btn.disabled = true;
      try {
        const res = await call("router/apply", { auth, plan: p });
        info = res.info;
        fillForm(res.plan);
        renderInfo();
        notify(p.mode === "off" ? "Router rules removed" : "Applied to router");
      } finally {
        btn.disabled = false;
      }
    });
  });

  async function init() {
    try {
      const st = await call("router");
      defaultURL = st.default_url || "";
      fillForm(st.plan || { mode: "off" });
    } catch (e) {
      notify(e.message, true);
    }
    if (memAuth) {
      try {
        info = await call("router/info", { auth: memAuth });
      } catch (e) {
        if (e.status === 401) setAuth(null);
        else notify(e.message, true);
      }
    }
    renderInfo();
    syncForm();
  }

  init();
})();
