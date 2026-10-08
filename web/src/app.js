// ray PWA. Plain JavaScript, no build step. The Go/wasm side
// (web/wasm/main_js.go) provides rayNewKey, rayPublicKey and rayDial.
//
// Wire protocol: newline-delimited JSON frames over one tailcat TCP
// stream; see internal/server/session.go.

import { renderChat, statusHTML } from "./bob.js";

const DEFAULT_DERP_MAP = "https://tailcat.dev/derpmap.json";

// Stamped by web/build.sh; unbuilt sources show "dev".
const APP_COMMIT = "__APP_COMMIT__";
const APP_BUILT = "__APP_BUILT__";
const appVersion = APP_COMMIT.startsWith("__") ? "dev" : APP_COMMIT;

// ---------- diagnostics ----------
// A small event log kept in localStorage so it survives reloads and a page
// that iOS killed in the background. Shown under ⋯ → Diagnostics.
const diag = {
  KEY: "ray.diag",
  MAX: 300,
  read() {
    try { return JSON.parse(localStorage.getItem(this.KEY)) || []; } catch { return []; }
  },
  log(ev, info) {
    try {
      const l = this.read();
      l.push({ t: Date.now(), ev, ...(info ? { i: info } : {}) });
      localStorage.setItem(this.KEY, JSON.stringify(l.slice(-this.MAX)));
    } catch {}
  },
  clear() { try { localStorage.removeItem(this.KEY); } catch {} },
  text() {
    const l = this.read();
    const fmt = (t) => new Date(t).toLocaleTimeString([], { hour12: false }) + "." + String(t % 1000).padStart(3, "0");
    return l.map((e, k) => {
      const gap = k ? ` (+${((e.t - l[k - 1].t) / 1000).toFixed(1)}s)` : "";
      return `${fmt(e.t)}${gap} ${e.ev}${e.i ? " " + (typeof e.i === "string" ? e.i : JSON.stringify(e.i)) : ""}`;
    }).join("\n");
  },
};
{
  const prev = diag.read().at(-1);
  diag.log("boot", {
    v: appVersion,
    standalone: !!(navigator.standalone || matchMedia("(display-mode: standalone)").matches),
    prev: prev ? prev.ev : null, // what the last session did before this boot
    nav: performance.getEntriesByType?.("navigation")?.[0]?.type,
    h: innerHeight,
    vv: Math.round(visualViewport?.height || 0),
  });
}
const $ = (id) => document.getElementById(id);

// ---------- storage ----------

const store = {
  get(k, def) {
    try { const v = localStorage.getItem("ray." + k); return v == null ? def : JSON.parse(v); } catch { return def; }
  },
  set(k, v) {
    try { localStorage.setItem("ray." + k, JSON.stringify(v)); } catch {}
  },
};

const prefs = Object.assign({ wrap: false, font: 12, live: false, bobView: "chat" }, store.get("prefs", {}));
const savePrefs = () => store.set("prefs", prefs);

let hosts = store.get("hosts", []); // [{addr, name, derp, pair}]
const saveHosts = () => store.set("hosts", hosts);
let currentAddr = store.get("current", hosts[0]?.addr || null);

function currentHost() { return hosts.find((h) => h.addr === currentAddr) || null; }

function deviceName() {
  const ua = navigator.userAgent;
  if (/iPhone/.test(ua)) return "iPhone";
  if (/iPad/.test(ua) || (/Macintosh/.test(ua) && navigator.maxTouchPoints > 1)) return "iPad";
  if (/Android/.test(ua)) return "Android";
  return "Browser";
}

// ---------- pairing links ----------

// A pairing link looks like https://…/ray/#c=<tailcat addr>&p=<code>&h=<host>[&d=<derp map>]
function parsePairLink(text) {
  let hash = text;
  try { hash = new URL(text, location.href).hash; } catch {}
  hash = hash.replace(/^#/, "");
  const q = new URLSearchParams(hash);
  const addr = q.get("c");
  if (!addr || !addr.startsWith("tc")) return null;
  return { addr, pair: q.get("p") || "", name: q.get("h") || "Mac", derp: q.get("d") || "" };
}

function adoptPairLink(p) {
  const h = hosts.find((x) => x.addr === p.addr);
  if (h) {
    if (p.pair) h.pair = p.pair;
    if (p.name) h.name = p.name;
    if (p.derp) h.derp = p.derp;
  } else {
    hosts.push({ addr: p.addr, name: p.name, derp: p.derp, pair: p.pair });
  }
  saveHosts();
  currentAddr = p.addr;
  store.set("current", currentAddr);
}

{
  const p = parsePairLink(location.hash);
  if (p) {
    adoptPairLink(p);
    // Keep the address and code out of history and screenshots.
    history.replaceState(null, "", location.pathname + location.search);
  }
}

// ---------- viewport (iOS keyboard) ----------

// iOS doesn't shrink the layout for the on-screen keyboard, so the app is
// sized to the visual viewport. That height is only trusted while a text
// field has focus; otherwise use the full window, so a stale keyboard-era
// height (e.g. the app was backgrounded with the keyboard up) can't leave
// the app short with a strip of page showing below it.
function fitViewport() {
  const vv = window.visualViewport;
  const typing = document.activeElement?.matches?.("textarea, input");
  const h = vv && typing ? vv.height : Math.max(vv?.height || 0, window.innerHeight);
  document.documentElement.style.setProperty("--vh", h + "px");
  if (vv?.offsetTop) window.scrollTo(0, 0);
}

// After being backgrounded, iOS standalone web apps sometimes come back
// with the page unpainted (a black screen until reload). Re-measure and
// nudge the compositor to redraw everything.
function repaint() {
  fitViewport();
  const app = document.getElementById("app");
  if (!app) return;
  app.style.transform = "translateZ(0)";
  void app.offsetHeight; // force layout
  requestAnimationFrame(() => {
    app.style.transform = "";
    fitViewport();
  });
}

if (window.visualViewport) {
  window.visualViewport.addEventListener("resize", fitViewport);
  window.visualViewport.addEventListener("scroll", fitViewport);
}
window.addEventListener("resize", fitViewport);
window.addEventListener("orientationchange", () => setTimeout(repaint, 300));
window.addEventListener("pageshow", (e) => {
  // Restored from the back-forward cache: same stale state as a long
  // background, so start fresh.
  diag.log("pageshow", { persisted: e.persisted });
  if (e.persisted) return reloadFresh("bfcache");
  repaint();
});
window.addEventListener("focus", repaint);
// After a while in the background, iOS can hand the app back unpainted (a
// black screen until reload), and the tailcat connection is dead by then
// anyway. So past a short absence, reload instead of trying to recover in
// place. The selected terminal is remembered; the composer draft is kept.
const RELOAD_AFTER_HIDDEN_MS = 45 * 1000;
// A frozen timer this long means iOS suspended the page; see the heartbeat.
const RELOAD_AFTER_STALL_MS = 20 * 1000;
let hiddenAt = 0;
function reloadFresh(reason) {
  diag.log("reload", reason || "");
  try {
    const draft = document.getElementById("input")?.value;
    if (draft) sessionStorage.setItem("ray.draft", draft);
  } catch {}
  location.reload();
}
document.addEventListener("visibilitychange", () => {
  if (document.hidden) {
    hiddenAt = Date.now();
    diag.log("hidden");
    return;
  }
  const away = hiddenAt ? Date.now() - hiddenAt : 0;
  hiddenAt = 0;
  diag.log("visible", { away: Math.round(away / 1000) + "s", h: innerHeight, vv: Math.round(visualViewport?.height || 0) });
  if (away > RELOAD_AFTER_HIDDEN_MS) return reloadFresh("away " + Math.round(away / 1000) + "s");
  repaint();
});
window.addEventListener("pagehide", (e) => diag.log("pagehide", { persisted: e.persisted }));
document.addEventListener("freeze", () => diag.log("freeze"));
document.addEventListener("resume", () => diag.log("resume"));

// Heartbeat: if timers stopped for a long stretch, treat it like a long
// background on the next tick (iOS doesn't always send visibilitychange).
{
  let beat = Date.now();
  setInterval(() => {
    const now = Date.now();
    const gap = now - beat;
    beat = now;
    if (gap > 10000) {
      diag.log("stall", { gap: Math.round(gap / 1000) + "s", hidden: document.hidden, vis: document.visibilityState, focus: document.hasFocus() });
      // Seen on iOS: on return the page's timers resume but it still reports
      // hidden, gets no visibilitychange, and isn't repainted (black screen).
      // So reload after any long freeze, whatever visibility claims; if we
      // really are in the background, the reload is harmless.
      if (gap > RELOAD_AFTER_STALL_MS) reloadFresh("stall " + Math.round(gap / 1000) + "s");
    }
  }, 2000);
}
// The keyboard closing doesn't always fire a viewport resize on iOS.
document.addEventListener("focusout", () => setTimeout(fitViewport, 50));
document.addEventListener("focusin", () => setTimeout(fitViewport, 50));
fitViewport();

// ---------- wasm boot ----------

async function fetchWasm(onProgress) {
  const counted = (resp, total) => {
    let loaded = 0;
    return resp.body.pipeThrough(new TransformStream({
      transform(chunk, c) {
        loaded += chunk.byteLength;
        if (total) onProgress(Math.min(1, loaded / total));
        c.enqueue(chunk);
      },
    }));
  };
  // GitHub Pages doesn't compress .wasm, so ship it pre-gzipped.
  if ("DecompressionStream" in window) {
    const gz = await fetch("ray.wasm.gz");
    if (gz.ok) {
      const total = Number(gz.headers.get("Content-Length")) || 0;
      const body = counted(gz, total).pipeThrough(new DecompressionStream("gzip"));
      return new Response(body, { headers: { "Content-Type": "application/wasm" } });
    }
  }
  const resp = await fetch("ray.wasm");
  if (!resp.ok) throw new Error(`fetching ray.wasm: ${resp.status}`);
  const total = Number(resp.headers.get("Content-Length")) || 0;
  return new Response(counted(resp, total), { headers: { "Content-Type": "application/wasm" } });
}

async function bootWasm() {
  const bar = $("loader-bar");
  const text = $("loader-text");
  text.textContent = "Loading tailcat…";
  const ready = new Promise((resolve) => { globalThis.onRayWasmReady = resolve; });
  const go = new Go();
  const { instance } = await WebAssembly.instantiateStreaming(
    fetchWasm((f) => { bar.value = f; text.textContent = `Loading tailcat… ${Math.floor(f * 100)}%`; }),
    go.importObject,
  );
  go.run(instance);
  await ready;
}

function deviceKey() {
  let k = store.get("device", null);
  if (!k || !k.privateKeyJSON) {
    k = rayNewKey();
    store.set("device", k);
  }
  return k;
}

// ---------- connection ----------

class Link {
  constructor(host, handlers) {
    this.host = host;
    this.h = handlers;
    this.state = "idle";
    this.conn = null;
    this.stopped = false;
    this.backoff = 1000;
    this.wq = Promise.resolve();
    this.enc = new TextEncoder();
    this.nextId = 1;
    this.pending = new Map();
    this.timer = null;
  }

  setState(s, detail) {
    this.state = s;
    this.h.onState(s, detail);
  }

  start() {
    this.stopped = false;
    this.connect();
  }

  stop() {
    this.stopped = true;
    clearTimeout(this.timer);
    this.drop();
  }

  // kick reconnects right away if we're waiting to retry.
  kick() {
    if (this.stopped || this.state === "unauthorized") return;
    if (this.state === "offline") {
      clearTimeout(this.timer);
      this.backoff = 1000;
      this.connect();
    } else if (this.state === "online") {
      this.ping();
    }
  }

  drop() {
    const c = this.conn;
    this.conn = null;
    clearInterval(this.hb);
    if (c) { try { c.close(); } catch {} }
    for (const [, p] of this.pending) p.reject(new Error("disconnected"));
    this.pending.clear();
  }

  async connect() {
    if (this.stopped) return;
    this.drop();
    this.setState("connecting");
    const key = deviceKey();
    let conn;
    try {
      conn = await rayDial({
        addr: this.host.addr,
        privateKey: key.privateKeyJSON,
        derpMapURL: this.host.derp || DEFAULT_DERP_MAP,
        port: 1,
        verbose: new URLSearchParams(location.search).has("verbose"),
      });
    } catch (e) {
      return this.retry(String(e.message || e));
    }
    if (this.stopped) { conn.close(); return; }
    this.conn = conn;
    this.wq = Promise.resolve();
    this.lastRx = Date.now();
    this.write({ t: "hello", v: 1, name: deviceName(), pair: this.host.pair || undefined });
    this.hb = setInterval(() => {
      if (Date.now() - this.lastRx > 40000) {
        this.retry("connection timed out");
      } else {
        this.ping();
      }
    }, 15000);
    this.readLoop(conn);
  }

  retry(why) {
    if (this.stopped) return;
    this.drop();
    this.setState("offline", why);
    clearTimeout(this.timer);
    this.timer = setTimeout(() => this.connect(), this.backoff);
    this.backoff = Math.min(this.backoff * 2, 15000);
  }

  async readLoop(conn) {
    const dec = new TextDecoder();
    let buf = "";
    try {
      for (;;) {
        const chunk = await conn.read();
        if (conn !== this.conn) return;
        if (chunk === null) break;
        this.lastRx = Date.now();
        buf += dec.decode(chunk, { stream: true });
        let i;
        while ((i = buf.indexOf("\n")) >= 0) {
          const line = buf.slice(0, i);
          buf = buf.slice(i + 1);
          if (line.trim()) this.onFrame(JSON.parse(line));
        }
      }
      if (conn === this.conn && this.state !== "unauthorized") this.retry("server closed the connection");
    } catch (e) {
      if (conn === this.conn) this.retry(String(e.message || e));
    }
  }

  onFrame(m) {
    switch (m.t) {
      case "welcome":
        this.backoff = 1000;
        serverVersion = m.version || "";
        renderVersion();
        if (this.host.pair) {
          delete this.host.pair; // one-time code consumed
          saveHosts();
        }
        if (m.host && !this.host.renamed) { this.host.name = m.host; saveHosts(); }
        this.setState("online", m);
        break;
      case "error":
        if (m.code === "unauthorized") {
          this.stopped = true;
          this.drop();
          this.setState("unauthorized", m.error);
        } else {
          this.h.onNotice(m.error || "server error");
        }
        break;
      case "ack": {
        const p = this.pending.get(m.id);
        if (p) {
          this.pending.delete(m.id);
          m.error ? p.reject(new Error(m.error)) : p.resolve(m);
        }
        break;
      }
      case "pong":
        break;
      default:
        this.h.onFrame(m);
    }
  }

  write(obj) {
    if (!this.conn) return false;
    const conn = this.conn;
    const bytes = this.enc.encode(JSON.stringify(obj) + "\n");
    // Writes must be serialized: each one runs on its own goroutine.
    this.wq = this.wq.then(() => conn.write(bytes)).catch((e) => {
      if (conn === this.conn) this.retry(String(e.message || e));
    });
    return true;
  }

  // request sends a frame and resolves on its ack.
  request(obj) {
    const id = this.nextId++;
    return new Promise((resolve, reject) => {
      if (!this.write({ ...obj, id })) return reject(new Error("not connected"));
      this.pending.set(id, { resolve, reject });
      setTimeout(() => {
        if (this.pending.delete(id)) reject(new Error("timed out"));
      }, 20000);
    });
  }

  ping() { this.write({ t: "ping" }); }
}

// ---------- UI state ----------

const ui = {
  link: null,
  tree: [],
  sel: null, // {ws, sf}
  screens: new Map(), // "ws/sf" -> lines
  chats: new Map(), // "ws/sf" -> bob session {task, state, msgs, approval, error}
  ctrl: false,
};

const termEl = $("term");
const inputEl = $("input");

function applyPrefs() {
  document.documentElement.style.setProperty("--term-size", prefs.font + "px");
  termEl.classList.toggle("wrap", prefs.wrap);
  $("mode-btn").setAttribute("aria-pressed", String(prefs.live));
  $("mode-btn").textContent = prefs.live ? "live" : "line";
  inputEl.placeholder = prefs.live ? "Live: keys go straight to the terminal" : isBobSel?.() ? "Message bob…" : "Message terminal…";
  $("send-btn").hidden = prefs.live;
  for (const b of $("opts-menu").querySelectorAll("button")) {
    if (b.dataset.act === "wrap") b.setAttribute("aria-checked", String(prefs.wrap));
  }
}

function setDrawer(open) { $("app").classList.toggle("drawer", open); }

function showBanner(text, kind) {
  const b = $("banner");
  if (!text) { b.hidden = true; return; }
  b.hidden = false;
  b.className = "banner" + (kind ? " " + kind : "");
  b.textContent = text;
}

function setConnState(s, detail) {
  const dot = $("host-dot");
  dot.className = "dot " + ({ online: "ok", connecting: "wait", offline: "err", unauthorized: "err" }[s] || "");
  const label = {
    connecting: "Connecting…",
    online: "Connected · relayed, end-to-end encrypted",
    offline: "Offline — retrying",
    unauthorized: "Not paired",
    idle: "Not connected",
  }[s] || s;
  $("conn-label").textContent = label;
  $("composer").classList.toggle("disabled", s !== "online" || !ui.sel || !!ui.sel.info);
  if (s === "online") showBanner(null);
  else if (s === "offline") showBanner(`Can't reach ${currentHost()?.name || "the Mac"}: ${detail || "offline"}. Retrying…`);
  else if (s === "unauthorized") {
    showBanner(`This phone isn't paired with ${currentHost()?.name || "this Mac"}. Run “ray pair” on the Mac and scan the new code.`, "err");
  } else if (s === "connecting" && !ui.tree.length) showBanner(null);
  updateTitle();
}

function updateTitle() {
  const h = currentHost();
  $("host-name").textContent = h ? h.name : "ray";
  const ws = ui.tree.find((w) => w.id === ui.sel?.ws);
  $("title").textContent = ws ? (ws.title || "Workspace") : (h ? h.name : "ray");
  $("subtitle").textContent = ws ? [ws.group && !ws.group.anchor ? ws.group.name : null, h?.name].filter(Boolean).join(" · ") : "";
  renderTabs();
  document.title = ws ? `${ws.title} · ray` : "ray";
}

const termGlyph = '<svg class="glyph" viewBox="0 0 20 20" aria-hidden="true"><rect x="2.5" y="3.5" width="15" height="13" rx="2.5" fill="none" stroke="currentColor" stroke-width="1.5"/><path d="M6 8l2.5 2L6 12M10 12.5h4" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>';
// A chat bubble with a spark: terminals running IBM Bob Shell.
const bobGlyph = '<svg class="glyph" viewBox="0 0 20 20" aria-hidden="true"><path d="M4.5 4h11a2 2 0 0 1 2 2v6.5a2 2 0 0 1-2 2H9l-3.5 3v-3h-1a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2z" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round"/><path d="M10 6.6l.8 1.8 1.8.8-1.8.8-.8 1.8-.8-1.8-1.8-.8 1.8-.8z" fill="currentColor"/></svg>';
const webGlyph = '<svg class="glyph" viewBox="0 0 20 20" aria-hidden="true"><circle cx="10" cy="10" r="7" fill="none" stroke="currentColor" stroke-width="1.5"/><path d="M3 10h14M10 3c2.5 2.5 2.5 11.5 0 14M10 3c-2.5 2.5-2.5 11.5 0 14" fill="none" stroke="currentColor" stroke-width="1.2"/></svg>';

const isTerminal = (s) => !s.type || /term/i.test(s.type);

// Collapsed groups, per host; seeded from cmux's own collapsed state.
function isCollapsed(g) {
  const m = store.get("collapsed." + currentAddr, {});
  return g.id in m ? m[g.id] : !!g.collapsed;
}
function setCollapsed(g, v) {
  const m = store.get("collapsed." + currentAddr, {});
  m[g.id] = v;
  store.set("collapsed." + currentAddr, m);
}

const folderGlyph = '<svg class="glyph" viewBox="0 0 20 20" aria-hidden="true"><path d="M2.5 6.2c0-1 .8-1.7 1.7-1.7h3.4l1.7 1.8h6.5c1 0 1.7.8 1.7 1.7v6.8c0 1-.8 1.7-1.7 1.7H4.2c-1 0-1.7-.8-1.7-1.7z" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round"/></svg>';
const chevGlyph = '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M6 4l4 4-4 4" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>';

// openWorkspace shows a workspace's focused terminal (or explains why it
// can't).
function openWorkspace(ws) {
  const terms = (ws.surfaces || []).filter(isTerminal);
  if ((ws.surfaces || []).length > 0 && !terms.length) return showInfo(ws);
  const s = terms.find((s) => s.focused) || terms[0];
  // With no terminal listed (cmux couldn't list the workspace), watch the
  // workspace itself: cmux reads its focused terminal, and any failure
  // shows in the banner.
  select(ws.id, s ? s.id : "");
  if (!s && ws.error) showBanner(`${ws.title || "Workspace"}: ${ws.error}`, "err");
}

function rowBadges(row, ws) {
  if (ws.selected) {
    const f = document.createElement("span");
    f.className = "front";
    f.title = "Frontmost on the Mac";
    row.append(f);
  }
  const n = (ws.surfaces || []).length;
  if (n > 1) {
    const b = document.createElement("span");
    b.className = "badge";
    b.textContent = n;
    b.title = `${n} tabs`;
    row.append(b);
  }
  if (ws.error) {
    const b = document.createElement("span");
    b.className = "badge warn";
    b.textContent = "!";
    row.append(b);
    row.title = ws.error;
  }
}

function workspaceRow(ws) {
  const terms = (ws.surfaces || []).filter(isTerminal);
  // Listed, but nothing ray can stream (e.g. only browser tabs).
  const noTerms = (ws.surfaces || []).length > 0 && terms.length === 0;
  const row = document.createElement("button");
  row.className = "row" + (noTerms ? " muted" : "") + (ui.sel?.ws === ws.id ? " active" : "");
  row.innerHTML = noTerms ? webGlyph : (ws.surfaces || []).some((x) => x.bob) ? bobGlyph : termGlyph;
  const l = document.createElement("span");
  l.className = "label";
  l.textContent = ws.title || `Workspace ${ws.index + 1}`;
  row.append(l);
  rowBadges(row, ws);
  row.onclick = () => openWorkspace(ws);
  return row;
}

// groupBlock renders a cmux sidebar group: a header (which is itself a
// workspace, the anchor, when cmux has one) and its member workspaces.
function groupBlock(g, anchor, members) {
  const wrap = document.createElement("div");
  wrap.className = "group";
  const collapsed = isCollapsed(g);
  const head = document.createElement("div");
  head.className = "group-head" + (anchor && ui.sel?.ws === anchor.id ? " active" : "");
  const chev = document.createElement("button");
  chev.className = "chev-btn" + (collapsed ? "" : " open");
  chev.setAttribute("aria-label", collapsed ? `Expand ${g.name}` : `Collapse ${g.name}`);
  chev.setAttribute("aria-expanded", String(!collapsed));
  chev.innerHTML = chevGlyph;
  chev.onclick = (e) => {
    e.stopPropagation();
    setCollapsed(g, !collapsed);
    renderTree();
  };
  const row = document.createElement("button");
  row.className = "row group-row";
  row.innerHTML = folderGlyph;
  const l = document.createElement("span");
  l.className = "label";
  l.textContent = g.name || anchor?.title || "Group";
  row.append(l);
  if (anchor) rowBadges(row, anchor);
  if (anchor && (anchor.surfaces || []).some((x) => x.bob)) {
    const m = document.createElement("span");
    m.className = "bobmark";
    m.title = "Running bob";
    m.innerHTML = bobGlyph;
    row.insertBefore(m, row.querySelector(".label").nextSibling);
  }
  // The header opens the anchor's own terminals when it has any (as in
  // cmux); otherwise it just folds the group.
  const anchorHasTabs = anchor && (anchor.surfaces || []).length > 0;
  row.onclick = () => {
    if (anchorHasTabs) openWorkspace(anchor);
    else { setCollapsed(g, !collapsed); renderTree(); }
  };
  head.append(chev, row);
  wrap.append(head);
  if (!collapsed) {
    const list = document.createElement("div");
    list.className = "members";
    for (const ws of members) list.append(workspaceRow(ws));
    wrap.append(list);
  }
  return wrap;
}

function renderTree() {
  const nav = $("tree");
  nav.textContent = "";
  if (!ui.tree.length) {
    const p = document.createElement("div");
    p.className = "tree-label";
    p.textContent = ui.link?.state === "online" ? "No workspaces" : "";
    nav.append(p);
    return;
  }
  const label = document.createElement("div");
  label.className = "tree-label";
  label.textContent = "Workspaces";
  nav.append(label);
  // Keep cmux's order: a group appears where its first workspace does.
  const done = new Set();
  for (const ws of ui.tree) {
    const g = ws.group;
    if (!g) {
      nav.append(workspaceRow(ws));
      continue;
    }
    if (done.has(g.id)) continue;
    done.add(g.id);
    const inGroup = ui.tree.filter((w) => w.group?.id === g.id);
    const anchor = inGroup.find((w) => w.group.anchor) || null;
    nav.append(groupBlock(g, anchor, inGroup.filter((w) => w !== anchor)));
  }
  renderTabs();
}

// renderTabs shows the selected workspace's tabs above the terminal, like
// cmux's tab bar, when it has more than one.
function renderTabs() {
  const bar = $("tabs");
  const ws = ui.tree.find((w) => w.id === ui.sel?.ws);
  const surfaces = ws?.surfaces || [];
  bar.textContent = "";
  bar.hidden = surfaces.length < 2 || !!ui.sel?.info;
  if (bar.hidden) return;
  for (const s of surfaces) {
    const b = document.createElement("button");
    const term = isTerminal(s);
    b.className = "tab" + (ui.sel.sf === s.id ? " active" : "");
    b.innerHTML = s.bob ? bobGlyph : term ? termGlyph : webGlyph;
    const l = document.createElement("span");
    l.textContent = s.title || (term ? "terminal" : s.type);
    b.append(l);
    if (term) b.onclick = () => select(ws.id, s.id);
    else {
      b.disabled = true;
      b.title = "ray can only show terminals";
    }
    bar.append(b);
  }
  bar.querySelector(".active")?.scrollIntoView({ block: "nearest", inline: "nearest" });
}

function renderHostMenu() {
  const m = $("host-menu");
  m.textContent = "";
  for (const h of hosts) {
    const b = document.createElement("button");
    b.innerHTML = `<span class="dot ${h.addr === currentAddr ? "ok" : ""}"></span>`;
    b.append(document.createTextNode(h.name || "Mac"));
    b.onclick = () => { m.hidden = true; switchHost(h.addr); };
    m.append(b);
  }
  const sep = document.createElement("div");
  sep.className = "sep";
  m.append(sep);
  const add = document.createElement("button");
  add.textContent = "Pair another Mac…";
  add.onclick = () => { m.hidden = true; showWelcome(true); setDrawer(false); };
  m.append(add);
  if (currentHost()) {
    const forget = document.createElement("button");
    forget.className = "danger";
    forget.textContent = `Forget ${currentHost().name}`;
    forget.onclick = () => {
      m.hidden = true;
      if (!confirm(`Forget ${currentHost().name}? You'll need to pair again.`)) return;
      hosts = hosts.filter((h) => h.addr !== currentAddr);
      saveHosts();
      switchHost(hosts[0]?.addr || null);
    };
    m.append(forget);
  }
}

function showWelcome(on) {
  $("welcome").hidden = !on;
  $("term-wrap").hidden = on;
  $("composer").hidden = on;
}

// ---------- terminal view ----------

// Empty ids are omitted on the wire, so normalize undefined to "".
const key = (ws, sf) => (ws || "") + "/" + (sf || "");

function atBottom() {
  return termEl.scrollHeight - termEl.scrollTop - termEl.clientHeight < 40;
}

function renderScreen() {
  const empty = $("empty");
  if (!ui.sel || ui.sel.info) {
    $("bobbar").hidden = true;
    $("chat").hidden = true;
    termEl.hidden = false;
  }
  if (!ui.sel) {
    termEl.textContent = "";
    empty.hidden = false;
    empty.textContent = ui.link?.state === "online" ? "Pick a terminal from the sidebar." : "";
    return;
  }
  if (ui.sel.info) {
    const ws = ui.tree.find((w) => w.id === ui.sel.ws);
    const kinds = [...new Set((ws?.surfaces || []).map((s) => s.type || "panel"))].join(" and ");
    termEl.textContent = "";
    empty.hidden = false;
    empty.textContent = `${ws?.title || "This workspace"} only has ${kinds || "non-terminal"} tabs. ray can only show terminals. Use ⋯ → Show on Mac to bring it up there.`;
    $("jump-btn").hidden = true;
    return;
  }
  const bobOn = isBobSel();
  $("bobbar").hidden = !bobOn;
  if (!prefs.live) inputEl.placeholder = bobOn ? "Message bob…" : "Message terminal…";
  const chatOn = bobOn && prefs.bobView === "chat";
  $("chat").hidden = !chatOn;
  termEl.hidden = chatOn;
  if (bobOn) renderBobBar();
  if (chatOn) {
    empty.hidden = true;
    renderBob();
    return;
  }
  const lines = ui.screens.get(key(ui.sel.ws, ui.sel.sf));
  empty.hidden = !!lines;
  if (!lines) {
    empty.textContent = "Loading…";
    termEl.textContent = "";
    return;
  }
  const stick = atBottom() || termEl.dataset.fresh === "1";
  termEl.textContent = lines.join("\n");
  termEl.dataset.fresh = "0";
  if (stick) termEl.scrollTop = termEl.scrollHeight;
  $("jump-btn").hidden = atBottom();
}

// ---------- bob chat ----------

function selSurface() {
  const ws = ui.tree.find((w) => w.id === ui.sel?.ws);
  return ws?.surfaces?.find((x) => x.id === ui.sel?.sf) || null;
}

// isBobSel: the selected terminal is running IBM Bob Shell.
function isBobSel() {
  return !!(ui.sel && !ui.sel.info && selSurface()?.bob);
}

function onChat(m) {
  const k = key(m.ws, m.sf);
  const f = m.chat || {};
  const c = ui.chats.get(k) || { msgs: [] };
  if (f.error) {
    c.error = f.error;
  } else {
    c.error = null;
    if (f.reset) c.msgs = [];
    const last = c.msgs.length ? c.msgs[c.msgs.length - 1].seq : -1;
    for (const x of f.msgs || []) if (x.seq > last) c.msgs.push(x);
    if (c.msgs.length > 400) c.msgs = c.msgs.slice(-400);
    c.task = f.task || c.task;
    c.state = f.state || c.state;
    c.approval = f.approval || null;
  }
  ui.chats.set(k, c);
  if (ui.sel && k === key(ui.sel.ws, ui.sel.sf)) {
    if (isBobSel()) renderBobBar();
    if (isBobSel() && prefs.bobView === "chat") renderBob();
  }
}

function renderBobBar() {
  $("bob-status").innerHTML = statusHTML(ui.chats.get(key(ui.sel.ws, ui.sel.sf)));
  for (const b of $("bob-view").querySelectorAll("button")) b.classList.toggle("on", b.dataset.view === prefs.bobView);
}

function renderBob() {
  const el = $("chat");
  const chat = ui.chats.get(key(ui.sel.ws, ui.sel.sf));
  // Follow new output when already at the bottom, and always bring a new
  // approval prompt into view.
  const approval = chat?.approval?.request_id || "";
  const stick = el.scrollHeight - el.scrollTop - el.clientHeight < 60 || el.dataset.fresh !== "0" || (approval && approval !== el.dataset.approval);
  el.dataset.approval = approval;
  renderChat(el, chat);
  el.dataset.fresh = "0";
  if (stick) el.scrollTop = el.scrollHeight;
}

$("bob-view").addEventListener("click", (e) => {
  const v = e.target.closest("button")?.dataset.view;
  if (!v) return;
  prefs.bobView = v;
  savePrefs();
  applyPrefs();
  $("chat").dataset.fresh = "1";
  termEl.dataset.fresh = "1";
  renderScreen();
});

$("chat").addEventListener("click", async (e) => {
  const b = e.target.closest("button[data-approve]");
  if (!b || !ui.sel) return;
  haptic();
  const buttons = $("chat").querySelectorAll("button[data-approve]");
  buttons.forEach((x) => { x.disabled = true; });
  b.textContent = "Sending…";
  try {
    await ui.link?.request({ t: "approve", ws: ui.sel.ws, sf: ui.sel.sf, data: b.dataset.approve });
  } catch (err) {
    showBanner("Couldn't answer bob: " + err.message, "err");
    buttons.forEach((x) => { x.disabled = false; });
  }
});

// showInfo selects a workspace that has no terminal to stream and says so
// in the main pane, instead of asking cmux to read a browser tab.
function showInfo(ws) {
  ui.sel = { ws: ws.id, sf: "", info: true };
  ui.link?.write({ t: "watch" }); // stop streaming the previous terminal
  clearErrorBanner();
  renderTree();
  renderScreen();
  updateTitle();
  setDrawer(false);
  $("composer").classList.add("disabled");
}

// clearErrorBanner hides a banner about a terminal's errors, but leaves
// connection banners (offline, not paired) alone since they apply everywhere.
function clearErrorBanner() {
  if (ui.link?.state === "online") showBanner(null);
}

function select(ws, sf) {
  ui.sel = { ws, sf };
  $("chat").dataset.fresh = "1";
  clearErrorBanner();
  store.set("sel." + currentAddr, ui.sel);
  termEl.dataset.fresh = "1";
  ui.link?.write({ t: "watch", ws, sf });
  renderTree();
  renderScreen();
  updateTitle();
  setDrawer(false);
  $("composer").classList.toggle("disabled", ui.link?.state !== "online");
}

function onTree(workspaces) {
  ui.tree = workspaces || [];
  // Keep the current selection if it still exists; otherwise restore the
  // last one for this host, else follow what's frontmost on the Mac.
  const exists = (s) => s && ui.tree.some((w) => w.id === s.ws && (!s.sf || (w.surfaces || []).some((x) => x.id === s.sf)));
  if (!exists(ui.sel)) {
    let next = store.get("sel." + currentAddr, null);
    if (!exists(next)) {
      const ws = ui.tree.find((w) => w.selected) || ui.tree[0];
      const terms = (ws?.surfaces || []).filter(isTerminal);
      const sf = terms.find((s) => s.focused) || terms[0];
      next = ws && sf ? { ws: ws.id, sf: sf.id } : null;
    }
    if (next) select(next.ws, next.sf);
    else { ui.sel = null; renderScreen(); }
  }
  renderTree();
  updateTitle();
}

// onNotice shows (or, with no error, clears) a server notice. Notices about
// a terminal only count while that terminal is the one on screen, so a
// late error from the tab you just left doesn't follow you.
function onNotice(m) {
  if (m.ws || m.sf) {
    if (!ui.sel || ui.sel.info || key(m.ws, m.sf) !== key(ui.sel.ws, ui.sel.sf)) return;
  }
  showBanner(m.error || null, "err");
}

function onScreen(m) {
  const k = key(m.ws, m.sf);
  const prev = ui.screens.get(k) || [];
  const drop = m.drop || 0;
  const keep = m.keep || 0;
  ui.screens.set(k, prev.slice(drop, drop + keep).concat(m.lines || []));
  if (ui.sel && k === key(ui.sel.ws, ui.sel.sf)) renderScreen();
}

termEl.addEventListener("scroll", () => { $("jump-btn").hidden = atBottom(); }, { passive: true });

// No page zoom (iOS ignores user-scalable=no in some modes, so stop its
// pinch gestures directly). Pinching the terminal resizes its text instead.
{
  let startFont = 0;
  const onTerm = (e) => $("term-wrap").contains(e.target);
  document.addEventListener("gesturestart", (e) => {
    e.preventDefault();
    startFont = onTerm(e) ? prefs.font : 0;
  }, { passive: false });
  document.addEventListener("gesturechange", (e) => {
    e.preventDefault();
    if (!startFont) return;
    const f = Math.max(8, Math.min(22, Math.round(startFont * e.scale)));
    if (f !== prefs.font) {
      prefs.font = f;
      applyPrefs();
    }
  }, { passive: false });
  document.addEventListener("gestureend", (e) => {
    e.preventDefault();
    if (startFont) savePrefs();
    startFont = 0;
  }, { passive: false });
  // Other browsers: block multi-touch page zoom.
  document.addEventListener("touchmove", (e) => {
    if (e.touches.length > 1) e.preventDefault();
  }, { passive: false });
}
$("jump-btn").onclick = () => { termEl.scrollTop = termEl.scrollHeight; };

// ---------- input ----------

function sendText(data) {
  if (!ui.sel || !data) return;
  ui.link?.write({ t: "text", ws: ui.sel.ws, sf: ui.sel.sf, data });
}
function sendKey(k) {
  if (!ui.sel) return;
  ui.link?.write({ t: "key", ws: ui.sel.ws, sf: ui.sel.sf, key: k });
}
function haptic() { try { navigator.vibrate?.(8); } catch {} }

function setCtrl(on) {
  ui.ctrl = on;
  $("ctrl-key").classList.toggle("on", on);
}

// ctrlChar maps a typed character to its control code (ctrl-a → 0x01).
function ctrlChar(ch) {
  const c = ch.toUpperCase().charCodeAt(0);
  if (c >= 64 && c <= 95) return String.fromCharCode(c - 64);
  if (ch === " ") return "\u0000";
  return null;
}

for (const b of $("keys").querySelectorAll("button")) {
  // Keep the keyboard up: don't let the button steal focus.
  b.addEventListener("pointerdown", (e) => e.preventDefault());
  b.addEventListener("click", () => {
    haptic();
    if (b.dataset.mod === "ctrl") return setCtrl(!ui.ctrl);
    if (b.dataset.key) sendKey(b.dataset.key);
    else if (b.dataset.raw) sendText(JSON.parse(`"${b.dataset.raw}"`));
    setCtrl(false);
  });
}

function autosize() {
  inputEl.style.height = "auto";
  inputEl.style.height = Math.min(inputEl.scrollHeight, 140) + "px";
}

$("input-form").addEventListener("submit", (e) => {
  e.preventDefault();
  submitLine();
});

function submitLine() {
  $("chat").dataset.fresh = "1"; // jump to the bottom to follow the reply
  const text = inputEl.value;
  if (text) sendText(text);
  sendKey("enter");
  inputEl.value = "";
  autosize();
  termEl.dataset.fresh = "1";
}

// Text arriving in the composer: in live mode it goes straight out; in
// either mode a pending ctrl modifier turns the next character into a
// control code.
inputEl.addEventListener("beforeinput", (e) => {
  const t = e.inputType;
  if (ui.ctrl && t === "insertText" && e.data) {
    e.preventDefault();
    const c = ctrlChar(e.data[0]);
    if (c != null) sendText(c);
    setCtrl(false);
    return;
  }
  if (!prefs.live) {
    if (t === "insertLineBreak" || t === "insertParagraph") {
      e.preventDefault();
      submitLine();
    }
    return;
  }
  e.preventDefault();
  if (t === "insertText" || t === "insertReplacementText" || t === "insertFromPaste") {
    const data = e.data ?? e.dataTransfer?.getData("text/plain") ?? "";
    sendText(data);
  } else if (t === "insertLineBreak" || t === "insertParagraph") {
    sendKey("enter");
  } else if (t === "deleteContentBackward") {
    sendKey("backspace");
  } else if (t === "deleteContentForward") {
    sendKey("delete");
  }
});

// Some mobile keyboards (Android Gboard) don't fire cancellable
// beforeinput; catch anything that slipped into the box in live mode.
inputEl.addEventListener("input", () => {
  if (prefs.live && inputEl.value) {
    sendText(inputEl.value);
    inputEl.value = "";
  }
  autosize();
});

const hwKeys = { ArrowUp: "up", ArrowDown: "down", ArrowLeft: "left", ArrowRight: "right", Escape: "escape", Tab: "tab" };
inputEl.addEventListener("keydown", (e) => {
  if (e.isComposing) return;
  if (e.ctrlKey && e.key.length === 1) {
    const c = ctrlChar(e.key);
    if (c != null) { e.preventDefault(); sendText(c); }
    return;
  }
  if (prefs.live && (e.key === "Backspace" || e.key === "Enter")) {
    // iOS fires no beforeinput for backspace in an empty box.
    e.preventDefault();
    sendKey(e.key === "Enter" ? "enter" : "backspace");
    return;
  }
  if (prefs.live || !inputEl.value) {
    // Arrow keys etc. drive the terminal when there's no draft to edit.
    const k = hwKeys[e.key];
    if (k && !(e.key === "Tab" && e.shiftKey)) {
      e.preventDefault();
      sendKey(k);
      return;
    }
  }
  if (!prefs.live && e.key === "Enter" && !e.shiftKey) {
    e.preventDefault();
    submitLine();
  }
});

$("mode-btn").onclick = () => {
  prefs.live = !prefs.live;
  savePrefs();
  applyPrefs();
  if (prefs.live && inputEl.value) { inputEl.value = ""; autosize(); }
  inputEl.focus();
};

// ---------- chrome ----------

$("menu-btn").onclick = () => setDrawer(true);
$("scrim").onclick = () => setDrawer(false);
$("host-btn").onclick = () => {
  const m = $("host-menu");
  if (m.hidden) renderHostMenu();
  m.hidden = !m.hidden;
};
$("new-ws-btn").onclick = async () => {
  try { await ui.link?.request({ t: "new" }); } catch (e) { showBanner("New workspace failed: " + e.message, "err"); }
};
$("opts-btn").onclick = (e) => {
  e.stopPropagation();
  $("opts-menu").hidden = !$("opts-menu").hidden;
};
document.addEventListener("click", (e) => {
  if (!$("opts-menu").contains(e.target)) $("opts-menu").hidden = true;
});
$("opts-menu").addEventListener("click", async (e) => {
  const act = e.target.dataset?.act;
  if (!act) return;
  $("opts-menu").hidden = true;
  if (act === "wrap") prefs.wrap = !prefs.wrap;
  if (act === "font+") prefs.font = Math.min(prefs.font + 1, 22);
  if (act === "font-") prefs.font = Math.max(prefs.font - 1, 8);
  if (act === "reconnect") ui.link?.retry("reconnecting");
  if (act === "diag") return showDiagnostics();
  if (act === "focus" && ui.sel) {
    try { await ui.link?.request({ t: "focus", ws: ui.sel.ws, sf: ui.sel.sf }); } catch (err) { showBanner(err.message, "err"); }
  }
  savePrefs();
  applyPrefs();
  renderScreen();
});

$("paste-form").addEventListener("submit", (e) => {
  e.preventDefault();
  const p = parsePairLink($("paste-input").value.trim());
  if (!p) { showBanner("That doesn't look like a ray pairing link.", "err"); return; }
  $("paste-input").value = "";
  adoptPairLink(p);
  switchHost(p.addr);
});

// Coming back from the background (or the network returning) should
// reconnect immediately, not wait out the backoff.
document.addEventListener("visibilitychange", () => { if (!document.hidden) ui.link?.kick(); });
window.addEventListener("online", () => ui.link?.kick());
window.addEventListener("hashchange", () => {
  const p = parsePairLink(location.hash);
  if (!p) return;
  history.replaceState(null, "", location.pathname + location.search);
  adoptPairLink(p);
  switchHost(p.addr);
});

// ---------- diagnostics panel ----------

function showDiagnostics() {
  const panel = $("diag");
  const body = $("diag-text");
  const header = `ray app ${appVersion} · Mac ${serverVersion || "?"} · ${navigator.userAgent}\n`;
  body.textContent = header + (diag.text() || "(empty)");
  panel.hidden = false;
  body.scrollTop = body.scrollHeight;
}
$("diag-close").onclick = () => { $("diag").hidden = true; };
$("diag-clear").onclick = () => { diag.clear(); showDiagnostics(); };
$("diag-copy").onclick = async () => {
  const text = $("diag-text").textContent;
  try {
    await navigator.clipboard.writeText(text);
    $("diag-copy").textContent = "Copied";
  } catch {
    // Fallback: select it so the user can copy by hand.
    const r = document.createRange();
    r.selectNodeContents($("diag-text"));
    const sel = getSelection();
    sel.removeAllRanges();
    sel.addRange(r);
    $("diag-copy").textContent = "Selected";
  }
  setTimeout(() => { $("diag-copy").textContent = "Copy"; }, 1500);
};

// ---------- host switching ----------

function switchHost(addr) {
  ui.link?.stop();
  ui.link = null;
  ui.tree = [];
  ui.sel = null;
  ui.screens.clear();
  currentAddr = addr;
  store.set("current", addr);
  renderTree();
  renderScreen();
  const h = currentHost();
  if (!h) {
    showWelcome(true);
    setConnState("idle");
    return;
  }
  showWelcome(false);
  ui.link = new Link(h, {
    onState: (s, d) => {
      if (s !== "connecting") diag.log("conn", d && typeof d === "string" ? `${s}: ${d}` : s);
      setConnState(s, d);
      if (s === "online" && ui.sel && !ui.sel.info) ui.link.write({ t: "watch", ws: ui.sel.ws, sf: ui.sel.sf });
      renderTree();
      renderScreen();
    },
    onFrame: (m) => {
      if (m.t === "tree") onTree(m.workspaces);
      else if (m.t === "screen") onScreen(m);
      else if (m.t === "notice") onNotice(m);
      else if (m.t === "chat") onChat(m);
    },
    onNotice: (t) => showBanner(t, "err"),
  });
  ui.link.start();
}

// ---------- version ----------

let serverVersion = "";

function renderVersion() {
  const built = APP_BUILT.startsWith("__") ? "" : new Date(APP_BUILT).toLocaleString([], { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
  let text = `app ${appVersion}` + (built ? ` · ${built}` : "");
  if (serverVersion) text += `\nMac ${serverVersion}`;
  $("version-label").textContent = text;
  $("version-label").style.whiteSpace = "pre";
}

// The service worker installs a new release in the background, but this
// page keeps running the old code until it reloads. Offer that reload.
function watchForUpdates(reg) {
  const offer = () => { $("update-banner").hidden = false; };
  const track = (w) => w?.addEventListener("statechange", () => {
    if (w.state === "activated" && navigator.serviceWorker.controller) offer();
  });
  if (reg.installing) track(reg.installing);
  reg.addEventListener("updatefound", () => track(reg.installing));
  // Check for a new release when the app comes back to the foreground.
  document.addEventListener("visibilitychange", () => { if (!document.hidden) reg.update().catch(() => {}); });
}
$("update-banner").onclick = () => location.reload();

// ---------- start ----------

applyPrefs();
renderVersion();
if ("serviceWorker" in navigator && location.protocol === "https:") {
  const hadController = !!navigator.serviceWorker.controller;
  navigator.serviceWorker.register("sw.js").then((reg) => { if (hadController) watchForUpdates(reg); }).catch(() => {});
}
try {
  await bootWasm();
} catch (e) {
  $("loader-text").textContent = "Couldn't load: " + (e.message || e);
  throw e;
}
$("loader").classList.add("done");
setTimeout(() => $("loader").remove(), 300);
window.rayReady = true;
try {
  const draft = sessionStorage.getItem("ray.draft");
  if (draft) {
    sessionStorage.removeItem("ray.draft");
    inputEl.value = draft;
    autosize();
  }
} catch {}
switchHost(currentAddr);
