// Service worker: makes ray installable and start instantly (the wasm is
// ~8 MB gzipped, so it must only be downloaded once per release).
// VERSION is stamped by the build; a new deploy gets a new cache.
const VERSION = "__VERSION__";
const CACHE = "ray-" + VERSION;
const SHELL = [
  "./",
  "index.html",
  "app.js",
  "bob.js",
  "style.css",
  "wasm_exec.js",
  "tailcat-worker.js",
  "manifest.webmanifest",
  "icon.svg",
  "icon-192.png",
  "icon-512.png",
  "ray.wasm.gz",
];

self.addEventListener("install", (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener("activate", (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k.startsWith("ray-") && k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (e) => {
  const url = new URL(e.request.url);
  if (e.request.method !== "GET" || url.origin !== location.origin) return;
  e.respondWith(
    caches.open(CACHE).then(async (c) => {
      // Pairing links carry their data in the #fragment, which never
      // reaches here, so "./" always matches the cached shell.
      const hit = await c.match(e.request, { ignoreSearch: true });
      if (hit) return hit;
      const resp = await fetch(e.request);
      if (resp.ok && url.pathname.startsWith(new URL("./", location).pathname)) c.put(e.request, resp.clone());
      return resp;
    }),
  );
});

// bob notifications, pushed by the Mac (internal/server/notify.go).
self.addEventListener("push", (e) => {
  let n = {};
  try { n = e.data.json(); } catch { n = { title: e.data?.text() || "ray" }; }
  e.waitUntil(self.registration.showNotification(n.title || "ray", {
    body: n.body || "",
    tag: n.tag || undefined,
    data: { ws: n.ws, sf: n.sf },
    icon: "icon-192.png",
    badge: "icon-192.png",
  }));
});

// Tapping one opens its terminal. The target travels three ways, since
// any one can miss on iOS: in a cache the page checks as it comes up (it
// may be reloading as it resumes), in a message to a running page, and in
// the URL of a page we open.
self.addEventListener("notificationclick", (e) => {
  e.notification.close();
  const { ws, sf } = e.notification.data || {};
  e.waitUntil((async () => {
    const wins = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
    const target = { ws, sf, at: Date.now(), how: wins.length ? "focus" : "open" };
    if (ws && sf) {
      const c = await caches.open("notify-target");
      await c.put("open", new Response(JSON.stringify(target)));
    }
    if (wins.length) {
      await wins[0].focus().catch(() => {});
      wins[0].postMessage({ t: "open", target });
    } else {
      const q = ws && sf ? "?open=" + encodeURIComponent(ws) + "&sf=" + encodeURIComponent(sf) : "";
      await self.clients.openWindow("./" + q);
    }
  })());
});
