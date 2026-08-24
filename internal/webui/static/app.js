"use strict";

// ---- auth ---------------------------------------------------------------
const TOKEN_KEY = "af_admin_token";
let token = localStorage.getItem(TOKEN_KEY) || "";

const connBadge = document.getElementById("conn");
const overlay = document.getElementById("tokenOverlay");

function setConn(state) {
  connBadge.className = "badge " + (state === "ok" ? "ok" : state === "auth" ? "warn" : "err");
  connBadge.textContent = state === "ok" ? "connected" : state === "auth" ? "token required" : "offline";
}

async function api(path, opts = {}) {
  opts.headers = Object.assign({}, opts.headers, token ? { Authorization: "Bearer " + token } : {});
  if (opts.body && typeof opts.body !== "string") {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(opts.body);
  }
  const res = await fetch(path, opts);
  if (res.status === 401) {
    setConn("auth");
    overlay.classList.remove("hidden");
    throw new Error("unauthorized");
  }
  setConn("ok");
  return res;
}

document.getElementById("tokenSave").onclick = async () => {
  token = document.getElementById("tokenInput").value.trim();
  try {
    const res = await api("/admin/api/state");
    if (!res.ok) throw new Error(await res.text());
    localStorage.setItem(TOKEN_KEY, token);
    overlay.classList.add("hidden");
    document.getElementById("tokenErr").textContent = "";
    boot();
  } catch (e) {
    document.getElementById("tokenErr").textContent = "token rejected";
  }
};
document.getElementById("lockBtn").onclick = () => overlay.classList.remove("hidden");

// ---- tabs ---------------------------------------------------------------
const tabButtons = document.querySelectorAll("#tabs button");
tabButtons.forEach(b => b.onclick = () => {
  tabButtons.forEach(x => x.classList.toggle("active", x === b));
  document.querySelectorAll(".tab").forEach(s => s.classList.toggle("active", s.id === "tab-" + b.dataset.tab));
  if (b.dataset.tab === "config") loadConfig();
  if (b.dataset.tab === "models") loadModels();
  if (b.dataset.tab === "metrics") loadMetrics();
});

const esc = s => String(s ?? "").replace(/[&<>"]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
const fmtTime = u => u ? new Date(u * 1000).toLocaleString() : "—";
const fmtUptime = s => {
  const d = Math.floor(s / 86400), h = Math.floor(s % 86400 / 3600), m = Math.floor(s % 3600 / 60);
  return (d ? d + "d " : "") + (h ? h + "h " : "") + m + "m";
};

// ---- dashboard ----------------------------------------------------------
async function loadState() {
  try {
    const res = await api("/admin/api/state");
    if (!res.ok) return;
    const s = await res.json();
    document.getElementById("instanceInfo").innerHTML = `
      <dt>Version</dt><dd>${esc(s.version)}</dd>
      <dt>Uptime</dt><dd>${fmtUptime(s.uptime_s)}</dd>
      <dt>Started</dt><dd>${fmtTime(s.started_at)}</dd>
      <dt>Config</dt><dd class="mono">${esc(s.config_path)}</dd>`;
    document.getElementById("sessCounts").textContent = `${s.sessions_active} active · ${s.sessions_idle} idle`;
    document.querySelector("#sessionsTable tbody").innerHTML = (s.sessions || []).map(x => `
      <tr><td class="mono">${esc(x.session_id)}</td><td>${esc(x.agent)}</td>
      <td class="mono">${esc(x.parent_id || "—")}</td>
      <td><span class="badge ${x.busy ? "info" : "ok"}">${x.busy ? "busy" : "idle"}</span></td></tr>`).join("")
      || `<tr><td colspan="4" class="muted">no live sessions</td></tr>`;
    document.querySelector("#agentsTable tbody").innerHTML = (s.agents || []).map(a => `
      <tr><td>${esc(a.name)}</td><td>${esc(a.model || "default")}</td><td class="mono">${esc(a.loop)}</td>
      <td>${a.singleton ? "singleton" : a.persistent ? "persistent" : "per-chat"}</td></tr>`).join("");
    document.querySelector("#channelsTable tbody").innerHTML = (s.channels || []).map(c => `
      <tr><td>${esc(c.name)}</td><td>${esc(c.type)}</td><td>${esc(c.agent)}</td>
      <td class="mono">${esc(c.path || "—")}</td>
      <td>${c.media_enabled ? '<span class="badge ok">on</span>' : '<span class="muted">off</span>'}</td></tr>`).join("")
      || `<tr><td colspan="5" class="muted">no channels</td></tr>`;
    document.getElementById("credDisabled").classList.toggle("hidden", !!s.credentials_enabled);
    document.getElementById("credPane").classList.toggle("hidden", !s.credentials_enabled);
    if (s.credentials_enabled) loadCredUsers();
  } catch (e) { /* badge already reflects state */ }
}

// ---- credentials --------------------------------------------------------
async function loadCredUsers() {
  const res = await api("/admin/api/credentials/users");
  if (!res.ok) return;
  const { users } = await res.json();
  const sel = document.getElementById("credUser");
  const cur = sel.value;
  sel.innerHTML = (users || []).map(u => `<option>${esc(u)}</option>`).join("");
  if (cur) sel.value = cur;
  loadCreds();
}
function credUser() {
  return document.getElementById("credUserNew").value.trim() || document.getElementById("credUser").value;
}
async function loadCreds() {
  const u = credUser();
  if (!u) return;
  const res = await api("/admin/credentials?user=" + encodeURIComponent(u));
  if (!res.ok) return;
  const { credentials } = await res.json();
  document.querySelector("#credTable tbody").innerHTML = (credentials || []).map(c => `
    <tr><td>${esc(c.service)}</td><td>${esc(c.kind)}</td>
    <td class="mono">${esc(c.fingerprint || "—")}</td>
    <td>${fmtTime(c.updated_at)}</td>
    <td><button class="danger small" data-del="${esc(c.service)}">revoke</button></td></tr>`).join("")
    || `<tr><td colspan="5" class="muted">no keys for this user</td></tr>`;
  document.querySelectorAll("[data-del]").forEach(b => b.onclick = async () => {
    if (!confirm(`Revoke key for service "${b.dataset.del}" (user ${credUser()})?`)) return;
    await api(`/admin/credentials/${encodeURIComponent(b.dataset.del)}?user=${encodeURIComponent(credUser())}`, { method: "DELETE" });
    loadCreds(); loadCredUsers();
  });
}
document.getElementById("credRefresh").onclick = () => { loadCredUsers(); };
document.getElementById("credUser").onchange = loadCreds;
document.getElementById("credAdd").onclick = async () => {
  const body = {
    user: credUser(),
    service: document.getElementById("credService").value.trim(),
    kind: document.getElementById("credKind").value.trim() || "api_key",
    secret: document.getElementById("credSecret").value,
    header: document.getElementById("credHeader").value.trim(),
    scheme: document.getElementById("credScheme").value.trim(),
  };
  if (!body.user || !body.service || !body.secret) { alert("user, service and secret are required"); return; }
  const res = await api("/admin/credentials", { method: "POST", body });
  if (res.ok) {
    document.getElementById("credSecret").value = "";
    loadCreds(); loadCredUsers();
  } else alert("save failed: " + await res.text());
};

// ---- log tail (SSE over fetch, so the bearer header can be sent) --------
async function streamLogs() {
  const el = document.getElementById("logTail");
  for (;;) {
    try {
      const res = await api("/admin/api/logs");
      if (!res.ok || !res.body) throw new Error("log stream failed");
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      let buf = "";
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        const chunks = buf.split("\n\n");
        buf = chunks.pop();
        for (const ch of chunks) {
          const line = ch.split("\n").filter(l => l.startsWith("data: ")).map(l => l.slice(6)).join("\n");
          if (!line) continue;
          const atBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 20;
          el.textContent += line + "\n";
          if (el.textContent.length > 200000) el.textContent = el.textContent.slice(-100000);
          if (atBottom) el.scrollTop = el.scrollHeight;
        }
      }
    } catch (e) {
      if (e.message === "unauthorized") return; // overlay is up; boot() restarts this
    }
    await new Promise(r => setTimeout(r, 3000)); // reconnect backoff
  }
}

// ---- models -------------------------------------------------------------
async function loadModels() {
  const res = await api("/admin/api/models");
  if (!res.ok) return;
  const { models } = await res.json();
  document.getElementById("driftBanner").classList.toggle("hidden", !(models || []).some(m => m.drift));
  document.querySelector("#modelsTable tbody").innerHTML = (models || []).map(m => {
    const state = !m.in_runtime ? '<span class="badge err">file only</span>'
      : !m.in_file ? '<span class="badge warn">runtime only</span>'
      : m.drift ? '<span class="badge warn">drift</span>'
      : '<span class="badge ok">in sync</span>';
    return `<tr>
      <td><b>${esc(m.name)}</b></td><td>${esc(m.provider)}</td><td class="mono">${esc(m.model)}</td>
      <td class="mono">${esc(m.base_url || "—")}</td>
      <td class="mono">${m.has_key ? esc(m.key_fingerprint || "set") : '<span class="muted">none</span>'}</td>
      <td>${state}</td>
      <td style="white-space:nowrap">
        <button class="small secondary" data-edit="${esc(m.name)}">edit</button>
        <button class="small secondary" data-test="${esc(m.name)}">test</button>
        <button class="small danger" data-rm="${esc(m.name)}">remove</button>
      </td></tr>`;
  }).join("") || `<tr><td colspan="7" class="muted">no models configured</td></tr>`;
  document.querySelectorAll("[data-edit]").forEach(b => b.onclick = () => openEditor((models || []).find(m => m.name === b.dataset.edit)));
  document.querySelectorAll("[data-rm]").forEach(b => b.onclick = async () => {
    if (!confirm(`Remove model "${b.dataset.rm}" from the live runtime?`)) return;
    await api(`/admin/api/models/${encodeURIComponent(b.dataset.rm)}`, { method: "DELETE" });
    loadModels();
  });
  document.querySelectorAll("[data-test]").forEach(b => b.onclick = async () => {
    b.textContent = "…";
    const res = await api(`/admin/api/models/${encodeURIComponent(b.dataset.test)}/test`, { method: "POST" });
    const v = await res.json();
    b.textContent = v.ok ? "✓ " + v.latency_ms + "ms" : "✗ " + (v.class || "error");
    b.className = "small " + (v.ok ? "" : "danger");
  });
}

function openEditor(m) {
  document.getElementById("modelEditor").classList.remove("hidden");
  document.getElementById("modelEditorTitle").textContent = m ? `Edit model "${m.name}"` : "Add model";
  document.getElementById("mName").value = m ? m.name : "";
  document.getElementById("mName").disabled = !!m;
  document.getElementById("mProvider").value = m ? m.provider : "openai";
  document.getElementById("mModel").value = m ? m.model : "";
  document.getElementById("mBaseURL").value = m ? m.base_url : "";
  document.getElementById("mAPIKey").value = ""; // never prefilled; blank keeps the current key
  document.getElementById("mTimeout").value = m ? m.timeout : "";
  document.getElementById("mRetry").value = m ? m.retry : 0;
  document.getElementById("mMaxTokens").value = m ? m.max_tokens : 0;
  document.getElementById("mServerTools").value = m ? (m.server_tools || []).join(", ") : "";
}
document.getElementById("newModelBtn").onclick = () => openEditor(null);
document.getElementById("mCancel").onclick = () => document.getElementById("modelEditor").classList.add("hidden");
function editorBody() {
  return {
    provider: document.getElementById("mProvider").value,
    model: document.getElementById("mModel").value.trim(),
    base_url: document.getElementById("mBaseURL").value.trim(),
    api_key: document.getElementById("mAPIKey").value,
    timeout: document.getElementById("mTimeout").value.trim(),
    retry: +document.getElementById("mRetry").value || 0,
    max_tokens: +document.getElementById("mMaxTokens").value || 0,
    server_tools: document.getElementById("mServerTools").value.split(",").map(s => s.trim()).filter(Boolean),
  };
}
document.getElementById("mSave").onclick = async () => {
  const name = document.getElementById("mName").value.trim() || "default";
  const res = await api(`/admin/api/models/${encodeURIComponent(name)}`, { method: "PUT", body: editorBody() });
  if (res.ok) {
    document.getElementById("modelEditor").classList.add("hidden");
    loadModels();
  } else {
    const e = await res.json();
    alert("save failed: " + (e.error || res.status));
  }
};
document.getElementById("mTest").onclick = async () => {
  const el = document.getElementById("mTestResult");
  const name = document.getElementById("mName").value.trim() || "default";
  el.textContent = "applying…";
  // Test verifies the form's values, so apply them first (same as Save).
  await api(`/admin/api/models/${encodeURIComponent(name)}`, { method: "PUT", body: editorBody() });
  el.textContent = "testing…";
  const res = await api(`/admin/api/models/${encodeURIComponent(name)}/test`, { method: "POST" });
  const v = await res.json();
  el.textContent = v.ok ? `✓ ok (${v.latency_ms}ms)` : `✗ ${v.class}: ${v.error || ""}`;
  loadModels();
};
document.getElementById("persistBtn").onclick = async () => {
  if (!confirm("Write the live model set into the config file? (backup kept as .bak)")) return;
  const res = await api("/admin/api/models/persist", { method: "POST" });
  const v = await res.json();
  if (!v.ok) alert("persist failed: " + v.error);
  loadModels();
};
document.getElementById("revertBtn").onclick = async () => {
  if (!confirm("Discard runtime model edits and reload from the config file?")) return;
  const res = await api("/admin/api/models/revert", { method: "POST" });
  const v = await res.json();
  if (!v.ok) alert("revert failed: " + v.error);
  loadModels();
};

// ---- config -------------------------------------------------------------
let configMtime = 0;
async function loadConfig() {
  const res = await api("/admin/api/config");
  if (!res.ok) return;
  const c = await res.json();
  configMtime = c.mtime;
  document.getElementById("configPath").textContent = c.path;
  document.getElementById("configEditor").value = c.raw;
  document.getElementById("configView").textContent = JSON.stringify(c.view, null, 2);
  document.getElementById("restartBanner").classList.add("hidden");
  document.getElementById("conflictBanner").classList.add("hidden");
  document.getElementById("configStatus").textContent = "";
}
document.getElementById("validateBtn").onclick = async () => {
  const st = document.getElementById("configStatus");
  st.textContent = "validating…";
  const res = await api("/admin/api/config/validate", { method: "POST", body: { raw: document.getElementById("configEditor").value } });
  const v = await res.json();
  st.textContent = v.ok ? "✓ valid (boot-equivalent checks)" : "✗ " + v.error;
};
document.getElementById("saveConfigBtn").onclick = async () => {
  const st = document.getElementById("configStatus");
  const res = await api("/admin/api/config/save", { method: "POST", body: { raw: document.getElementById("configEditor").value, mtime: configMtime } });
  const v = await res.json();
  if (res.status === 409) {
    document.getElementById("conflictBanner").classList.remove("hidden");
    st.textContent = "";
    return;
  }
  if (v.ok) {
    configMtime = v.mtime;
    document.getElementById("restartBanner").classList.remove("hidden");
    st.textContent = "✓ saved";
  } else {
    st.textContent = "✗ " + (v.error || "save failed");
  }
};
document.getElementById("reloadConfigBtn").onclick = loadConfig;

// ---- metrics ------------------------------------------------------------
const sparks = {}; // name -> canvas ctx cache
async function loadMetrics() {
  const res = await api("/admin/api/metrics");
  if (!res.ok) return;
  const { series, latest } = await res.json();
  const grid = document.getElementById("metricsGrid");
  const names = Object.keys(series || {}).sort();
  for (const name of names) {
    let card = document.getElementById("mc-" + name);
    if (!card) {
      card = document.createElement("div");
      card.className = "card";
      card.id = "mc-" + name;
      card.innerHTML = `<div class="metric-name">${esc(name)}</div><div class="metric-val"></div><canvas class="spark"></canvas>`;
      grid.appendChild(card);
    }
    card.querySelector(".metric-val").textContent = (latest[name] ?? 0).toLocaleString();
    drawSpark(card.querySelector("canvas"), series[name] || []);
  }
}
function drawSpark(canvas, pts) {
  const dpr = window.devicePixelRatio || 1;
  const w = canvas.clientWidth, h = canvas.clientHeight;
  if (!w) return;
  canvas.width = w * dpr; canvas.height = h * dpr;
  const ctx = canvas.getContext("2d");
  ctx.scale(dpr, dpr);
  ctx.clearRect(0, 0, w, h);
  if (pts.length < 2) return;
  const max = Math.max(...pts.map(p => p.v), 1);
  const min = Math.min(...pts.map(p => p.v));
  const span = Math.max(max - min, 1);
  ctx.strokeStyle = "#58a6ff";
  ctx.lineWidth = 1.5;
  ctx.beginPath();
  pts.forEach((p, i) => {
    const x = i / (pts.length - 1) * w;
    const y = h - 2 - ((p.v - min) / span) * (h - 4);
    i ? ctx.lineTo(x, y) : ctx.moveTo(x, y);
  });
  ctx.stroke();
}

// ---- boot ---------------------------------------------------------------
let booted = false;
function boot() {
  loadState();
  loadModels();
  if (!booted) {
    booted = true;
    streamLogs();
    setInterval(loadState, 5000);
    setInterval(() => {
      if (document.querySelector("#tabs button[data-tab=metrics]").classList.contains("active")) loadMetrics();
    }, 5000);
  }
}
if (token) boot(); else { setConn("auth"); overlay.classList.remove("hidden"); }
