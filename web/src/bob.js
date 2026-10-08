// Chat view for terminals running IBM Bob Shell. Pure rendering: app.js owns
// the connection and state, and calls renderChat with the latest session.
//
// chat = {task, state, msgs, approval, error}; see internal/server/bob.go.

const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

// ---------- markdown (small, escape-first) ----------

function inline(s) {
  // s is already HTML-escaped.
  const codes = [];
  s = s.replace(/`([^`\n]+)`/g, (_, c) => `\u0000${codes.push(c) - 1}\u0000`);
  s = s
    .replace(/\*\*([^*\n]+)\*\*/g, "<strong>$1</strong>")
    .replace(/(^|[^*\w])\*([^*\n]+)\*(?=[^*\w]|$)/g, "$1<em>$2</em>")
    .replace(/\[([^\]\n]+)\]\((https?:\/\/[^)\s]+)\)/g, '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>');
  return s.replace(/\u0000(\d+)\u0000/g, (_, i) => `<code>${codes[i]}</code>`);
}

export function md(text) {
  const blocks = [];
  // Fenced code first, so nothing inside it is touched.
  let src = String(text ?? "").replace(/```[^\n]*\n?([\s\S]*?)```/g, (_, code) => `\u0001${blocks.push(code.replace(/\n$/, "")) - 1}\u0001`);
  src = esc(src);
  const out = [];
  let list = null; // "ul" | "ol"
  let para = [];
  const flushPara = () => {
    if (para.length) out.push(`<p>${inline(para.join("<br>"))}</p>`);
    para = [];
  };
  const closeList = () => {
    if (list) out.push(`</${list}>`);
    list = null;
  };
  for (const line of src.split("\n")) {
    let m;
    if ((m = line.match(/^\u0001(\d+)\u0001$/))) {
      flushPara(); closeList();
      out.push(`<pre class="code">${esc(blocks[+m[1]])}</pre>`);
    } else if ((m = line.match(/^(#{1,6})\s+(.*)$/))) {
      flushPara(); closeList();
      out.push(`<p class="h">${inline(m[2])}</p>`);
    } else if ((m = line.match(/^\s*[-*•]\s+(.*)$/)) || (m = line.match(/^\s*(\d+)[.)]\s+(.*)$/))) {
      flushPara();
      const kind = /^\s*\d/.test(line) ? "ol" : "ul";
      if (list !== kind) { closeList(); out.push(`<${kind}>`); list = kind; }
      out.push(`<li>${inline(m[m.length - 1])}</li>`);
    } else if (!line.trim()) {
      flushPara(); closeList();
    } else {
      closeList();
      para.push(line.replace(/\u0001(\d+)\u0001/g, (_, i) => `<code>${esc(blocks[+i])}</code>`));
    }
  }
  flushPara(); closeList();
  return out.join("");
}

// ---------- tools ----------

const titleCase = (s) => String(s || "tool").replace(/^mcp__[^_]+__/, "").replace(/[_-]+/g, " ").replace(/\b\w/g, (c) => c.toUpperCase());

function args(call) {
  if (!call?.args) return {};
  return typeof call.args === "string" ? safeJSON(call.args) : call.args;
}
function safeJSON(s) { try { return JSON.parse(s); } catch { return {}; } }

// The one-line gist of a tool call: its command, path or query.
function gist(call) {
  const a = args(call);
  for (const k of ["command", "path", "file_path", "filePath", "target_file", "pattern", "query", "url", "mode", "message"]) {
    if (typeof a[k] === "string" && a[k]) return a[k];
  }
  const first = Object.values(a).find((v) => typeof v === "string" && v.length < 200);
  return first || "";
}

// bob's todo list arrives as text lines like "[x] done", "[-] doing", "[ ] todo".
function todoItems(call) {
  for (const v of Object.values(args(call))) {
    const text = Array.isArray(v) ? v.map((x) => (typeof x === "string" ? x : `[${x.status === "completed" ? "x" : x.status === "in_progress" ? "-" : " "}] ${x.content || x.text || ""}`)).join("\n") : v;
    if (typeof text !== "string") continue;
    const items = text.split("\n").map((l) => l.match(/^\s*(?:[-*]\s*)?\[([ xX\-~])\]\s*(.*)$/)).filter(Boolean);
    if (items.length) return items.map((m) => ({ state: m[1] === " " ? "todo" : /[xX]/.test(m[1]) ? "done" : "doing", text: m[2] }));
  }
  return null;
}

function toolBody(call, res) {
  const a = args(call);
  const parts = [];
  const pre = (label, text) => text && parts.push(`${label ? `<div class="tl">${esc(label)}</div>` : ""}<pre>${esc(text)}</pre>`);
  if (call) {
    if (typeof a.command === "string") pre("", a.command);
    for (const k of ["diff", "content", "search", "replace", "old_str", "new_str", "text"]) {
      if (typeof a[k] === "string" && a[k]) pre(k, a[k]);
    }
    if (!parts.length && Object.keys(a).length) pre("", JSON.stringify(a, null, 2));
  }
  if (res?.output) pre(res.error ? "error" : "output", res.output);
  return parts.join("");
}

const icons = {
  ok: '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M3.5 8.5l3 3 6-7" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/></svg>',
  err: '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M4.5 4.5l7 7M11.5 4.5l-7 7" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/></svg>',
  wait: '<svg viewBox="0 0 16 16" aria-hidden="true"><circle cx="8" cy="8" r="5.5" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="M8 5v3.2l2 1.3" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>',
  run: '<span class="spin" aria-hidden="true"></span>',
};

function toolCard(call, res, chat, open) {
  const pending = chat.approval?.call?.id && call && chat.approval.call.id === call.id;
  const status = res ? (res.error ? "err" : "ok") : pending ? "wait" : chat.state === "working" ? "run" : "wait";
  const name = res?.display || titleCase(call?.name || res?.name);
  const todos = call?.name === "update_todo_list" ? todoItems(call) : null;
  const id = esc(call?.id || res?.call_id || "");
  if (todos) {
    return `<div class="tool todo ${status}"><div class="tool-head">${icons[status]}<b>Todo list</b></div><ul class="todos">${todos.map((t) => `<li class="${t.state}"><span class="box"></span>${inline(esc(t.text))}</li>`).join("")}</ul></div>`;
  }
  return `<details class="tool ${status}" data-id="${id}"${open ? " open" : ""}><summary>${icons[status]}<b>${esc(name)}</b><span class="gist">${esc(gist(call))}</span></summary><div class="tool-body">${toolBody(call, res) || '<span class="muted">No output</span>'}</div></details>`;
}

// ---------- chat ----------

const stateText = { working: "Working…", approval: "Needs your approval", idle: "Your turn", stopped: "bob isn't running" };

export function statusHTML(chat) {
  if (!chat) return "";
  if (chat.error) return `<span class="pill err">${esc(chat.error)}</span>`;
  const cost = chat.task?.cost || [...(chat.msgs || [])].reverse().find((m) => m.cost)?.cost;
  return `<span class="pill ${esc(chat.state)}">${chat.state === "working" ? icons.run : '<span class="dot"></span>'}${esc(stateText[chat.state] || chat.state || "")}</span>` +
    (cost ? `<span class="cost">$${cost.toFixed(2)}</span>` : "") +
    (chat.task?.title ? `<span class="task" title="${esc(chat.task.title)}">${esc(chat.task.title)}</span>` : "");
}

export function renderChat(el, chat) {
  if (!chat) {
    el.innerHTML = '<div class="chat-empty">Loading bob session…</div>';
    return;
  }
  // Keep tool cards the user expanded open across re-renders.
  const open = new Set([...el.querySelectorAll("details.tool[open]")].map((d) => d.dataset.id));
  const results = new Map();
  for (const m of chat.msgs || []) if (m.result?.call_id) results.set(m.result.call_id, m.result);
  const used = new Set();
  const html = [];
  for (const m of chat.msgs || []) {
    if (m.role === "user") {
      if (m.text) html.push(`<div class="msg user"><div class="bubble">${esc(m.text)}</div></div>`);
    } else if (m.role === "assistant") {
      const parts = [];
      if (m.text) parts.push(`<div class="md">${md(m.text)}</div>`);
      for (const c of m.calls || []) {
        const r = results.get(c.id);
        if (r) used.add(c.id);
        parts.push(toolCard(c, r, chat, open.has(c.id)));
      }
      if (m.stop === "user_cancelled") parts.push('<div class="note">Cancelled</div>');
      if (parts.length) html.push(`<div class="msg bob">${parts.join("")}</div>`);
    } else if (m.role === "tool") {
      // Shown inside its call's card; only orphans get their own.
      if (m.result && !used.has(m.result.call_id) && !(chat.msgs || []).some((x) => (x.calls || []).some((c) => c.id === m.result.call_id))) {
        html.push(`<div class="msg bob">${toolCard(null, m.result, chat, open.has(m.result.call_id))}</div>`);
      }
    } else if (m.text) {
      html.push(`<div class="note ${m.role === "error" ? "err" : ""}">${esc(m.text)}</div>`);
    }
  }
  if (!html.length) html.push('<div class="chat-empty">No messages yet. Say something to bob below.</div>');
  if (chat.state === "working") html.push(`<div class="typing">${icons.run}<span>bob is working…</span></div>`);
  if (chat.approval) {
    const c = chat.approval.call;
    html.push(`<div class="approval">
      <div class="approval-title">${icons.wait}<b>${esc(c ? titleCase(c.name) : "Permission needed")}</b></div>
      ${c ? `<div class="approval-gist">${esc(gist(c)) || ""}</div>` : ""}
      <div class="approval-actions">
        <button data-approve="once" class="primary">Approve</button>
        <button data-approve="always">Always for this task</button>
        <button data-approve="reject" class="danger">Reject</button>
      </div>
    </div>`);
  }
  el.innerHTML = html.join("");
}
