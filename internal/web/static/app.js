"use strict";

// UI state that survives re-rendering on every status update.
const ui = {
  status: null,
  connected: false,
  inputs: {},  // data-key -> value
  bleOpen: {}, // headphone id -> true
  open: {},    // expanded details: headphone id, "node:<id>" or "pairing" -> true
  pairNode: "",
  showAll: false,
  busy: {},    // action key -> true while a request runs
};

const $ = (sel) => document.querySelector(sel);

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function toast(msg, err = false) {
  const el = $("#toast");
  el.textContent = msg;
  el.className = "toast" + (err ? " err" : "");
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => el.classList.add("hidden"), err ? 6000 : 3000);
}

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    // Errors with a code are translated; the English text is the fallback.
    const k = data.code && "err_" + data.code;
    throw new Error(k && k in I18N.en ? t(k, ...(data.args || [])) : data.error || res.statusText);
  }
  return data;
}

async function run(key, fn, okKey) {
  ui.busy[key] = true;
  render();
  try {
    await fn();
    if (okKey) toast(t(okKey));
  } catch (e) {
    toast(e.message, true);
  } finally {
    delete ui.busy[key];
    render();
  }
}

function input(key, placeholder, def = "") {
  if (!(key in ui.inputs)) ui.inputs[key] = def;
  return `<input data-key="${esc(key)}" placeholder="${esc(placeholder)}" value="${esc(ui.inputs[key])}">`;
}

function actionKey(action, args) {
  return action + ":" + JSON.stringify(args);
}

function btn(action, label, args = {}, cls = "") {
  const data = Object.entries(args).map(([k, v]) => `data-${k}="${esc(v)}"`).join(" ");
  const busy = ui.busy[actionKey(action, args)];
  return `<button class="${cls}" data-action="${action}" ${data} ${busy ? "disabled" : ""}>${esc(label)}</button>`;
}

function nodeOptions(selected) {
  const nodes = (ui.status?.nodes || []).filter((n) => n.online);
  return nodes.map((n) => `<option value="${esc(n.id)}" ${n.id === selected ? "selected" : ""}>${esc(n.name)}</option>`).join("");
}

function firstOnlineNode() {
  return (ui.status.nodes.find((n) => n.online) || {}).id || "";
}

function fmtWait(s) {
  return s < 60 ? t("uptimeS", s) : t("waitMin", Math.floor(s / 60), String(s % 60).padStart(2, "0"));
}

function fmtUptime(s) {
  if (s < 120) return t("uptimeS", s);
  if (s < 7200) return t("uptimeMin", Math.round(s / 60));
  return t("uptimeH", Math.floor(s / 3600), Math.round((s % 3600) / 60));
}

const stateLabel = {
  absent: ["stateAbsent", "muted"],
  available: ["stateAvailable", "info"],
  connected: ["stateConnected", "ok"],
  streaming: ["stateStreaming", "ok"],
};

function rssiRows(h) {
  if (!h.rssi?.length) {
    return `<div class="row small muted">${esc(h.ble_name || h.ble_addr || h.ble_irk ? t("noReception") : t("noBLEIdentity"))}</div>`;
  }
  const rows = h.rssi.map((r) => {
    const v = r.smoothed || r.rssi;
    const pct = Math.max(0, Math.min(100, ((v + 100) / 60) * 100));
    return `<div>${esc(r.node_name || r.node)}${r.active ? " ★" : ""}</div>
      <div class="bar ${r.active ? "active" : ""}"><span style="width:${pct}%"></span></div>
      <div class="mono">${r.rssi} dBm${r.smoothed ? ` (Ø ${r.smoothed})` : ""}${r.age_s > 5 ? ` · ${esc(t("rssiOld", r.age_s))}` : ""}</div>`;
  });
  return `<div class="rssi">${rows.join("")}</div>`;
}

// Battery level reported over the hands-free profile. Without a connection it
// is the last known value.
function batteryPill(h) {
  const b = h.battery;
  if (!b) return "";
  const live = h.state === "connected" || h.state === "streaming";
  const cls = !live ? "muted" : b.percent <= 15 ? "bad" : b.percent <= 30 ? "warn" : "ok";
  let label = t("battery", b.percent) + (b.charging ? " ⚡" : "");
  if (!live) label += " " + t("batteryLastKnown");
  const when = new Date(b.at).toLocaleString(lang);
  const title = t("batteryTitle", when, t("batterySource_" + b.source));
  return `<span class="pill ${cls}" title="${esc(title)}">${esc(label)}</span>`;
}

// Key/value rows for the details panels; rows with an empty value are left out.
function kv(rows) {
  return `<div class="kv">${rows.filter(([, v]) => v !== "" && v != null)
    .map(([k, v]) => `<div class="muted">${esc(t(k))}</div><div>${v}</div>`).join("")}</div>`;
}

function moreButton(key) {
  return `<button class="text" data-action="toggle-more" data-id="${esc(key)}">${esc(t(ui.open[key] ? "less" : "more"))}</button>`;
}

function errorLine(h) {
  if (h.error_code) {
    const a = h.error_args || [];
    const msg = h.error_code === "node_busy"
      ? (a[2] !== "" && a[2] !== undefined ? t("errNodeBusyWait", a[0], a[1], fmtWait(+a[2])) : t("errNodeBusy", a[0], a[1]))
      : t("err_" + h.error_code);
    return `<div class="row small warn-text">${esc(msg)}</div>`;
  }
  return h.error ? `<div class="row small bad-text">${esc(h.error)}</div>` : "";
}

function renderHeadphone(h) {
  const [labelKey, cls] = stateLabel[h.state] || [h.state, "muted"];
  const s = h.stream;
  const live = h.state === "connected" || h.state === "streaming";
  let html = `<div class="card" data-card="hp:${esc(h.id)}"><h3>${esc(h.name)} <span class="pill ${cls}">${esc(t(labelKey))}</span>
    ${batteryPill(h)}
    ${h.voice_active ? `<span class="pill info">${esc(t("voiceActive"))}</span>` : ""}
    ${h.player_on_ma ? "" : `<span class="pill warn" title="${esc(t("notOnMATitle"))}">${esc(t("notOnMA"))}</span>`}</h3>`;

  // One summary line: node, codec and reception.
  const active = h.rssi?.find((r) => r.active);
  const summary = [h.node_name, s ? `${s.codec} · ${Math.round(s.bitrate / 1000)} kbit/s` : "", active ? `${active.rssi} dBm` : ""].filter(Boolean);
  if (summary.length) html += `<div class="row small muted">${esc(summary.join(" · "))}</div>`;
  if (h.title) {
    html += `<div class="nowplaying">${h.artwork_url ? `<img src="${esc(h.artwork_url)}" alt="">` : ""}
      <div><div><b>${esc(h.title)}</b> ${h.playing ? "▶" : "⏸"}</div><div class="small muted">${esc(h.artist || "")}${h.album ? " · " + esc(h.album) : ""}</div></div></div>`;
  }
  if (live && h.disconnect_in !== undefined) html += `<div class="row small muted">${esc(t("idleLine", fmtWait(h.disconnect_in)))}</div>`;
  html += errorLine(h);

  const ckey = "connect-node:" + h.id;
  if (!ui.inputs[ckey]) ui.inputs[ckey] = h.node || firstOnlineNode();
  const online = (ui.status.nodes || []).filter((n) => n.online).length;
  html += `<div class="actions">
    ${!live && online > 1 ? `<select data-key="${esc(ckey)}">${nodeOptions(ui.inputs[ckey])}</select>` : ""}
    ${live ? btn("disconnect", t("disconnect"), { id: h.id }) : btn("connect", t("connect"), { id: h.id }, "primary")}
    <span class="spacer"></span>${moreButton(h.id)}</div>`;
  if (ui.open[h.id]) html += renderHeadphoneDetails(h);
  return html + "</div>";
}

function renderHeadphoneDetails(h) {
  const s = h.stream;
  const live = h.state === "connected" || h.state === "streaming";
  let voice = "";
  if (h.wyoming_port) {
    voice = esc(h.voice_active ? t("voiceActive") : h.wyoming_ready ? t("voiceReady") : t("voiceNoHA")) +
      ` <span class="mono muted">${esc(location.hostname)}:${h.wyoming_port}</span>`;
  }
  const parts = [h.ble_name, h.ble_addr, h.ble_irk ? t("irkShort") : ""].filter(Boolean);
  const ident = parts.length ? parts.join(" / ") : t("none");
  let html = `<div class="panel">` + kv([
    ["kvDevice", `${esc(h.device_name || "")} <span class="mono muted">${esc(h.addr)}</span>`],
    ["kvStream", s ? esc(t("streamLine", s.codec, Math.round(s.bitrate / 1000), s.mtu)) : ""],
    ["kvBuffer", s ? esc(t("bufferLine", s.node_queue_ms, (s.buffered_ms / 1000).toFixed(1), s.packets, s.drift_ms.toFixed(1), s.corrections, s.congestion, s.node_queue_min_ms ?? "–")) : ""],
    ["kvOffers", esc((h.offered_codecs || []).join(", "))],
    ["kvVolume", esc(`${h.volume} %` + (h.abs_volume ? t("volumeHeadphone") : ""))],
    ["kvVoice", voice],
    ["kvYield", live && h.yield_in !== undefined ? esc(h.yield_in > 0 ? t("inTime", fmtWait(h.yield_in)) : t("now")) : ""],
    ["kvPaired", esc((h.paired_nodes || []).join(", ") || "–")],
    ["kvBLE", esc(ident)],
  ]);
  html += `<div class="subhead">${esc(t("kvReception"))}</div>` + rssiRows(h);
  html += `<div class="subhead">${esc(t("settings"))}</div>` + renderCodecRow(h);
  html += `<div class="actions">
    ${btn("toggle-ble", ui.bleOpen[h.id] ? t("bleClose") : t("bleOpen"), { id: h.id })}
    ${btn("rename", t("rename"), { id: h.id })}
    ${btn("forget", t("forget"), { id: h.id }, "danger")}</div>`;
  if (ui.bleOpen[h.id]) html += renderBLEPanel(h);
  return html + "</div>";
}

function renderCodecRow(h) {
  const ck = "codec:" + h.id;
  const qk = "ldacq:" + h.id;
  if (!(ck in ui.inputs)) ui.inputs[ck] = h.codec_pref;
  if (!(qk in ui.inputs)) ui.inputs[qk] = h.ldac_quality;
  const codecs = (ui.status.codec_choices || ["auto"]).map((c) =>
    `<option value="${esc(c)}" ${c === ui.inputs[ck] ? "selected" : ""}>${esc(c === "auto" ? t("codecAuto") : c)}</option>`).join("");
  const qualities = ["auto", "hq", "sq", "mq"].map((q) =>
    `<option value="${q}" ${q === ui.inputs[qk] ? "selected" : ""}>${esc(t("ldacQ_" + q))}</option>`).join("");
  const changed = ui.inputs[ck] !== h.codec_pref || ui.inputs[qk] !== h.ldac_quality;
  return `<div class="actions tight"><label class="small">${esc(t("codec"))} <select data-key="${esc(ck)}">${codecs}</select></label>
    <label class="small">LDAC <select data-key="${esc(qk)}">${qualities}</select></label>
    ${changed ? btn("codec-save", t("apply"), { id: h.id }, "primary") : ""}
    <span class="small muted">${esc(t("codecHint"))}</span></div>`;
}

// Random addresses tell their kind by the two most significant bits:
// 11 static, 01 resolvable private (rotating), 00 non-resolvable.
function addrTypeLabel(b) {
  if (b.addr_type === 0) return t("addrPublic");
  if (b.addr_type !== 1) return t("addrRPA");
  const top = parseInt(b.addr.slice(0, 2), 16) & 0xc0;
  return top === 0xc0 ? t("addrStatic") : top === 0x40 ? t("addrRPA") : t("addrRandom");
}

// A BLE address is a usable identity if it is public or static random.
function stableAddr(b) {
  return b.addr_type === 0 || (b.addr_type === 1 && parseInt(b.addr.slice(0, 2), 16) >= 0xc0);
}

// Bluetooth SIG company IDs of common headphone and phone makers.
const COMPANIES = {
  // Headphones in pairing mode often send Microsoft's Swift Pair beacon.
  0x0006: "Microsoft / Swift Pair", 0x004c: "Apple", 0x0057: "Harman", 0x0059: "Nordic", 0x0075: "Samsung",
  0x0087: "Garmin", 0x009e: "Bose", 0x00e0: "Google", 0x012d: "Sony", 0x0171: "Amazon",
  0x027d: "Huawei", 0x02e5: "Espressif", 0x038f: "Xiaomi",
};

function companyLabel(c) {
  if (c === undefined || c < 0) return "–";
  return COMPANIES[c] || "0x" + c.toString(16).padStart(4, "0");
}

// A resolvable private address: random, top two bits 01.
function rotatingAddr(b) {
  return b.addr_type === 1 && (parseInt(b.addr.slice(0, 2), 16) & 0xc0) === 0x40;
}

// One scanned advertiser: what it is, whether the bridge attributes it to a
// headphone (and how), and what can be taken over from it.
function renderBLEEntry(h, b) {
  const mine = b.headphone === h.id;
  let match = "";
  if (mine) {
    match = `<span class="pill ok">${esc(t("matchThis", t("via_" + b.via)))}</span>`;
  } else if (b.headphone) {
    const other = ui.status.headphones.find((x) => x.id === b.headphone);
    match = `<span class="pill muted">${esc(t("matchOther", other?.name || b.headphone))}</span>`;
  }
  // Nothing to take over from an entry that already identifies this headphone.
  const actions = mine ? "" : [
    b.name ? btn("ble-use", t("useName"), { id: h.id, name: b.name }) : "",
    stableAddr(b) ? btn("ble-use", t("useAddr"), { id: h.id, addr: b.addr }) : "",
    rotatingAddr(b) ? btn("ble-pair", t("blePair"), { id: h.id, addr: b.addr, type: b.addr_type }, "primary") : "",
  ].join("");
  return `<div class="bleitem${mine ? " mine" : ""}" data-key="ble:${esc(b.addr)}">
    <div class="bleinfo">
      <div><b>${esc(b.name || "–")}</b> <span class="small muted">· ${esc(companyLabel(b.company))} · ${b.rssi} dBm</span> ${match}</div>
      <div class="small muted"><span class="mono">${esc(b.addr)}</span> · ${esc(addrTypeLabel(b))}</div>
    </div>
    ${actions ? `<div class="bleactions">${actions}</div>` : ""}
  </div>`;
}

function renderBLEPanel(h) {
  const key = "ble-node:" + h.id;
  if (!ui.inputs[key]) ui.inputs[key] = h.node || firstOnlineNode();
  const n = ui.status.nodes.find((x) => x.id === ui.inputs[key]);
  // Entries of this headphone first, then by reception (the bridge sorts by RSSI).
  const list = (n?.ble || []).filter((b) => ui.showAll || b.name || b.headphone)
    .sort((x, y) => (y.headphone === h.id) - (x.headphone === h.id));
  let html = `<div class="panel"><div class="small muted">${esc(t("bleHelp"))}</div>
    <div class="small muted">${esc(t("blePairHelp"))}</div>
    <div class="actions"><select data-key="${esc(key)}">${nodeOptions(ui.inputs[key])}</select>
    ${btn("ble-scan", n?.ble_scanning ? t("bleScanning") : t("bleScan"), { id: h.id })}
    <label class="small"><input type="checkbox" data-action="show-all" ${ui.showAll ? "checked" : ""}> ${esc(t("alsoUnnamed"))}</label></div>`;
  if (h.ble_irk) {
    html += `<div class="actions tight">${h.ble_irk_unverified
      ? `<span class="pill warn" title="${esc(t("irkUnverifiedTitle"))}">${esc(t("irkUnverified"))}</span>`
      : `<span class="pill ok">${esc(t("irkKnown"))}</span>`}
      ${h.ble_identity ? `<span class="small muted">${esc(t("irkIdentity"))} <span class="mono">${esc(h.ble_identity)}</span></span>` : ""}
      ${btn("ble-irk-clear", t("irkClear"), { id: h.id }, "danger")}</div>`;
  }
  if (list.length) {
    html += `<div class="blelist">` + list.slice(0, 40).map((b) => renderBLEEntry(h, b)).join("") + `</div>`;
  }
  html += `<div class="actions">${input("ble-name:" + h.id, t("namePrefix"), h.ble_name)}${input("ble-addr:" + h.id, t("addrOptional"), h.ble_addr)}
    ${btn("ble-save", t("save"), { id: h.id }, "primary")}</div>
    <div class="subhead">${esc(t("irkManual"))}</div>
    <div class="small muted">${esc(t("irkManualHelp"))}</div>
    <div class="actions tight"><input class="mono irk" data-key="irk:${esc(h.id)}" placeholder="${esc(t("irkPlaceholder"))}" value="${esc(ui.inputs["irk:" + h.id] || "")}" autocomplete="off" spellcheck="false">
    ${btn("irk-save", t("irkSave"), { id: h.id })}</div></div>`;
  return html;
}

function renderNode(n) {
  const key = "node:" + n.id;
  let html = `<div class="card" data-card="${esc(key)}"><h3>${esc(n.name)} ${n.online ? `<span class="pill ok">${esc(t("online"))}</span>` : `<span class="pill off">${esc(t("offline"))}</span>`}
    ${n.link !== "idle" && n.online ? `<span class="pill info">${esc(n.link)}</span>` : ""}</h3>`;
  if (!n.online) {
    if (n.last_error) html += `<div class="row small bad-text">${esc(n.last_error)}</div>`;
  } else {
    const peer = n.peer ? [n.peer_name || n.peer, n.codec].filter(Boolean).join(" · ") : t("nodeFree");
    html += `<div class="row small${n.peer ? "" : " muted"}">${esc(peer)}</div>`;
    if (n.underruns || n.dropped) html += `<div class="row small warn-text">${esc(t("nodeProblems", n.underruns, n.dropped))}</div>`;
  }
  html += `<div class="actions"><span class="spacer"></span>${moreButton(key)}</div>`;
  if (ui.open[key]) {
    html += `<div class="panel">` + kv([
      ["kvAddr", `<span class="mono">${esc(n.id)}</span>`],
      ["kvBT", n.bt_addr && !n.bt_addr.startsWith("00:00") ? `<span class="mono">${esc(n.bt_addr)}</span>` : ""],
      ["kvCodecs", esc((n.codecs || []).join(", "))],
      ["kvMTU", n.online && n.peer ? String(n.mtu) : ""],
      ["kvQueue", n.online ? esc(t("nodeQueue", n.queue_ms, n.sent, n.underruns, n.dropped)) : ""],
      ["kvMemory", n.online ? esc(t("nodeHeap", Math.round(n.free_heap / 1024), Math.round(n.min_free_heap / 1024))) : ""],
      ["kvUptime", n.online ? esc(fmtUptime(n.uptime_s)) : ""],
      ["kvBonds", n.online ? String(n.bonds?.length || 0) : ""],
    ]) + `</div>`;
  }
  return html + "</div>";
}

function renderPairing() {
  const st = ui.status;
  if (!ui.pairNode || !st.nodes.find((n) => n.id === ui.pairNode && n.online)) ui.pairNode = firstOnlineNode();
  const n = st.nodes.find((x) => x.id === ui.pairNode);
  // Collapsed unless something is going on or nothing is paired yet.
  if (!ui.open.pairing && st.headphones.length && !n?.inquiring && !st.pairing) {
    return `<div class="actions flush"><span class="small muted">${esc(t("pairHint"))}</span><span class="spacer"></span>
      <button data-action="toggle-more" data-id="pairing">${esc(t("pairOpen"))}</button></div>`;
  }
  let html = `<ol class="steps small"><li>${t("pairStep1")}</li><li>${t("pairStep2")}</li><li>${t("pairStep3")}</li></ol>
    <div class="actions"><select data-key="pair-node">${nodeOptions(ui.pairNode)}</select>
    ${btn("inquiry", n?.inquiring ? t("inquiryRunning") : t("inquiry"))}
    <label class="small"><input type="checkbox" data-action="show-all" ${ui.showAll ? "checked" : ""}> ${esc(t("allDeviceTypes"))}</label></div>`;
  const found = (n?.found || []).filter((f) => ui.showAll || f.is_audio);
  if (found.length) {
    html += `<table><tr><th>${esc(t("colDevice"))}</th><th>${esc(t("colAddress"))}</th><th>dBm</th><th>${esc(t("colMAName"))}</th><th></th></tr>` +
      found.map((f) => {
        const known = st.headphones.find((h) => h.addr === f.addr);
        return `<tr><td>${esc(f.name || "–")}</td><td class="mono">${esc(f.addr)}</td><td>${f.rssi}</td>
          <td>${input("pair-name:" + f.addr, t("colName"), known ? known.name : f.name)}</td>
          <td>${btn("pair", known ? t("pairThisNode") : t("pair"), { addr: f.addr }, "primary")}</td></tr>`;
      }).join("") + `</table>`;
  } else if (n && !n.inquiring) {
    html += `<div class="small muted">${esc(t("nothingFound"))}</div>`;
  }
  if (st.pairing) html += `<div class="row"><span class="pill warn">${esc(t("pairingRunning"))}</span></div>`;
  if (ui.open.pairing) html += `<div class="actions"><span class="spacer"></span><button class="text" data-action="toggle-more" data-id="pairing">${esc(t("less"))}</button></div>`;
  return html;
}

function renderStatic() {
  document.querySelectorAll("[data-i18n]").forEach((el) => {
    setText(el, t(el.dataset.i18n));
  });
  setText($("#lang"), lang === "de" ? "English" : "Deutsch");
  // The connection chip only shows up when the live updates are lost.
  const c = $("#conn");
  setText(c, t("disconnected"));
  const cls = "pill off" + (ui.connected ? " hidden" : "");
  if (c.className !== cls) c.className = cls;
}

// Updates the page in place (see morph.js), so buttons, drop-downs and input
// fields stay the same elements across status updates.
function render() {
  renderStatic();
  const st = ui.status;
  if (!st) return;
  setText($("#meta"), t("meta", st.version, st.sendspin_url));
  morph($("#headphones"), st.headphones.length
    ? st.headphones.map(renderHeadphone).join("")
    : `<div class="card muted">${esc(t("noHeadphones"))}</div>`);
  morph($("#nodes"), st.nodes.map(renderNode).join(""));
  morph($("#pairing"), renderPairing());
}

document.addEventListener("input", (e) => {
  const key = e.target.dataset.key;
  if (!key) return;
  ui.inputs[key] = e.target.value;
  if (key === "pair-node") ui.pairNode = e.target.value;
  if (key.startsWith("ble-node:") || key === "pair-node" || key.startsWith("codec:") || key.startsWith("ldacq:")) render();
});

document.addEventListener("change", (e) => {
  if (e.target.dataset.action === "show-all") {
    ui.showAll = e.target.checked;
    render();
  }
});

document.addEventListener("click", (e) => {
  if (e.target.id === "lang") {
    setLang(lang === "de" ? "en" : "de");
    render();
    return;
  }
  const b = e.target.closest("button[data-action]");
  if (!b) return;
  const d = { ...b.dataset };
  const action = d.action;
  delete d.action;
  const key = actionKey(action, d);
  switch (action) {
    case "connect":
      run(key, () => api("POST", `/api/headphones/${d.id}/connect`, { node: ui.inputs["connect-node:" + d.id] }), "toastConnecting");
      break;
    case "disconnect":
      run(key, () => api("POST", `/api/headphones/${d.id}/disconnect`), "toastDisconnected");
      break;
    case "rename": {
      const h = ui.status.headphones.find((x) => x.id === d.id);
      const name = prompt(t("promptName"), h?.name || "");
      if (name) run(key, () => api("POST", `/api/headphones/${d.id}/rename`, { name }), "toastRenamed");
      break;
    }
    case "forget":
      if (confirm(t("confirmForget"))) run(key, () => api("DELETE", `/api/headphones/${d.id}`), "toastRemoved");
      break;
    case "toggle-more":
      ui.open[d.id] = !ui.open[d.id];
      render();
      break;
    case "toggle-ble":
      ui.bleOpen[d.id] = !ui.bleOpen[d.id];
      render();
      break;
    case "ble-scan":
      run(key, () => api("POST", "/api/ble-discovery", { node: ui.inputs["ble-node:" + d.id], seconds: 15 }));
      break;
    case "ble-use":
      if (d.name) ui.inputs["ble-name:" + d.id] = d.name;
      if (d.addr) ui.inputs["ble-addr:" + d.id] = d.addr;
      render();
      break;
    case "ble-pair":
      toast(t("toastBLEPairing"));
      run(key, () => api("POST", `/api/headphones/${d.id}/ble-pair`,
        { node: ui.inputs["ble-node:" + d.id], addr: d.addr, addr_type: +d.type }), "toastBLEPaired");
      break;
    case "irk-save":
      run(key, async () => {
        const r = await api("POST", `/api/headphones/${d.id}/ble-irk`, { irk: ui.inputs["irk:" + d.id] || "" });
        ui.inputs["irk:" + d.id] = ""; // do not keep the key in the page
        toast(r.matched ? t("toastIRKMatched", r.matched) : t("toastIRKUnverified"));
      });
      break;
    case "ble-irk-clear":
      if (confirm(t("confirmIRKClear"))) run(key, () => api("DELETE", `/api/headphones/${d.id}/ble-irk`), "toastIRKCleared");
      break;
    case "ble-save":
      run(key, () => api("POST", `/api/headphones/${d.id}/ble`, { name: ui.inputs["ble-name:" + d.id] || "", addr: ui.inputs["ble-addr:" + d.id] || "" }), "toastBLESaved");
      break;
    case "codec-save":
      run(key, () => api("POST", `/api/headphones/${d.id}/codec`, { codec: ui.inputs["codec:" + d.id], ldac_quality: ui.inputs["ldacq:" + d.id] }), "toastCodecSaved");
      break;
    case "inquiry":
      run(key, () => api("POST", "/api/inquiry", { node: ui.pairNode, seconds: 12 }));
      break;
    case "pair":
      toast(t("toastPairing"));
      run(key, () => api("POST", "/api/pair", { node: ui.pairNode, addr: d.addr, name: ui.inputs["pair-name:" + d.addr] || "" }), "toastPaired");
      break;
  }
});

function connect() {
  const es = new EventSource("/api/events");
  es.onopen = () => {
    ui.connected = true;
    render();
  };
  es.onmessage = (e) => {
    ui.status = JSON.parse(e.data);
    ui.connected = true;
    render();
  };
  es.onerror = () => {
    ui.connected = false;
    render();
    // An expired login: reload, which leads to the login page.
    fetch("/api/whoami").then((r) => { if (r.status === 401) location.reload(); }).catch(() => {});
  };
}

// Shows who is logged in (only with the Home Assistant login).
function showUser() {
  fetch("/api/whoami").then((r) => (r.ok ? r.json() : null)).then((d) => {
    if (d && d.user) {
      $("#user").innerHTML = `${esc(d.user)} · <a href="/auth/logout">${esc(t("logout"))}</a>`;
    }
  }).catch(() => {});
}

setLang(lang);
render();
connect();
showUser();
