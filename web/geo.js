"use strict";

// Country lists card: picks the countries whose subnets the container serves to the router.
(() => {
  const el = (s) => document.querySelector(s);
  const form = el("#geo-form");
  const AUTH_KEY = "awg-router-auth"; // shared with router.js

  function notify(msg, isError) {
    if (typeof toast === "function") toast(msg, isError);
    else if (isError) alert(msg);
  }

  async function call(path, body) {
    const res = await fetch("/api/" + path, body === undefined ? {} : {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    let data = {};
    try { data = await res.json(); } catch { /* empty body */ }
    if (!res.ok) throw new Error(data.error || res.status + " " + res.statusText);
    return data;
  }

  function ago(iso) {
    const t = Date.parse(iso);
    return isNaN(t) ? "never" : fmtAgo(t / 1000);
  }

  function render(st) {
    form.countries.value = (st.countries || []).join(" ");
    form.source.value = st.source || "";
    form.source.placeholder = st.default_source || "";

    const list = el("#geo-status");
    list.replaceChildren();
    for (const c of st.status || []) {
      const li = document.createElement("li");
      const cc = document.createElement("span");
      cc.className = "cc";
      cc.textContent = c.country.toUpperCase();
      li.append(cc, c.prefixes ? `${c.prefixes} subnets, updated ${ago(c.updated)}` : "no list yet");
      if (c.error) {
        const err = document.createElement("span");
        err.className = "err";
        err.textContent = " · " + c.error;
        li.append(err);
      }
      list.append(li);
    }

    const url = st.url || location.origin + st.path;
    const link = el("#geo-url a");
    link.href = location.origin + st.path;
    link.textContent = url;
    el("#geo-url").hidden = !st.subnets;

    const policy = "ftp,read,write,test";
    el("#geo-manual pre").textContent = [
      `/system/script/add name=${st.list} policy=${policy} source={`,
      `  /tool/fetch url="${url}" dst-path=${st.list}.rsc`,
      `  /import file-name=${st.list}.rsc`,
      `  /file/remove ${st.list}.rsc`,
      `}`,
      `/system/scheduler/add name=${st.list} start-time=04:30:00 interval=1d policy=${policy} \\`,
      `    on-event="/system/script/run ${st.list}"`,
      `/system/script/run ${st.list}`,
    ].join("\n");
  }

  // Asks the router to reload the list now, if the routing plan uses it and the router login is
  // known in this tab. Otherwise the router picks it up at its next daily run.
  async function pushToRouter() {
    let auth = null;
    try { auth = JSON.parse(sessionStorage.getItem(AUTH_KEY)); } catch { /* no login */ }
    try {
      const { plan } = await call("router");
      if (!plan || !plan.bypass_geo) return "";
      if (!auth) return " The router loads it at its next daily update (04:30).";
      await call("router/geo", { auth });
      return " The router is loading it now.";
    } catch (e) {
      return " Router: " + e.message;
    }
  }

  async function submit(btn, req, okMsg) {
    btn.disabled = true;
    const label = btn.textContent;
    btn.textContent = "…";
    try {
      const st = await req();
      render(st);
      const failed = (st.status || []).filter((c) => c.error).map((c) => c.country.toUpperCase());
      if (failed.length) notify("Download failed: " + failed.join(", ") + ". See the list below.", true);
      else notify(okMsg + (await pushToRouter()));
    } catch (e) {
      notify(e.message, true);
    } finally {
      btn.disabled = false;
      btn.textContent = label;
    }
  }

  form.addEventListener("submit", (e) => {
    e.preventDefault();
    const body = {
      countries: form.countries.value.split(/[\s,;]+/).filter(Boolean),
      source: form.source.value.trim(),
    };
    submit(form.querySelector("button[type=submit]"), () => call("geo", body), "Country list saved.");
  });
  el("#geo-update").addEventListener("click", (e) => submit(e.currentTarget, () => call("geo/update", {}), "Country list updated."));

  call("geo").then(render).catch((e) => notify(e.message, true));
})();
