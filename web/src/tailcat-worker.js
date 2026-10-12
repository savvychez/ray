// Runs ray's Go/wasm tailcat transport (web/wasm/main_js.go) in a worker,
// off the page's main thread. After iOS has frozen the app for a while,
// the Go runtime wakes up with a backlog of timers, handshakes and relay
// reconnects; on the main thread that froze the page itself (unpainted,
// deaf to taps, unable even to reload). Here it can't, and the page can
// terminate a stuck worker outright.
//
// Protocol (all messages carry op):
//   page → worker: {op:"call", id, fn, args}   rayNewKey / rayPublicKey
//                  {op:"dial", id, opts}       rayDial; result is a conn id
//                  {op:"write", id, conn, data:ArrayBuffer}
//                  {op:"close", conn}
//   worker → page: {op:"progress", f} {op:"ready"} {op:"fatal", error}
//                  {op:"result", id, ok, value|error}
//                  {op:"data", conn, chunk:Uint8Array} {op:"eof", conn}
//                  {op:"err", conn, error}

importScripts("wasm_exec.js");

const conns = new Map();
let nextConn = 1;

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
  if ("DecompressionStream" in self) {
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

async function boot() {
  const ready = new Promise((resolve) => { self.onRayWasmReady = resolve; });
  const go = new Go();
  const { instance } = await WebAssembly.instantiateStreaming(
    fetchWasm((f) => postMessage({ op: "progress", f })),
    go.importObject,
  );
  go.run(instance);
  await ready;
}

const booted = boot();
booted.then(() => postMessage({ op: "ready" }), (e) => postMessage({ op: "fatal", error: String(e.message || e) }));

const errText = (e) => String(e?.message || e);
const reply = (id, ok, v) => postMessage(ok ? { op: "result", id, ok, value: v } : { op: "result", id, ok, error: v });

self.onmessage = async (e) => {
  const m = e.data;
  try {
    await booted;
  } catch (err) {
    if (m.id) reply(m.id, false, errText(err));
    return;
  }
  switch (m.op) {
    case "call": {
      const fn = { rayNewKey: self.rayNewKey, rayPublicKey: self.rayPublicKey }[m.fn];
      if (!fn) return reply(m.id, false, "unknown function " + m.fn);
      try {
        reply(m.id, true, fn(...(m.args || [])));
      } catch (err) {
        reply(m.id, false, errText(err));
      }
      break;
    }
    case "dial": {
      let c;
      try {
        c = await self.rayDial(m.opts);
      } catch (err) {
        return reply(m.id, false, errText(err));
      }
      const id = nextConn++;
      conns.set(id, c);
      reply(m.id, true, id);
      pump(id, c);
      break;
    }
    case "write": {
      const c = conns.get(m.conn);
      if (!c) return reply(m.id, false, "connection closed");
      try {
        await c.write(new Uint8Array(m.data));
        reply(m.id, true);
      } catch (err) {
        reply(m.id, false, errText(err));
      }
      break;
    }
    case "close": {
      const c = conns.get(m.conn);
      conns.delete(m.conn);
      try { c?.close(); } catch {}
      break;
    }
  }
};

// pump forwards a connection's reads to the page.
async function pump(id, c) {
  try {
    for (;;) {
      const chunk = await c.read();
      if (!conns.has(id)) return;
      if (chunk === null) {
        conns.delete(id);
        return postMessage({ op: "eof", conn: id });
      }
      postMessage({ op: "data", conn: id, chunk }, [chunk.buffer]);
    }
  } catch (err) {
    if (conns.delete(id)) postMessage({ op: "err", conn: id, error: errText(err) });
  }
}
